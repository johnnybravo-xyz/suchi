// Package postingest owns the post-ingest job kind — the first
// dispatcher step that runs after a document row lands.
//
// The chain, in order:
//
//  1. qpdf --remove-restrictions --decrypt   (normalization)
//  2. pdf-inspector / pdftotext              (text-native decision)
//     3a. If text-native → write content into documents.content.
//     3b. If scanned → OCRmyPDF, store archive PDF in CAS, write text.
//
// Every step degrades gracefully — a missing binary or an
// unrecognized input skips that step and lets the pipeline continue.
// The design principle: ingest completes even when some tools aren't
// installed. Missing OCR just means documents.content stays empty
// until a real ocrmypdf lands.
//
// Non-PDF mime types are a no-op today. Office docs / images grow a
// converter step in a follow-up.
package postingest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/classify/rules"
	"github.com/suchi-dms/suchi/core/db"
	"github.com/suchi-dms/suchi/core/jobs"
	"github.com/suchi-dms/suchi/core/pipeline/barcode"
	"github.com/suchi-dms/suchi/core/pipeline/epub"
	"github.com/suchi-dms/suchi/core/pipeline/ocrmypdf"
	"github.com/suchi-dms/suchi/core/pipeline/pdfinspector"
	"github.com/suchi-dms/suchi/core/pipeline/qpdf"
	"github.com/suchi-dms/suchi/core/pipeline/zugferd"
	"github.com/suchi-dms/suchi/core/render/view"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
)

// PostClassifyKind is the job kind the LLM classifier plugin's
// Subscriber picks up. Post-ingest enqueues one at the tail of Handle
// when enqueueClassify=true.
const PostClassifyKind = "post-classify"

// Kind is the job.kind value the outbox uses.
const Kind = "post-ingest"

// Handler chains qpdf → pdf-inspector → ocrmypdf, updates the
// documents row with content + optional archive_blob, runs the
// rules-engine classifier, refreshes the rendered-view symlink, and
// (when enqueueClassify=true) hands off to the LLM classifier via a
// post-classify job.
type Handler struct {
	db              *db.DB
	cas             *blob.CAS
	log             *slog.Logger
	langs           []string
	render          *view.Renderer // optional — nil disables rendered-view
	enqueueClassify bool           // true when an LLM classifier is registered
}

// New builds a Handler ready to register with a Dispatcher.
//
// langs is the list of tesseract languages passed to ocrmypdf when
// OCR fires. Empty defaults to ["eng"]. render is optional; nil
// disables the rendered-view projection (bare-metal deployments or
// tests that don't care about the symlink tree).
func New(d *db.DB, cas *blob.CAS, log *slog.Logger, langs []string, r *view.Renderer, enqueueClassify bool) *Handler {
	if len(langs) == 0 {
		langs = []string{"eng"}
	}
	return &Handler{
		db:              d,
		cas:             cas,
		log:             log.With("component", "post-ingest"),
		langs:           langs,
		render:          r,
		enqueueClassify: enqueueClassify,
	}
}

// Kinds implements pluginapi.Subscriber.
func (h *Handler) Kinds() []string { return []string{Kind} }

