package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// VerifyOptions carries flags to Verify. Zero value not runnable.
type VerifyOptions struct {
	// BundleRoot is the path to the exporter output dir. Required.
	BundleRoot string
}

// Validate enforces required-field invariants. Called by Verify; can
// be called by CLI parsers to fail early.
func (o VerifyOptions) Validate() error {
	if o.BundleRoot == "" {
		return errors.New("BundleRoot is required")
	}
	return nil
}

// VerifyReport is the dry-diff between a bundle and the live suchi DB.
// The three slices partition every doc in the bundle:
//
//	New    — source PKs that would be imported (no matching row here)
//	Match  — source PKs that already exist AND whose compared fields agree
//	Differ — source PKs that already exist BUT some field diverges
//
// Orphan is orthogonal: source PKs present in the suchi DB but NOT
// in this bundle. Useful during shadow-window migrations to spot
// source-side deletions.
type VerifyReport struct {
	New    []int64
	Match  []int64
	Differ []DocDiff
	Orphan []int64
}

// DocDiff names the fields that disagree between the bundle and the
// live suchi row for a given legacy_id. Only field NAMES; the
// values themselves aren't returned because they might be large (title
// truncation, OCR text) and the operator wants a summary, not a dump.
type DocDiff struct {
	LegacyID int64
	Fields   []string
}

// Verify reads the bundle and diffs it against the live DB without
// touching a single row. Runs no owner resolution, opens no
// transactions, writes no blobs. Safe against production data.
func Verify(ctx context.Context, d *db.DB, log *slog.Logger, opts VerifyOptions) (*VerifyReport, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	log = log.With("component", "import.bundle.verify", "bundle", opts.BundleRoot)

	objs, err := LoadManifests(opts.BundleRoot)
	if err != nil {
		return nil, err
	}

	rep := &VerifyReport{}
	bundlePKs := map[int64]bool{}

	for _, o := range objs {
		if o.Model != ModelDocument {
			continue
		}
		var f DocumentFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode document pk=%d: %w", o.PK, err)
		}
		bundlePKs[o.PK] = true

		// Existing suchi row?
		var (
			suchiTitle string
			suchiSize  int64
		)
		err := d.Read.QueryRowContext(ctx, `
			SELECT title, original_size FROM documents WHERE legacy_id = ?
		`, o.PK).Scan(&suchiTitle, &suchiSize)
		if err != nil {
			// Not found → this doc would be imported.
			rep.New = append(rep.New, o.PK)
			continue
		}

		diff := compareDoc(opts.BundleRoot, f, suchiTitle, suchiSize)
		if len(diff) == 0 {
			rep.Match = append(rep.Match, o.PK)
		} else {
			rep.Differ = append(rep.Differ, DocDiff{LegacyID: o.PK, Fields: diff})
		}
	}

	// Orphans: suchi docs with a legacy_id that's not in this bundle.
	rows, err := d.Read.QueryContext(ctx, `
		SELECT legacy_id FROM documents
		WHERE legacy_id IS NOT NULL AND trashed_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pk int64
		if err := rows.Scan(&pk); err != nil {
			return nil, err
		}
		if !bundlePKs[pk] {
			rep.Orphan = append(rep.Orphan, pk)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	log.Info("verify.done",
		"new", len(rep.New),
		"match", len(rep.Match),
		"differ", len(rep.Differ),
		"orphan", len(rep.Orphan),
	)
	return rep, nil
}

// compareDoc returns the list of fields that disagree between the
// bundle-side view of a document and the suchi row. Field names are
// stable strings ("title", "original_size") so downstream consumers
// can key on them.
//
// Compared fields (intentionally minimal — high-signal, cheap):
//   - title              : straight string compare
//   - original_size      : file size on disk vs stored original_size
//
// Deliberately NOT compared: content (OCR text can differ trivially
// across source-tool versions without a real change), tags (name-remap
// noise would produce false positives before we build the mapping).
// Add fields here as needs surface.
func compareDoc(bundleRoot string, f DocumentFields, suchiTitle string, suchiSize int64) []string {
	var diff []string
	if f.Title != suchiTitle {
		diff = append(diff, "title")
	}
	origPath, _ := FilePaths(bundleRoot, f)
	if fi, err := os.Stat(origPath); err == nil {
		if fi.Size() != suchiSize {
			diff = append(diff, "original_size")
		}
	}
	return diff
}
