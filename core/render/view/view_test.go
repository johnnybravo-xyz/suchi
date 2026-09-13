package view_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
)

func TestRenderAndMovePreserveBlobContents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rendered views require Unix symlinks")
	}
	renderer, database, cas, renderDir := openRenderer(t)
	ctx := context.Background()
	original, err := cas.Put(strings.NewReader("original receipt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(ctx, `
		INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at)
		VALUES (1, 1, 1, ?, ?, 'Receipt', 1, 0, 0)
	`, original.SHA256, original.Size); err != nil {
		t.Fatal(err)
	}
	path, err := renderer.Render(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	readRendered := func(path, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(renderDir, path))
		if err != nil || string(got) != want {
			t.Fatalf("read rendered %q = %q, %v; want %q", path, got, err, want)
		}
	}
	readRendered(path, "original receipt")

	// Processing can produce an archive blob without changing the filing path.
	archive, err := cas.Put(strings.NewReader("searchable receipt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(ctx,
		`UPDATE documents SET archive_blob = ? WHERE id = 1`, archive.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Move(ctx, 1); err != nil {
		t.Fatal(err)
	}
	readRendered(path, "searchable receipt")

	if _, err := database.ExecWrite(ctx,
		`UPDATE documents SET title = 'Filed receipt' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Move(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(renderDir, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old rendered path still exists: %v", err)
	}
	var movedPath string
	if err := database.Read.QueryRowContext(ctx,
		`SELECT new_path FROM render_moves WHERE document_id = 1 ORDER BY id DESC LIMIT 1`).
		Scan(&movedPath); err != nil {
		t.Fatal(err)
	}
	readRendered(movedPath, "searchable receipt")

	for _, stale := range []bool{false, true} {
		if err := os.Remove(filepath.Join(renderDir, movedPath)); err != nil {
			t.Fatal(err)
		}
		if stale {
			if err := os.Symlink(filepath.Join(t.TempDir(), "old-data", "blobs", "sha256", archive.SHA256[:2], archive.SHA256[2:4], archive.SHA256[4:6], archive.SHA256), filepath.Join(renderDir, movedPath)); err != nil {
				t.Fatal(err)
			}
		}
		if err := renderer.Move(ctx, 1); err != nil {
			t.Fatal(err)
		}
		readRendered(movedPath, "searchable receipt")
	}
	if err := renderer.Move(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var moves int
	if err := database.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM render_moves WHERE document_id = 1`).Scan(&moves); err != nil {
		t.Fatal(err)
	}
	if moves != 3 {
		t.Fatalf("move records = %d; want initial render, archive change and path change", moves)
	}
	for hash, want := range map[string]string{
		original.SHA256: "original receipt", archive.SHA256: "searchable receipt",
	} {
		stored, err := cas.Get(hash)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(stored)
		_ = stored.Close()
		if err != nil || string(got) != want {
			t.Fatalf("blob changed during rendering: %q, %v", got, err)
		}
	}
}

func TestRenderRejectsInvalidBlobHash(t *testing.T) {
	renderer, database, _, _ := openRenderer(t)
	ctx := context.Background()
	if _, err := database.ExecWrite(ctx, `
		INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at)
		VALUES (1, 1, 1, 'bad', 0, 'Receipt', 1, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Render(ctx, 1); err == nil || !strings.Contains(err.Error(), "bad hash") {
		t.Fatalf("render invalid blob: %v; want bad hash error", err)
	}
}

func TestDefaultDateFallbackAndExplicitTemplates(t *testing.T) {
	for _, mode := range []string{"jd", "flat"} {
		t.Run(mode, func(t *testing.T) {
			_, database, cas, root := openRenderer(t)
			if _, err := database.ExecWrite(t.Context(), `UPDATE jd_systems SET taxonomy=? WHERE id=1`, mode); err != nil {
				t.Fatal(err)
			}
			renderer, err := view.New(database, cas, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			blob, err := cas.Put(strings.NewReader("immutable original"))
			if err != nil {
				t.Fatal(err)
			}
			created := time.Date(2025, 4, 3, 18, 0, 0, 0, time.UTC).Unix()
			added := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC).Unix()
			if _, err := database.ExecWrite(t.Context(), `INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,jd_category_id,created_at,added_at,updated_at) VALUES(1,1,1,?,?,'Receipt',1,?,?,0)`, blob.SHA256, blob.Size, created, added); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				created, added int64
				want           string
			}{{created, added, "2025-04-03 Receipt__1.pdf"}, {0, added, "2026-09-13 Receipt__1.pdf"}, {0, 0, "undated Receipt__1.pdf"}} {
				if _, err := database.ExecWrite(t.Context(), `UPDATE documents SET created_at=?,added_at=? WHERE id=1`, tc.created, tc.added); err != nil {
					t.Fatal(err)
				}
				if err := renderer.Move(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				var rendered string
				if err := database.Read.QueryRow(`SELECT new_path FROM render_moves ORDER BY id DESC LIMIT 1`).Scan(&rendered); err != nil {
					t.Fatal(err)
				}
				if filepath.Base(rendered) != tc.want {
					t.Fatalf("date filename=%q want %q", rendered, tc.want)
				}
			}
			if _, err := database.ExecWrite(t.Context(), `INSERT INTO storage_paths(system_id,id,name,slug,path,created_at,updated_at) VALUES(1,1,'Custom','custom','custom/{{ title }}__{{ doc_pk }}.pdf',0,0); UPDATE documents SET storage_path_id=1 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if err := renderer.Move(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "custom", "Receipt__1.pdf")); err != nil || string(data) != "immutable original" {
				t.Fatalf("explicit template changed: %s %v", data, err)
			}
		})
	}
}

func TestDocumentCannotWriteOrReconcileIndexNamespace(t *testing.T) {
	renderer, database, cas, root := openRenderer(t)
	blob, err := cas.Put(strings.NewReader("document"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, paths.IndexDirectory)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "00.00 archive.huml")
	if err := os.WriteFile(indexPath, []byte("complete index"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `INSERT INTO storage_paths(system_id,id,name,slug,path,created_at,updated_at) VALUES(1,1,'Custom','custom',?,0,0)`, paths.IndexDirectory+"/00.00 archive.huml"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,jd_category_id,storage_path_id,created_at,updated_at) VALUES(1,1,1,?,?,'Receipt',1,1,0,0)`, blob.SHA256, blob.Size); err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Render(t.Context(), 1); err == nil {
		t.Fatal("custom template overwrote index")
	}
	if err := os.Symlink(dir, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(t.Context(), `UPDATE storage_paths SET path='alias/00.00 archive.huml' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Render(t.Context(), 1); err == nil {
		t.Fatal("symlink alias overwrote index")
	}
	if _, err := database.ExecWrite(t.Context(), `INSERT INTO render_moves(document_id,prev_path,new_path,state,created_at) VALUES(1,?,'missing.pdf','pending',0)`, paths.IndexDirectory+"/00.00 archive.huml"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Reconcile(t.Context()); err == nil {
		t.Fatal("recovery must report the unsafe target")
	}
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "complete index" {
		t.Fatal("index changed through document projection")
	}
}

func openRenderer(t *testing.T) (*view.Renderer, *db.DB, *blob.CAS, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(dir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, database, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'owner@example.test', 'Owner', 'admin', 0, 0);
		INSERT INTO jd_areas(system_id, code_start, code_end, name, position)
		VALUES (1, 0, 9, 'Test', 0);
		INSERT INTO jd_categories(system_id, id, area_start, code, name, system)
		VALUES (1, 1, 0, 1, 'Inbox', 1);
	`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	renderDir := filepath.Join(dir, "rendered")
	renderer, err := view.New(database, cas, renderDir, log)
	if err != nil {
		t.Fatal(err)
	}
	return renderer, database, cas, renderDir
}
