// Package rescan is the shared pipeline-rescan primitive. Callers
// pick a filter (which docs?) plus a set of current pipeline version
// constants (whose signature counts as "current?"), and Enqueue lands
// one post-ingest job per matching doc into the durable outbox.
//
// Two callers today: the `suchi rescan` CLI (distro/cmd/suchi/) and
// the `rescan_enqueue` approvals-engine handler (this package's own
// handler.go). Sharing the enqueue path is why this package exists —
// the filter surface and job-payload shape stay in one place.
//
// Design principle: this package doesn't import
// core/pipeline/postingest — postingest imports us (via the handler
// registration in main.go). Version constants come from the caller
// as an Options field, breaking the cycle without magic.

package rescan

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// PostIngestKind mirrors postingest.Kind. Duplicated as a constant
// here so this package doesn't import postingest (see package-doc).
// If postingest.Kind ever changes, this constant must move with it.
const PostIngestKind = "post-ingest"

// Options is the filter + version snapshot passed by callers.
// Every filter field is optional; the empty Options selects every
// live doc (rare — usually paired with at least one filter).
type Options struct {
	// Selection filters — combined with AND.
	Stale         string        // "ocr" | "llm" | "content" | ""
	IDs           []int64       // explicit id list; empty = no filter
	JDCategory    int64         // 0 = no filter
	Tag           string        // by name; empty = no filter
	Correspondent string        // by name; empty = no filter
	OlderThan     time.Duration // 0 = no filter
	NewerThan     time.Duration // 0 = no filter
	OnlyFailed    bool
	OnlyNoOCR     bool
	SampleSize    int // 0 = no cap; otherwise cap the matched set to N random picks
	// MinimumVersion narrows a stale-pipeline selection to documents that
	// have already completed at least this pipeline version. Automatic LLM
	// upgrade proposals set this to 1 so enabling the classifier does not
	// reinterpret never-classified documents as stale results.
	MinimumVersion int

	// Pipeline version constants at the caller's binary. Passed in
	// so this package doesn't import postingest or the LLM plugin.
	OCRVersion     int
	LLMVersion     int
	ContentVersion int
}

// Row is the projection Enqueue reads for each picked doc. Exported
// so the CLI can share the same struct for its preview output —
// otherwise there'd be two Row types and one would drift.
type Row struct {
	ID     int64
	SHA256 string
	Size   int64
	MIME   string
	// HasContent — used by the CLI's --estimate to guess whether a
	// doc still needs OCR or is content-populated already.
	HasContent bool
}

// Validate returns an error if Options carries a nonsense
// combination — currently just an unknown --stale kind. Kept as a
// separate step so callers can print a nice usage message before
// Enqueue would otherwise no-op silently.
func (o Options) Validate() error {
	switch o.Stale {
	case "", "ocr", "llm", "content":
		return nil
	}
	return fmt.Errorf("rescan: --stale must be ocr|llm|content (got %q)", o.Stale)
}