// Handle runs the pipeline. Errors bubble up to the dispatcher's
// backoff/retry loop; a persistent error (5 attempts) parks the job
// in state=dead and shows up in /api/tasks/.
func (h *Handler) Handle(ctx context.Context, e pluginapi.Event) error {
	log := h.log.With("doc_id", e.DocID)

	origBlob, mime, err := h.loadDoc(ctx, e.DocID)
	if err != nil {
		return fmt.Errorf("load doc: %w", err)
	}

	origBytes, err := h.readBlob(origBlob)
	if err != nil {
		return fmt.Errorf("cas get %s: %w", origBlob, err)
	}

	// Image path: skip qpdf/pdf-inspector/ocrmypdf (they'd fail on
	// non-PDF input), run barcode decode against the raw bytes, and
	// hand off to rules + rendered-view + classify like a PDF would.
	// QR/DataMatrix/Aztec values land in documents.content as
	// `barcode:<value>` tokens the FTS trigger picks up.
	if barcode.Recognized(mime) {
		bcs, berr := barcode.DecodeBytes(origBytes)
		if berr != nil {
			log.Info("post-ingest.image.decode_failed", "err", berr.Error())
		}
		content := barcode.TokensFor(bcs)
		if err := h.updateDoc(ctx, e.DocID, content, "", 0); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// EPUB path: pure-Go zip walker → concatenated XHTML text. No qpdf,
	// no OCR, no external binary. Metadata (title, authors) is logged
	// today; hooking it into custom fields lives with the other exotic
	// formats when they land.
	if epub.Recognized(mime) {
		res, err := epub.Extract(origBytes, log, epub.Options{})
		if err != nil {
			log.Warn("post-ingest.epub.error", "err", err.Error())
		}
		if res == nil {
			res = &epub.Result{Skipped: true}
		}
		log.Info("post-ingest.route.epub",
			"spine", res.SpineLen, "non_blank", res.NonBlank,
			"title", res.Title, "authors", res.Authors)
		if err := h.updateDoc(ctx, e.DocID, res.Text, "", 0); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	if !strings.HasPrefix(strings.ToLower(mime), "application/pdf") {
		log.Info("post-ingest.skip.non_pdf", "mime", mime)
		return nil
	}

	// 1. qpdf normalize
	normalized, err := qpdf.Normalize(ctx, bytes.NewReader(origBytes), log, qpdf.Options{})
	if err != nil {
		return fmt.Errorf("qpdf: %w", err)
	}
	pdfBytes := normalized.Data

	// 2. pdf-inspector
	ins, err := pdfinspector.Extract(ctx, bytes.NewReader(pdfBytes), log, pdfinspector.Options{})
	if err != nil {
		return fmt.Errorf("pdf-inspector: %w", err)
	}

	var (
		content     string
		archiveBlob string
		archiveSize int64
	)

	if ins.HasText {
		// Text-native shortcut — no OCR needed.
		content = ins.Text
		log.Info("post-ingest.route.text_native", "chars", ins.NonBlank)
	} else {
		// 3b. OCR path.
		ocr, err := ocrmypdf.OCR(ctx, bytes.NewReader(pdfBytes), log, ocrmypdf.Options{
			Languages: h.langs,
		})
		if err != nil {
			return fmt.Errorf("ocrmypdf: %w", err)
		}
		content = ocr.Text
		if !ocr.Skipped && len(ocr.ArchivePDF) > 0 {
			ref, err := h.cas.Put(bytes.NewReader(ocr.ArchivePDF))
			if err != nil {
				return fmt.Errorf("cas put archive: %w", err)
			}
			archiveBlob = ref.SHA256
			archiveSize = ref.Size
			log.Info("post-ingest.route.ocr", "archive_sha", ref.SHA256, "text_chars", len(content))
		} else {
			log.Info("post-ingest.route.ocr.skipped",
				"reason", firstNonEmpty(ocr.StderrTail, "ocrmypdf skipped"))
		}
	}

	if err := h.updateDoc(ctx, e.DocID, content, archiveBlob, archiveSize); err != nil {
		return err
	}

	// ZUGFeRD / Factur-X / XRechnung: pull structured invoice data from
	// the embedded XML if present. Best-effort — every non-invoice PDF
	// short-circuits at attachment discovery.
	if inv, zerr := zugferd.ExtractBytes(ctx, pdfBytes, log, zugferd.Options{}); zerr != nil {
		log.Warn("post-ingest.zugferd.error", "err", zerr.Error())
	} else if inv != nil {
		if aerr := zugferd.Apply(ctx, h.db, e.DocID, inv); aerr != nil {
			log.Warn("post-ingest.zugferd.apply", "err", aerr.Error())
		}
	}

	return h.postContentSteps(ctx, log, e.DocID)
}

// postContentSteps runs the after-content-lands steps common to both
// PDF and image paths: rules-engine, rendered-view refresh, and the
// LLM classify handoff. Extracted so both entry paths share exactly
// one implementation.
func (h *Handler) postContentSteps(ctx context.Context, log *slog.Logger, docID int64) error {
	// Rules engine runs after content lands so title/content triggers
	// see the extracted text. Rules failure is logged, not fatal —
	// classification is best-effort; the doc is already ingested.
	if applied, err := rules.Apply(ctx, h.db, log, docID); err != nil {
		log.Warn("post-ingest.rules.error", "err", err.Error())
	} else if len(applied) > 0 {
		log.Info("post-ingest.rules.applied", "count", len(applied))
	}

	// Rendered-view projection — best-effort. A failed render logs
	// a warning; the doc row is already the source of truth.
	if h.render != nil {
		if _, err := h.render.Render(ctx, docID); err != nil {
			log.Warn("post-ingest.render.error", "err", err.Error())
		}
	}

	// LLM classification handoff: enqueue a post-classify job that
	// the llm-classifier plugin's Subscriber picks up. Only enqueue
	// when the plugin is actually registered — otherwise the job
	// would die as a "no subscriber for kind" dead-letter.
	if h.enqueueClassify {
		if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
			return jobs.Enqueue(ctx, tx, PostClassifyKind, docID, "{}")
		}); err != nil {
			log.Warn("post-ingest.enqueue_classify", "err", err.Error())
		}
	}
	return nil
}

// loadDoc reads original_blob + mime_type. The trashed_at guard means
// a race between soft-delete and post-ingest gets us "not found" and
// the retry loop eventually parks the job dead — better than doing OCR
// on a document the user already trashed.
func (h *Handler) loadDoc(ctx context.Context, id int64) (origBlob, mime string, err error) {
	var mimeNull sql.NullString
	err = h.db.Read.QueryRowContext(ctx, `
		SELECT original_blob, COALESCE(mime_type, '')
		FROM documents
		WHERE id = ? AND trashed_at IS NULL
	`, id).Scan(&origBlob, &mimeNull)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("doc %d not found or trashed", id)
	}
	if mimeNull.Valid {
		mime = mimeNull.String
	}
	return
}

// readBlob pulls a blob into memory. For Phase-2 sizes (few MB PDFs)
// this is fine; larger inputs get a streaming refactor when we hit them.
func (h *Handler) readBlob(sha string) ([]byte, error) {
	rc, err := h.cas.Get(sha)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// updateDoc writes the pipeline output back. Empty content is a valid
// state — happens when both extraction and OCR are skipped (neither
// tool installed). The FTS5 trigger picks up the content column
// change automatically.
func (h *Handler) updateDoc(ctx context.Context, id int64, content, archBlob string, archSize int64) error {
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var archBlobArg any
		var archSizeArg any
		if archBlob != "" {
			archBlobArg = archBlob
			archSizeArg = archSize
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE documents
			SET content = ?, archive_blob = ?, archive_size = ?, updated_at = ?
			WHERE id = ?
		`, content, archBlobArg, archSizeArg, time.Now().Unix(), id)
		return err
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
