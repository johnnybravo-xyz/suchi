// Package refile is the "come back and change your mind" primitive.
//
// Operators can swap Suchi Presets, edit storage-path templates, add or
// tune classifier rules — but those changes don't retroactively rearrange
// documents that were ingested under the old configuration. Refile
// closes that loop:
//
//   - re-run the deterministic rules classifier against every live doc's
//     current metadata (cheap; touches DB rows only)
//   - enqueue a render job for every live doc so the rendered-view
//     symlinks converge on the current template
//
// Refile does NOT re-run OCR or the LLM classifier — those are expensive
// and are already deterministic given the CAS blob. A future
// `--include-llm` flag can enqueue re-classify jobs for the paid path.
//
// Safe to run concurrently with normal ingest — every action goes
// through the same idempotent handlers (rules.Apply, renderer.Move)
// used by the post-ingest chain.

package refile

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/classify/rules"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
)

// Stats reports what a refile pass touched. Returned so operators can
// scale a subsequent sweep or bail early if the numbers look wrong.
type Stats struct {
	DocsScanned    int64
	RulesApplied   int64
	RenderEnqueued int64
	Errors         int64
	Elapsed        time.Duration
}

// Options controls what a single refile pass does. Zero-value runs both
// passes; the flags let a caller narrow to just one when they know
// they only touched rules OR templates.
type Options struct {
	SkipRules  bool // don't re-run the deterministic classifier
	SkipRender bool // don't enqueue render/move jobs
	// Optional owner filter: when non-zero, only refile docs owned by
	// this user. Useful for "one household member wants to reflow
	// their own tree" without touching everyone else.
	OwnerID int64
}

// All walks every live document and applies the requested passes.
// Returns statistics on what happened; errors are logged and counted,
// never fatal — a broken rule or a bad template shouldn't stall the
// whole sweep.
//
// Concurrency invariants (worth reading before you change this):
//
//   - Doc IDs are snapshotted up front, so a sweep processes exactly
//     the docs that were live at start-time. Uploads that arrive
//     mid-sweep are NOT included — but they don't need to be. Any
//     new upload rides the normal postingest chain (rules → render)
//     which already reads the current preset + current template, so
//     new docs file themselves under the new tree without help.
//   - Two concurrent refiles are safe: the render subscriber
//     deduplicates on (doc_id, kind, state='pending'), so a doc
//     never gets its symlink swapped twice. rules.Apply is
//     idempotent per rule/doc pair.
//   - Refile applies rules; it does NOT undo prior rule actions.
//     If you deleted a rule that previously added tag X, tag X
//     stays on every doc it touched. The classifier is additive
//     by design; manual cleanup is the intended path for removals.
func All(ctx context.Context, d *db.DB, log *slog.Logger, opts Options) (Stats, error) {
	log = log.With("component", "refile")
	started := time.Now()
	var s Stats

	// Load the doc ids up front so a long-running sweep doesn't hold a
	// read cursor across the write transactions each doc triggers.
	ids, err := loadDocIDs(ctx, d, opts.OwnerID)
	if err != nil {
		return s, fmt.Errorf("refile: load doc ids: %w", err)
	}
	s.DocsScanned = int64(len(ids))
	log.Info("refile.begin", "docs", s.DocsScanned,
		"skip_rules", opts.SkipRules, "skip_render", opts.SkipRender,
		"owner_id", opts.OwnerID)

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		if !opts.SkipRules {
			applied, err := rules.Apply(ctx, d, log, id)
			if err != nil {
				log.Warn("refile.rules.err", "doc_id", id, "err", err.Error())
				s.Errors++
			} else if len(applied) > 0 {
				s.RulesApplied++
			}
		}
		if !opts.SkipRender {
			// Enqueue in its own write tx — the render subscriber
			// reads the doc's fresh metadata post-commit.
			if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
				return view.EnqueueMove(ctx, tx, id)
			}); err != nil {
				log.Warn("refile.enqueue.err", "doc_id", id, "err", err.Error())
				s.Errors++
			} else {
				s.RenderEnqueued++
			}
		}
	}
	s.Elapsed = time.Since(started)
	log.Info("refile.done", "docs_scanned", s.DocsScanned,
		"rules_applied", s.RulesApplied, "render_enqueued", s.RenderEnqueued,
		"errors", s.Errors, "elapsed", s.Elapsed)
	return s, nil
}

// EnqueueRenderOnly is the smallest possible action — just push a
// render/move job for every live doc. Useful when only the
// storage-path template changed and no metadata is affected.
func EnqueueRenderOnly(ctx context.Context, d *db.DB, log *slog.Logger, disp *jobs.Dispatcher) (Stats, error) {
	s, err := All(ctx, d, log, Options{SkipRules: true})
	if err == nil && disp != nil {
		disp.Nudge()
	}
	return s, err
}

func loadDocIDs(ctx context.Context, d *db.DB, ownerID int64) ([]int64, error) {
	q := `SELECT id FROM documents WHERE trashed_at IS NULL`
	args := []any{}
	if ownerID > 0 {
		q += ` AND owner_id = ?`
		args = append(args, ownerID)
	}
	q += ` ORDER BY id`
	rows, err := d.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
