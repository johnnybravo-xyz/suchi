package bundle_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/importer/bundle"
	"github.com/johnnybravo-xyz/suchi/core/jd"
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
		SystemID:   1,
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
	inbox, _ := jd.InboxCategoryID(ctx, d, 1)
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
		SystemID:   1,
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

func TestFilePathsStayInsideBundle(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.pdf")
	must(t, os.WriteFile(outside, []byte("private"), 0o600))
	must(t, os.Mkdir(filepath.Join(root, "originals"), 0o700))
	must(t, os.Symlink(outside, filepath.Join(root, "originals", "linked.pdf")))

	for _, name := range []string{"../../outside.pdf", "linked.pdf"} {
		if _, _, err := bundle.FilePaths(root, bundle.DocumentFields{OriginalFilename: name}); err == nil {
			t.Errorf("FilePaths(%q) should reject a bundle escape", name)
		}
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
	d, err := db.Open(ctx, filepath.Join(dataDir, "suchi.db"))
	must(t, err)
	t.Cleanup(func() { _ = d.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	must(t, err)
	must(t, db.Migrate(ctx, d, migs, log))
	must(t, jd.EnsureTree(ctx, d, log, jd.ModeJD, 1))

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

func TestBundleResumeAndVerifyStayInTargetSystem(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := buildFakeBundle(t, dir)
	d, cas, log, owner := setupTarget(t, ctx, filepath.Join(dir, "data"))
	_, err := d.Write.ExecContext(ctx, `
		UPDATE jd_systems SET code = 'S01' WHERE id = 1;
		INSERT INTO jd_systems(id, code, name, taxonomy, created_at, updated_at) VALUES (2, 'S02', 'Second', 'jd', 0, 0);
	`)
	must(t, err)
	must(t, jd.EnsureBootstrapTree(ctx, d, log, jd.ModeJD, 2))
	opts := bundle.Options{SystemID: 1, BundleRoot: root, OwnerEmail: owner}
	_, err = bundle.Run(ctx, d, cas, log, opts)
	must(t, err)
	before, err := bundle.Verify(ctx, d, log, bundle.VerifyOptions{SystemID: 2, BundleRoot: root})
	must(t, err)
	if len(before.New) != 2 || len(before.Match) != 0 || len(before.Orphan) != 0 {
		t.Fatalf("foreign legacy IDs affected verification: %+v", before)
	}
	opts.SystemID = 2
	report, err := bundle.Run(ctx, d, cas, log, opts)
	must(t, err)
	if report.Documents != 2 || report.DocumentsSkipped != 0 {
		t.Fatalf("foreign legacy IDs affected import: %+v", report)
	}
	var sameBlobs, isolatedMetadata int
	err = d.Read.QueryRowContext(ctx, `
		SELECT SUM(a.original_blob = b.original_blob), SUM(a.correspondent_id != b.correspondent_id)
		FROM documents a JOIN documents b ON b.legacy_id = a.legacy_id AND b.system_id = 2
		WHERE a.system_id = 1
	`).Scan(&sameBlobs, &isolatedMetadata)
	must(t, err)
	if sameBlobs != 2 || isolatedMetadata != 1 {
		t.Fatalf("CAS/metadata isolation: shared=%d independent=%d", sameBlobs, isolatedMetadata)
	}
	report, err = bundle.Run(ctx, d, cas, log, opts)
	must(t, err)
	if report.DocumentsSkipped != 2 || report.Documents != 0 {
		t.Fatalf("same-system resume failed: %+v", report)
	}
	// The same legacy identity may diverge independently after import. Verify
	// includes every owner in the selected system, never a foreign match/orphan.
	_, err = d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (2, 'second@example.test', 'Second owner', 'admin', 0, 0);
		UPDATE documents SET title = 'Locally edited S01' WHERE system_id = 1 AND legacy_id = 100;
		UPDATE documents SET legacy_id = 777 WHERE system_id = 1 AND legacy_id = 101;
		UPDATE documents SET owner_id = 2 WHERE system_id = 2 AND legacy_id = 101;
	`)
	must(t, err)
	first, err := bundle.Verify(ctx, d, log, bundle.VerifyOptions{SystemID: 1, BundleRoot: root})
	must(t, err)
	if !slices.Equal(first.New, []int64{101}) || !slices.Equal(first.Orphan, []int64{777}) ||
		len(first.Match) != 0 || len(first.Differ) != 1 || first.Differ[0].LegacyID != 100 ||
		!slices.Equal(first.Differ[0].Fields, []string{"title"}) {
		t.Fatalf("S01 verification lost local differences: %+v", first)
	}
	second, err := bundle.Verify(ctx, d, log, bundle.VerifyOptions{SystemID: 2, BundleRoot: root})
	must(t, err)
	if !slices.Equal(second.Match, []int64{100, 101}) || len(second.New) != 0 || len(second.Differ) != 0 || len(second.Orphan) != 0 {
		t.Fatalf("S02 verification mixed systems or omitted another owner: %+v", second)
	}
	report, err = bundle.Run(ctx, d, cas, log, opts)
	must(t, err)
	if report.Documents != 0 || report.DocumentsSkipped != 2 {
		t.Fatalf("resume failed for existing identity owned by another user: %+v", report)
	}
	var retainedOwner int64
	must(t, d.Read.QueryRowContext(ctx, `SELECT owner_id FROM documents WHERE system_id = 2 AND legacy_id = 101`).Scan(&retainedOwner))
	if retainedOwner != 2 {
		t.Fatalf("resume reassigned an existing document: owner=%d", retainedOwner)
	}
	var firstTitle, originalHash, archiveHash string
	must(t, d.Read.QueryRowContext(ctx, `
		SELECT title, original_blob, archive_blob FROM documents WHERE system_id = 1 AND legacy_id = 100
	`).Scan(&firstTitle, &originalHash, &archiveHash))
	if firstTitle != "Locally edited S01" || originalHash == archiveHash {
		t.Fatal("foreign resume changed the original document or conflated original and OCR archive")
	}
	source, err := os.ReadFile(filepath.Join(root, "originals", "doc-100.pdf"))
	must(t, err)
	stored, err := cas.Get(originalHash)
	must(t, err)
	original, err := io.ReadAll(stored)
	must(t, err)
	must(t, stored.Close())
	if !bytes.Equal(original, source) {
		t.Fatal("bundle resume modified immutable original CAS bytes")
	}
}
