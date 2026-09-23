// SPDX-License-Identifier: AGPL-3.0-or-later

package trash

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestPurgeRequiresRenderedLinkOwnership(t *testing.T) {
	for _, kind := range []string{
		"original", "archive", "journal new", "journal previous", "earlier same path", "restored CAS",
		"foreign CAS", "different journal path", "legacy unknown archive", "hash filename", "regular file", "directory",
		"outside parent", "redirected parent",
	} {
		t.Run(kind, func(t *testing.T) {
			service, database, cas, renderRoot := newTestService(t)
			ctx := context.Background()
			trashedAt := int64(1)
			original := putBlob(t, cas, "original")
			archive := putBlob(t, cas, "current archive")
			oldArchive := putBlob(t, cas, "previous archive")
			foreign := putBlob(t, cas, "another document")
			seedDocument(t, database, 1, 1, original, &trashedAt)
			if _, err := database.Write.ExecContext(ctx, `UPDATE documents SET archive_blob=? WHERE id=1`, archive); err != nil {
				t.Fatal(err)
			}
			relative := "Owner/document.pdf"
			previous, previousBlob, nextBlob := "", "", ""
			linkHash := original
			wantRemoved := true
			switch kind {
			case "archive":
				linkHash = archive
			case "journal new":
				linkHash, nextBlob = oldArchive, oldArchive
			case "journal previous":
				linkHash, previous, previousBlob = oldArchive, relative, oldArchive
			case "earlier same path", "different journal path":
				linkHash = oldArchive
				oldPath := relative
				if kind == "different journal path" {
					oldPath, wantRemoved = "unrelated.pdf", false
				}
				if _, err := database.Write.ExecContext(ctx, `INSERT INTO render_moves(document_id,prev_path,new_path,new_blob,state,created_at,applied_at)
					VALUES(1,'',?,?,'applied',1,1)`, oldPath, oldArchive); err != nil {
					t.Fatal(err)
				}
			case "legacy unknown archive":
				linkHash, wantRemoved = oldArchive, false
			case "foreign CAS":
				linkHash, wantRemoved = foreign, false
			case "hash filename", "regular file", "directory", "outside parent", "redirected parent":
				wantRemoved = false
			}
			if _, err := database.Write.ExecContext(ctx, `INSERT INTO render_moves(document_id,prev_path,prev_blob,new_path,new_blob,state,created_at,applied_at)
				VALUES(1,?,?,?,?,'applied',2,2)`, previous, previousBlob, relative, nextBlob); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(renderRoot, filepath.FromSlash(relative))
			parent := filepath.Dir(target)
			if kind == "outside parent" || kind == "redirected parent" {
				other := t.TempDir()
				if kind == "redirected parent" {
					other = filepath.Join(renderRoot, "other")
					if err := os.Mkdir(other, 0o750); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(other, parent); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(parent, 0o750); err != nil {
				t.Fatal(err)
			}
			link, err := cas.Path(linkHash)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "restored CAS":
				link = filepath.Join(t.TempDir(), "old-data", "blobs", "sha256", original[:2], original[2:4], original[4:6], original)
			case "hash filename":
				link = filepath.Join(t.TempDir(), original)
			}
			switch kind {
			case "regular file":
				err = os.WriteFile(target, []byte("keep this file"), 0o600)
			case "directory":
				err = os.Mkdir(target, 0o750)
			default:
				err = os.Symlink(link, target)
			}
			if err != nil {
				t.Fatal(err)
			}

			report, err := service.PurgeOne(ctx, 1, 1, &pluginapi.Principal{Kind: "user", UserID: 1}, "")
			if err != nil {
				t.Fatal(err)
			}
			if report.Purged != 1 || rowExists(t, database, "documents", 1) {
				t.Fatalf("document was not purged: %+v", report)
			}
			if wantRemoved {
				if report.RenderedFilesRemoved != 1 || report.CleanupFailures != 0 {
					t.Fatalf("owned link cleanup: %+v", report)
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned link survived: %v", err)
				}
				return
			}
			if report.RenderedFilesRemoved != 0 || report.CleanupFailures != 1 {
				t.Fatalf("foreign artifact cleanup: %+v", report)
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("foreign artifact was removed: %v", err)
			}
			if kind == "regular file" {
				content, err := os.ReadFile(target)
				if err != nil || string(content) != "keep this file" {
					t.Fatalf("foreign file changed: %q, %v", content, err)
				}
			} else if kind != "directory" {
				actual, err := os.Readlink(target)
				if err != nil || actual != link {
					t.Fatalf("foreign symlink changed: %q, %v", actual, err)
				}
			}
		})
	}
}

func TestPurgeCleansEverySupersededPendingProjection(t *testing.T) {
	service, database, cas, renderRoot := newTestService(t)
	ctx := context.Background()
	trashedAt := int64(1)
	hashes := []string{putBlob(t, cas, "original"), putBlob(t, cas, "archive A"), putBlob(t, cas, "archive B")}
	seedDocument(t, database, 1, 1, hashes[0], &trashedAt)
	if _, err := database.Write.ExecContext(ctx, `UPDATE documents SET archive_blob=? WHERE id=1`, putBlob(t, cas, "latest unpublished archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.ExecContext(ctx, `INSERT INTO render_moves(document_id,prev_path,prev_blob,new_path,new_blob,state,created_at,applied_at)
		VALUES(1,'','','original.pdf',?,'applied',1,1),
		      (1,'original.pdf',?,'archive-a.pdf',?,'pending',2,NULL),
		      (1,'archive-a.pdf',?,'archive-b.pdf',?,'pending',3,NULL)`,
		hashes[0], hashes[0], hashes[1], hashes[1], hashes[2]); err != nil {
		t.Fatal(err)
	}
	for i, relative := range []string{"original.pdf", "archive-a.pdf", "archive-b.pdf"} {
		link, err := cas.Path(hashes[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(link, filepath.Join(renderRoot, relative)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := service.PurgeOne(ctx, 1, 1, &pluginapi.Principal{Kind: "user", UserID: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 1 || report.RenderedFilesRemoved != 3 || report.CleanupFailures != 0 {
		t.Fatalf("pending projection cleanup: %+v", report)
	}
	for _, relative := range []string{"original.pdf", "archive-a.pdf", "archive-b.pdf"} {
		if _, err := os.Lstat(filepath.Join(renderRoot, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pending projection %s survived: %v", relative, err)
		}
	}
}
