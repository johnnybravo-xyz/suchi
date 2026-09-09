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

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
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
		INSERT INTO documents(id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at)
		VALUES (1, 1, ?, ?, 'Receipt', 1, 0, 0)
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
			if err := os.Symlink(filepath.Join(renderDir, "old-cas-layout"), filepath.Join(renderDir, movedPath)); err != nil {
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
	if moves != 2 {
		t.Fatalf("move records = %d; want initial render and path change only", moves)
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
		INSERT INTO documents(id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at)
		VALUES (1, 1, 'bad', 0, 'Receipt', 1, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Render(ctx, 1); err == nil || !strings.Contains(err.Error(), "bad hash") {
		t.Fatalf("render invalid blob: %v; want bad hash error", err)
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
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'Test', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1);
	`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	renderDir := filepath.Join(dir, "rendered")
	renderer, err := view.New(database, cas, renderDir, "jd", log)
	if err != nil {
		t.Fatal(err)
	}
	return renderer, database, cas, renderDir
}
