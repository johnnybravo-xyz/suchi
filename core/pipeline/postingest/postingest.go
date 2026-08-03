// Package postingest owns the post-ingest job kind — the first
// dispatcher step that runs after a document row lands, before OCR
// and classification.
//
// Phase 2 ships a placeholder Subscriber that just marks the job done
// and logs. The upcoming pdf-inspector / OCRmyPDF / rules-engine
// commits swap in the real routing: sniff → text-native shortcut OR
// OCR fanout → classify.
//
// Why the placeholder ships now: registering the kind at all makes
// the dispatcher recognize post-ingest jobs. Without a subscriber, a
// post-ingest job would land in `state=dead` immediately with "no
// subscriber registered" — noisy in the outbox and misleading to
// operators.
package postingest

import (
	"context"
	"log/slog"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Kind is the job.kind value used by ingest producers (upload API,
// fs-watch, email-ingest) when they enqueue post-ingest work. Exported
// so producers don't have to hard-code the string.
const Kind = "post-ingest"

// Handler is the Subscriber. Configuring it is a no-op today — later
// commits add fields for the pdf-inspector / OCRmyPDF wiring.
type Handler struct {
	log *slog.Logger
}

// New returns a ready-to-register Subscriber. Zero-config on purpose;
// the plumbing is what matters this commit.
func New(log *slog.Logger) *Handler {
	return &Handler{log: log.With("component", "post-ingest")}
}

// Kinds implements pluginapi.Subscriber.
func (h *Handler) Kinds() []string { return []string{Kind} }

// Handle acknowledges the job and returns nil so the dispatcher marks
// it done. Once pdf-inspector lands (task #26), this method fetches
// the doc, runs qpdf + inspector, and either shortcut-writes the
// text-native content or enqueues a post-ocr job.
func (h *Handler) Handle(ctx context.Context, e pluginapi.Event) error {
	h.log.Info("post-ingest.placeholder",
		"doc_id", e.DocID,
		"msg", "TODO: pdf-inspector routing not yet wired — marking done",
	)
	return nil
}