// Select fetches the filtered set of doc rows from the read pool.
// Shared with the CLI so its preview + estimate see the same set
// Enqueue is about to write.
func Select(ctx context.Context, d *db.DB, opts Options) ([]Row, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	where, args := buildFilters(opts)
	rows, err := d.Read.QueryContext(ctx, `
		SELECT d.id, COALESCE(d.original_blob, ''), COALESCE(d.original_size, 0),
		       COALESCE(d.mime_type, ''),
		       CASE WHEN COALESCE(d.content, '') = '' THEN 0 ELSE 1 END
		FROM documents d
		WHERE d.trashed_at IS NULL`+where+`
		ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var r Row
		var hasContent int
		if err := rows.Scan(&r.ID, &r.SHA256, &r.Size, &r.MIME, &hasContent); err != nil {
			return nil, err
		}
		r.HasContent = hasContent == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// SampleSize applied post-filter — the CLI's `--sample 20` and
	// the approvals-handler's "approve_sample" branch both take this
	// path. Fisher–Yates over math/rand's default source; the
	// randomness is preview convenience, not cryptography.
	if opts.SampleSize > 0 && len(out) > opts.SampleSize {
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		out = out[:opts.SampleSize]
	}
	return out, nil
}

// Enqueue selects the doc set and lands one post-ingest job per row
// in the durable outbox. Batched at 200/tx so a crash mid-run leaves
// a consistent state — jobs resume from where the last commit
// landed, matching the outbox invariants a fresh upload relies on.
//
// Returns the number of jobs enqueued.
func Enqueue(ctx context.Context, d *db.DB, opts Options) (int, error) {
	picks, err := Select(ctx, d, opts)
	if err != nil {
		return 0, err
	}
	if len(picks) == 0 {
		return 0, nil
	}
	const batchSize = 200
	enqueued := 0
	for i := 0; i < len(picks); i += batchSize {
		end := i + batchSize
		if end > len(picks) {
			end = len(picks)
		}
		batch := picks[i:end]
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			for _, r := range batch {
				payload, err := json.Marshal(map[string]any{
					"sha256":    r.SHA256,
					"size":      r.Size,
					"mime_type": r.MIME,
				})
				if err != nil {
					return err
				}
				if err := jobs.Enqueue(ctx, tx, PostIngestKind, r.ID, string(payload)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return enqueued, fmt.Errorf("enqueue batch %d: %w", i/batchSize, err)
		}
		enqueued += len(batch)
	}
	return enqueued, nil
}

// buildFilters composes the WHERE fragment. Every filter is
// optional; empty Options gives an empty fragment (the caller has
// already restricted via `trashed_at IS NULL`).
func buildFilters(opts Options) (string, []any) {
	var b strings.Builder
	var args []any

	if col, cur, ok := staleColumn(opts); ok {
		b.WriteString(" AND d." + col + " < ?")
		args = append(args, cur)
		if opts.MinimumVersion > 0 {
			b.WriteString(" AND d." + col + " >= ?")
			args = append(args, opts.MinimumVersion)
		}
	}
	if len(opts.IDs) > 0 {
		// Explicit id list — used by the SPA's rescan bulk action.
		// Combines with trashed_at IS NULL so trashed picks silently drop.
		b.WriteString(" AND d.id IN (")
		for i, id := range opts.IDs {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, id)
		}
		b.WriteString(")")
	}
	if opts.JDCategory > 0 {
		b.WriteString(" AND d.jd_category_id = ?")
		args = append(args, opts.JDCategory)
	}
	if opts.Tag != "" {
		b.WriteString(` AND EXISTS (SELECT 1 FROM document_tags dt JOIN tags t ON t.id = dt.tag_id
			WHERE dt.document_id = d.id AND t.name = ?)`)
		args = append(args, opts.Tag)
	}
	if opts.Correspondent != "" {
		b.WriteString(` AND EXISTS (SELECT 1 FROM correspondents c
			WHERE c.id = d.correspondent_id AND c.name = ?)`)
		args = append(args, opts.Correspondent)
	}
	if opts.OlderThan > 0 {
		b.WriteString(" AND d.created_at < ?")
		args = append(args, time.Now().Add(-opts.OlderThan).Unix())
	}
	if opts.NewerThan > 0 {
		b.WriteString(" AND d.created_at > ?")
		args = append(args, time.Now().Add(-opts.NewerThan).Unix())
	}
	if opts.OnlyFailed {
		b.WriteString(` AND EXISTS (SELECT 1 FROM jobs j
			WHERE j.doc_id = d.id AND j.kind = 'post-ingest' AND j.state = 'dead')`)
	}
	if opts.OnlyNoOCR {
		b.WriteString(" AND COALESCE(d.content, '') = ''")
	}
	return b.String(), args
}

// staleColumn maps a Stale filter value to (column, currentVersion,
// found?). Returns ok=false when Stale is empty.
func staleColumn(opts Options) (string, int, bool) {
	switch opts.Stale {
	case "ocr":
		return "pipeline_version_ocr", opts.OCRVersion, true
	case "content":
		return "pipeline_version_content", opts.ContentVersion, true
	case "llm":
		return "pipeline_version_llm", opts.LLMVersion, true
	}
	return "", 0, false
}

// CountStale returns how many live docs' pipeline_version_<kind> is
// behind the given current version. Used by the boot-time detector
// in detect.go — the tasks-inbox proposal only surfaces when > 0.
func CountStale(ctx context.Context, d *db.DB, kind string, current int) (int, error) {
	return countStale(ctx, d, kind, current, 0, false)
}

// CountProposalStale counts results eligible for an automatic upgrade
// proposal. Documents already represented by an unfinished post-ingest job
// are new or already need attention, not candidates for a second prompt.
// LLM version 0 means no successful classifier run, not an older result.
func CountProposalStale(ctx context.Context, d *db.DB, kind string, current int) (int, error) {
	minimum := 0
	if kind == "llm" {
		minimum = 1
	}
	return countStale(ctx, d, kind, current, minimum, true)
}

func countStale(ctx context.Context, d *db.DB, kind string, current, minimum int, proposal bool) (int, error) {
	col, _, ok := staleColumn(Options{Stale: kind})
	if !ok {
		return 0, fmt.Errorf("rescan: unknown kind %q", kind)
	}
	query := `SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL AND ` + col + ` < ?`
	args := []any{current}
	if minimum > 0 {
		query += ` AND ` + col + ` >= ?`
		args = append(args, minimum)
	}
	if proposal {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM jobs j
			 WHERE j.doc_id = documents.id
			   AND j.kind = 'post-ingest'
			   AND j.state IN ('pending', 'running', 'dead')
		)`
	}
	var n int
	err := d.Read.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}
