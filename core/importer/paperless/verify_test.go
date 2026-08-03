package paperless_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/suchi-dms/suchi/core/importer/paperless"
)

// TestVerifyReport exercises new/match/differ/orphan partitioning.
//
// Setup:
//  1. Import a base bundle with two docs (pk 100, 101).
//  2. Build a "next-nightly" bundle with:
//     - pk 100 unchanged → match
//     - pk 101 with a changed title → differ (title)
//     - pk 102 brand new → new
//     - pk 100+101 legacy in suchi but pk 999 fake orphan check
//  3. Run Verify and assert the partition.
func TestVerifyReport(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	baseBundle := buildFakeBundle(t, tmp) // pk 100 + 101 from paperless_test.go
	d, cas, log, ownerEmail := setupTarget(t, ctx, tmp+"/data")

	// Import the base bundle to seed suchi with 100 and 101.
	if _, err := paperless.Run(ctx, d, cas, log, paperless.Options{
		BundleRoot: baseBundle,
		OwnerEmail: ownerEmail,
	}); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	// Build a next-nightly bundle: pk 100 unchanged, pk 101 with a new
	// title, pk 102 new.
	nextRoot := tmp + "/next"
	must(t, os.MkdirAll(nextRoot+"/originals", 0o755))
	must(t, os.MkdirAll(nextRoot+"/archive", 0o755))
	// Copy pk-100's files unchanged so its size + title still match.
	origIn, err := os.ReadFile(baseBundle + "/originals/doc-100.pdf")
	must(t, err)
	must(t, os.WriteFile(nextRoot+"/originals/doc-100.pdf", origIn, 0o644))
	archIn, err := os.ReadFile(baseBundle + "/archive/doc-100.pdf")
	must(t, err)
	must(t, os.WriteFile(nextRoot+"/archive/doc-100.pdf", archIn, 0o644))
	// pk 101 gets identical bytes (size match) but a different title (diff).
	origIn, err = os.ReadFile(baseBundle + "/originals/doc-101.pdf")
	must(t, err)
	must(t, os.WriteFile(nextRoot+"/originals/doc-101.pdf", origIn, 0o644))
	// pk 102 new
	must(t, os.WriteFile(nextRoot+"/originals/doc-102.pdf",
		[]byte("%PDF-1.7\n<< new doc >>\n%%EOF\n"), 0o644))

	corPtr := int64(1)
	arch100 := "doc-100.pdf"
	mk := func(model string, pk int64, fields any) paperless.Object {
		f, err := json.Marshal(fields)
		must(t, err)
		return paperless.Object{Model: model, PK: pk, Fields: f}
	}
	manifest := paperless.Manifest{
		mk("documents.tag", 2, paperless.TagFields{Name: "utilities", Slug: "utilities"}),
		mk("documents.correspondent", 1, paperless.CorrespondentFields{Name: "BESCOM", Slug: "bescom"}),
		mk("documents.document", 100, paperless.DocumentFields{
			Title:            "Electricity bill March 2026",
			OriginalFilename: "doc-100.pdf",
			ArchiveFilename:  &arch100,
			MimeType:         "application/pdf",
			Created:          time.Now().UTC().Format(time.RFC3339),
			Correspondent:    &corPtr,
			Tags:             []int64{2},
		}),
		mk("documents.document", 101, paperless.DocumentFields{
			Title:            "Property tax 2025-26 (retitled)", // ← changed
			OriginalFilename: "doc-101.pdf",
			MimeType:         "application/pdf",
			Created:          time.Now().UTC().Format(time.RFC3339),
			Tags:             []int64{2},
		}),
		mk("documents.document", 102, paperless.DocumentFields{
			Title:            "Brand new doc",
			OriginalFilename: "doc-102.pdf",
			MimeType:         "application/pdf",
			Created:          time.Now().UTC().Format(time.RFC3339),
		}),
	}
	b, err := json.Marshal(manifest)
	must(t, err)
	must(t, os.WriteFile(nextRoot+"/manifest.json", b, 0o644))

	rep, err := paperless.Verify(ctx, d, log, paperless.VerifyOptions{BundleRoot: nextRoot})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Assertions.
	if len(rep.New) != 1 || rep.New[0] != 102 {
		t.Errorf("New = %v, want [102]", rep.New)
	}
	if len(rep.Match) != 1 || rep.Match[0] != 100 {
		t.Errorf("Match = %v, want [100]", rep.Match)
	}
	if len(rep.Differ) != 1 || rep.Differ[0].PaperlessID != 101 {
		t.Errorf("Differ = %+v, want [{101, [title]}]", rep.Differ)
	} else {
		if len(rep.Differ[0].Fields) != 1 || rep.Differ[0].Fields[0] != "title" {
			t.Errorf("Differ fields = %v, want [title]", rep.Differ[0].Fields)
		}
	}
	// No orphans: both suchi docs are in the next bundle.
	if len(rep.Orphan) != 0 {
		t.Errorf("Orphan = %v, want []", rep.Orphan)
	}
}
