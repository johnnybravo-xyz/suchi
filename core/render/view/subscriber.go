// Subscriber wrapper: expose the Renderer as a durable-outbox handler
// keyed to the "render" job kind. Every metadata mutator enqueues one
// job in the same tx as its own write; the dispatcher fires the
// subscriber after commit, so a crash between write and enqueue can
// never leave a doc missing from the render_moves audit trail.

package view

import (
	"context"
	"database/sql"

	"github.com/johnnybravo-xyz/suchi/core/jobs"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// EnqueueMove is the helper mutator sites call inside their own tx.
// One-liner replacement for "hard-wire a Renderer everywhere" — the
// mutator just says "this doc's metadata changed" and the dispatcher
// picks it up post-commit. Safe to call multiple times per tx; the
// dispatcher deduplicates on doc_id + kind under 'pending'.
func EnqueueMove(ctx context.Context, tx *sql.Tx, docID int64) error {
	return jobs.Enqueue(ctx, tx, Kind, docID, "{}")
}

// Kind is the job kind mutators enqueue when they change any metadata
// field the storage-path template reads (title, correspondent,
// document type, JD category, tags, storage_path, archive_serial).
const Kind = "render"

// Handler is the Subscriber wrapper. `nil` Renderer → nil handler; the
// dispatcher just doesn't see the kind and mutators' enqueued rows sit
// as no-op dead letters. That's acceptable for headless test setups
// that don't wire a renderer.
type Handler struct {
	r *Renderer
}

// NewHandler returns a Subscriber for the "render" kind. Returns nil
// when r is nil so callers can pass `view.NewHandler(renderer)`
// unconditionally without a guard at the callsite.
func NewHandler(r *Renderer) *Handler {
	if r == nil {
		return nil
	}
	return &Handler{r: r}
}

// Kinds implements pluginapi.Subscriber.
func (h *Handler) Kinds() []string { return []string{Kind} }

// Handle is the Subscriber entrypoint — dispatches to Renderer.Move.
// Move is idempotent (same-path re-render is a no-op), so a job that
// gets retried after a mid-move failure just retries the move.
func (h *Handler) Handle(ctx context.Context, e pluginapi.Event) error {
	return h.r.Move(ctx, e.DocID)
}
