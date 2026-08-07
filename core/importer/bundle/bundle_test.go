package bundle_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/importer/bundle"
	"github.com/suchi-dms/suchi/core/jd"
)

// TestImportEndToEnd runs a full import against a synthetic bundle and
// verifies row counts + idempotency on re-run.
func TestImportEndToEnd(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	bundleDir := buildFakeBundle(t, tmp)

	d, cas, log, ownerEmail := setupTarget(t, ctx, filepath.Join(tmp, "data"))

	// First run: full import.
	rep, err := bundle.Run(ctx, d, cas, log, bundle.Options{
		BundleRoot: bundleDir,
		OwnerEmail: ownerEmail,
	})
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if rep.Tags != 2 {
		t.Errorf("tags = %d, want 2", rep.Tags)
	}
	if rep.Correspondents != 1 {
		t.Errorf("correspondents = %d, want 1", rep.Correspondents)
	}
	if rep.Documents != 2 {
		t.Errorf("documents = %d, want 2", rep.Documents)
	}
	if rep.Blobs < 2 {
		t.Errorf("blobs = %d, want at least 2", rep.Blobs)
	}
	if rep.Notes != 1 {
		t.Errorf("notes = %d, want 1", rep.Notes)
	}

	// The row for pk=100 must carry its legacy_id AND land in inbox.
	var (
		haveLegacy int
		inCat      int64
	)
	if err := d.Read.QueryRow(
		`SELECT COUNT(*), jd_category_id FROM documents WHERE legacy_id = 100`,
	).Scan(&haveLegacy, &inCat); err != nil {
		t.Fatalf("check legacy id: %v", err)
	}
	if haveLegacy != 1 {
		t.Errorf("legacy_id row count = %d, want 1", haveLegacy)
	}
	inbox, _ := jd.InboxCategoryID(ctx, d)
	if inCat != inbox {
		t.Errorf("imported doc landed in category %d, want inbox %d", inCat, inbox)
	}

	// FTS mirror should have picked up the content column via trigger.
	var hits int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH ?`,
		"electricity").Scan(&hits); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits != 1 {
		t.Errorf("FTS hits for 'electricity' = %d, want 1", hits)
	}

	// Second run: everything must be skipped by legacy_id.
	rep2, err := bundle.Run(ctx, d, cas, log, bundle.Options{
		BundleRoot: bundleDir,
		OwnerEmail: ownerEmail,
	})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if rep2.DocumentsSkipped != 2 {
		t.Errorf("second run documents_skipped = %d, want 2", rep2.DocumentsSkipped)
	}
	if rep2.Documents != 0 {
		t.Errorf("second run documents = %d, want 0", rep2.Documents)
	}
}

// buildFakeBundle writes a minimal but structurally-valid an existing DMS
// export bundle into tmp/bundle and returns the bundle root.
//
// Structure:
//
//	bundle/
//	  manifest.json
//	  originals/doc-100.pdf
//	  originals/doc-101.pdf
//	  archive/doc-100.pdf
func buildFakeBundle(t *testing.T, tmp string) string {
	t.Helper()
	root := filepath.Join(tmp, "bundle")
	must(t, os.MkdirAll(filepath.Join(root, "originals"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "archive"), 0o755))
	writeFile := func(p, content string) {
		must(t, os.WriteFile(filepath.Join(root, p), []byte(content), 0o644))
	}
	writeFile("originals/doc-100.pdf", "%PDF-1.7\n<< /pk 100 original >>\n%%EOF\n")
	writeFile("originals/doc-101.pdf", "%PDF-1.7\n<< /pk 101 original >>\n%%EOF\n")
	writeFile("archive/doc-100.pdf", "%PDF-1.7\n<< /pk 100 archive OCR text >>\n%%EOF\n")

	corPtr := int64(1)
	arch100 := "doc-100.pdf"

	mk := func(model string, pk int64, fields any) bundle.Object {
		f, err := json.Marshal(fields)
		must(t, err)
		return bundle.Object{Model: model, PK: pk, Fields: f}
	}

	manifest := bundle.Manifest{
		mk("documents.tag", 1, bundle.TagFields{
			Name: "tax", Slug: "tax", Color: "#a6cee3",
		}),
		mk("documents.tag", 2, bundle.TagFields{
			Name: "utilities", Slug: "utilities", Color: "#fdbf6f",
		}),
		mk("documents.correspondent", 1, bundle.CorrespondentFields{
			Name: "BESCOM", Slug: "bescom",
		}),
		mk("documents.document", 100, bundle.DocumentFields{
			Title:            "Electricity bill March 2026",
			Content:          "total due for the electricity supply period",
			MimeType:         "application/pdf",
			OriginalFilename: "doc-100.pdf",
			ArchiveFilename:  &arch100,
			Created:          time.Now().UTC().Format(time.RFC3339),
			Added:            time.Now().UTC().Format(time.RFC3339),
			Modified:         time.Now().UTC().Format(time.RFC3339),
			Correspondent:    &corPtr,
			Tags:             []int64{2},
		}),
		mk("documents.document", 101, bundle.DocumentFields{
			Title:            "Property tax 2025-26",
			Content:          "assessment for the fiscal year",
			MimeType:         "application/pdf",
			OriginalFilename: "doc-101.pdf",
			Created:          time.Now().UTC().Format(time.RFC3339),
			Tags:             []int64{1, 2},
		}),
		mk("documents.note", 1, bundle.NoteFields{
			Document: 100, Note: "auto-fetched from mail", Created: time.Now().UTC().Format(time.RFC3339),
		}),
	}
	b, err := json.Marshal(manifest)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "manifest.json"), b, 0o644))
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// buildAutoJDBundle: two docs, one tagged "tax" (heuristic → code 22),
// one tagged "cli-test" (no rule → inbox).
func buildAutoJDBundle(t *testing.T, tmp string) string {
	t.Helper()
	root := tmp + "/bundle-auto"
	must(t, os.MkdirAll(root+"/originals", 0o755))
	writeFile := func(p, content string) {
		must(t, os.WriteFile(root+"/"+p, []byte(content), 0o644))
	}
	writeFile("originals/tax.pdf", "%PDF-1.7\ntax\n%%EOF\n")
	writeFile("originals/misc.pdf", "%PDF-1.7\nmisc\n%%EOF\n")

	mk := func(model string, pk int64, fields any) bundle.Object {
		f, err := json.Marshal(fields)
		must(t, err)
		return bundle.Object{Model: model, PK: pk, Fields: f}
	}
	manifest := bundle.Manifest{
		mk("documents.tag", 1, bundle.TagFields{Name: "tax", Slug: "tax", Color: "#000"}),
		mk("documents.tag", 2, bundle.TagFields{Name: "cli-test", Slug: "cli-test", Color: "#000"}),
		mk("documents.document", 200, bundle.DocumentFields{
			Title:            "ITR 2025-26",
			OriginalFilename: "tax.pdf",
			MimeType:         "application/pdf",
			Created:          time.Now().UTC().Format(time.RFC3339),
			Tags:             []int64{1},
		}),
		mk("documents.document", 201, bundle.DocumentFields{
			Title:            "Random note",
			OriginalFilename: "misc.pdf",
			MimeType:         "application/pdf",
			Created:          time.Now().UTC().Format(time.RFC3339),
			Tags:             []int64{2},
		}),
	}
	b, err := json.Marshal(manifest)
	must(t, err)
	must(t, os.WriteFile(root+"/manifest.json", b, 0o644))
	return root
}

// setupTarget: opens an in-tmpdir DB + CAS, seeds an admin user, returns
// everything plus the admin email so the importer can resolve owner.
func setupTarget(t *testing.T, ctx context.Context, dataDir string) (*db.DB, *blob.CAS, *slog.Logger, string) {
	t.Helper()
	must(t, os.MkdirAll(dataDir, 0o755))
	d, err := db.Open(ctx, filepath.Join(dataDir, "dms.db"))
	must(t, err)
	t.Cleanup(func() { _ = d.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	must(t, err)
	must(t, db.Migrate(ctx, d, migs, log))
	must(t, jd.EnsureTree(ctx, d, log, jd.ModeJD))

	// Seed admin.
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES ('admin@example.com', 'admin', 'admin', ?, ?)
		`, time.Now().Unix(), time.Now().Unix())
		return err
	})
	must(t, err)

	cas, err := blob.New(dataDir)
	must(t, err)

	return d, cas, log, "admin@example.com"
}
