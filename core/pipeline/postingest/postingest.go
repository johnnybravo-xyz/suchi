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
	"encoding/json"
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
	"github.com/suchi-dms/suchi/core/pipeline/djvu"
	"github.com/suchi-dms/suchi/core/pipeline/docsplit"
	"github.com/suchi-dms/suchi/core/pipeline/epub"
	"github.com/suchi-dms/suchi/core/pipeline/ocrmypdf"
	"github.com/suchi-dms/suchi/core/pipeline/pageanalyze"
	"github.com/suchi-dms/suchi/core/pipeline/pdfinspector"
	"github.com/suchi-dms/suchi/core/pipeline/qpdf"
	"github.com/suchi-dms/suchi/core/pipeline/tessocr"
	"github.com/suchi-dms/suchi/core/pipeline/zugferd"
	"github.com/suchi-dms/suchi/core/render/view"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
)

// postIngestPayload mirrors the api-side struct — kept as a value-type
// duplicate rather than a shared package because the shape is tiny
// and the API is the only other producer today.
type postIngestPayload struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime_type"`
}

// PostClassifyKind is the job kind the LLM classifier plugin's
// Subscriber picks up. Post-ingest enqueues one at the tail of Handle
// when enqueueClassify=true.
const PostClassifyKind = "post-classify"

// Kind is the job.kind value the outbox uses.
const Kind = "post-ingest"

// ContentLimits carries the per-format byte caps applied when writing
// documents.content. Zero-valued entries fall back to the extractor's
// package-level DefaultMaxTextBytes.
type ContentLimits struct {
	PDF  int64
	EPUB int64
	DjVu int64
}

// OCR engine selectors. "auto" prefers tessocr (~300 MB smaller image)
// when its binaries are on PATH, else falls back to ocrmypdf.
const (
	OCREngineAuto      = "auto"
	OCREngineTesseract = "tesseract"
	OCREngineOCRmyPDF  = "ocrmypdf"
)

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
	limits          ContentLimits
	ocrEngine       string // "auto" | "tesseract" | "ocrmypdf"
	scanBlank       ScanBlank
	scanSplit       ScanSplit
}

// ScanBlank carries the per-Handler configuration for blank-page
// detection + removal. Zero WhitenessThreshold falls through to
// pageanalyze's default (0.995).
type ScanBlank struct {
	Enabled            bool
	WhitenessThreshold float64
}

// ScanSplit carries the per-Handler configuration for multi-doc
// splitting on QR separator sheets. Zero Token → docsplit's default.
type ScanSplit struct {
	Enabled bool
	Token   string
	DPI     int
}

// Option configures a Handler. Zero-arg New() → sane defaults;
// caller layers overrides via With*() helpers.
type Option func(*Handler)

// WithLanguages sets the tesseract language codes for the OCR path.
// Empty or nil is a no-op (keeps the default ["eng"]).
func WithLanguages(langs []string) Option {
	return func(h *Handler) {
		if len(langs) > 0 {
			h.langs = langs
		}
	}
}

// WithRenderer wires in a rendered-view projection. Absent (nil) →
// no symlink tree is refreshed; useful for bare-metal or test setups.
func WithRenderer(r *view.Renderer) Option {
	return func(h *Handler) { h.render = r }
}

// WithLLMClassifier tells post-ingest to enqueue a post-classify job
// after content lands. Pass true only when an llm-classifier plugin
// is actually registered on the dispatcher — otherwise the job goes
// dead.
func WithLLMClassifier(enabled bool) Option {
	return func(h *Handler) { h.enqueueClassify = enabled }
}

// WithContentLimits pins the per-format extraction caps. Zero-valued
// entries fall through to each extractor's package default.
func WithContentLimits(l ContentLimits) Option {
	return func(h *Handler) { h.limits = l }
}

// WithOCREngine chooses between "auto" (default), "tesseract" and
// "ocrmypdf". Empty string is a no-op — the New() default wins.
func WithOCREngine(engine string) Option {
	return func(h *Handler) {
		if engine != "" {
			h.ocrEngine = engine
		}
	}
}

// WithScanBlank enables/tunes blank-page detection. When Enabled=true,
// post-ingest runs pageanalyze after qpdf normalize and trims blank
// pages from the working copy before pdf-inspector + OCR. The CAS
// original is never touched.
func WithScanBlank(cfg ScanBlank) Option {
	return func(h *Handler) { h.scanBlank = cfg }
}

// WithScanSplit enables multi-doc splitting on QR separator sheets.
// When Enabled=true and the input PDF has separator pages (QR pages
// carrying Token), post-ingest fans out one sibling document per
// segment, soft-deletes the parent, and returns without running the
// downstream chain on the parent.
func WithScanSplit(cfg ScanSplit) Option {
	return func(h *Handler) { h.scanSplit = cfg }
}

// New builds a Handler ready to register with a Dispatcher. The
// mandatory triple (db, cas, log) covers what every branch needs; every
// other knob is an Option. Zero options → English OCR, no renderer,
// no LLM handoff, extractor defaults, OCR engine "auto".
func New(d *db.DB, cas *blob.CAS, log *slog.Logger, opts ...Option) *Handler {
	h := &Handler{
		db:        d,
		cas:       cas,
		log:       log.With("component", "post-ingest"),
		langs:     []string{"eng"},
		ocrEngine: OCREngineAuto,
	}
	for _, o := range opts {
		o(h)
	}
	return h
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

	// DjVu path: djvutxt extracts the embedded text layer.
	if djvu.Recognized(mime) {
		res, err := djvu.Extract(ctx, bytes.NewReader(origBytes), log,
			djvu.Options{MaxTextBytes: h.limits.DjVu})
		if err != nil {
			return fmt.Errorf("djvu: %w", err)
		}
		log.Info("post-ingest.route.djvu",
			"non_blank", res.NonBlank, "skipped", res.Skipped, "truncated", res.Truncated)
		if err := h.updateDoc(ctx, e.DocID, res.Text, "", 0); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// EPUB path: pure-Go zip walker → concatenated XHTML text. No qpdf,
	// no OCR, no external binary. Metadata (title, authors) is logged
	// today; hooking it into custom fields lives with the other exotic
	// formats when they land.
	if epub.Recognized(mime) {
		res, err := epub.Extract(origBytes, log, epub.Options{MaxTextBytes: h.limits.EPUB})
		if err != nil {
			log.Warn("post-ingest.epub.error", "err", err.Error())
		}
		if res == nil {
			res = &epub.Result{Skipped: true}
		}
		log.Info("post-ingest.route.epub",
			"spine", res.SpineLen, "non_blank", res.NonBlank,
			"truncated", res.Truncated,
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

	// 1a. Multi-doc split on QR separator sheets (opt-in). Runs
	// BEFORE blank removal so page-number references stay valid.
	// When separators are found, splitAndFanOut creates one sibling
	// document per segment (each with its own post-ingest job), soft-
	// deletes the parent, and returns fanOut=true — we short-circuit
	// the downstream chain because the children carry it forward.
	if h.scanSplit.Enabled {
		fanOut, err := h.splitAndFanOut(ctx, log, e.DocID, pdfBytes)
		if err != nil {
			log.Warn("post-ingest.scan_split.error", "err", err.Error())
		}
		if fanOut {
			return nil
		}
	}

	// 1b. Blank-page removal (opt-in via config). Rasterizes each
	// page at low DPI and drops those above the whiteness threshold.
	// Operates on the working copy only — the CAS original stays
	// verbatim.
	if h.scanBlank.Enabled {
		trimmed, err := h.trimBlankPages(ctx, log, pdfBytes)
		if err != nil {
			log.Warn("post-ingest.scan_blank.error", "err", err.Error())
		} else if trimmed != nil {
			pdfBytes = trimmed
		}
	}

	// 2. pdf-inspector
	ins, err := pdfinspector.Extract(ctx, bytes.NewReader(pdfBytes), log,
		pdfinspector.Options{MaxTextBytes: h.limits.PDF})
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
		// 3b. OCR path — engine dispatch.
		c, ab, as, err := h.runOCR(ctx, log, pdfBytes)
		if err != nil {
			return err
		}
		content, archiveBlob, archiveSize = c, ab, as
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

// splitAndFanOut looks for QR separator sheets in pdfBytes and, if
// any are found, creates one sibling document per non-separator
// segment. Each child gets its own qpdf-trimmed PDF (via
// qpdf.SelectPages), its own CAS put, its own documents row (with
// split_parent_id back-pointer), and its own fresh post-ingest job.
// The parent document is soft-deleted after fanout — the CAS blob is
// untouched, so undelete restores the original if the split was
// wrong.
//
// Returns fanOut=true only when at least one child was created. When
// no separators are found (or docsplit is skipped for missing
// binary), returns fanOut=false and the caller continues with the
// original as a single doc.
func (h *Handler) splitAndFanOut(ctx context.Context, log *slog.Logger, parentID int64, pdfBytes []byte) (bool, error) {
	plan, err := docsplit.Analyze(ctx, pdfBytes, log, docsplit.Options{
		Token: h.scanSplit.Token,
		DPI:   h.scanSplit.DPI,
	})
	if err != nil {
		return false, err
	}
	if plan.Skipped || len(plan.SeparatorPages) == 0 {
		return false, nil
	}
	// A separator on every page (edge case) leaves no segments —
	// keep the original as a single doc rather than trashing it.
	if len(plan.Segments) == 0 {
		log.Warn("post-ingest.scan_split.no_segments",
			"pages", plan.TotalPages, "separators", len(plan.SeparatorPages))
		return false, nil
	}
	log.Info("post-ingest.scan_split.detected",
		"parent_id", parentID,
		"pages", plan.TotalPages,
		"separators", len(plan.SeparatorPages),
		"segments", len(plan.Segments))

	// Load enough of the parent's row to seed the children — owner,
	// title, mime, jd_category all copy through.
	parent, err := h.loadParentForSplit(ctx, parentID)
	if err != nil {
		return false, fmt.Errorf("load parent for split: %w", err)
	}

	for i, seg := range plan.Segments {
		segBytes, err := h.extractSegment(ctx, log, pdfBytes, seg)
		if err != nil {
			log.Warn("post-ingest.scan_split.extract_failed",
				"segment", i+1, "err", err.Error())
			continue
		}
		if err := h.createSplitChild(ctx, log, parent, parentID, i+1, seg, segBytes); err != nil {
			log.Warn("post-ingest.scan_split.child_failed",
				"segment", i+1, "err", err.Error())
			continue
		}
	}
	// Soft-delete the parent so the workspace only shows children.
	// CAS blob is still referenced by the trashed row, so gc leaves it.
	if err := h.softDeleteParent(ctx, parentID); err != nil {
		return true, fmt.Errorf("soft-delete parent: %w", err)
	}
	return true, nil
}

// splitParent is the projection of the parent doc row needed to seed
// its split children.
type splitParent struct {
	OwnerID      int64
	Title        string
	MIME         string
	JDCategoryID int64
}

func (h *Handler) loadParentForSplit(ctx context.Context, docID int64) (*splitParent, error) {
	p := &splitParent{}
	var mimeNull sql.NullString
	err := h.db.Read.QueryRowContext(ctx, `
		SELECT owner_id, title, COALESCE(mime_type, ''), jd_category_id
		FROM documents WHERE id = ?
	`, docID).Scan(&p.OwnerID, &p.Title, &mimeNull, &p.JDCategoryID)
	if err != nil {
		return nil, err
	}
	if mimeNull.Valid {
		p.MIME = mimeNull.String
	}
	return p, nil
}

func (h *Handler) extractSegment(ctx context.Context, log *slog.Logger, pdfBytes []byte, seg docsplit.Segment) ([]byte, error) {
	res, err := qpdf.SelectPages(ctx, pdfBytes, seg.Pages(), log, qpdf.Options{})
	if err != nil {
		return nil, err
	}
	if res.Skipped {
		return nil, fmt.Errorf("qpdf select-pages skipped: %s", res.StderrTail)
	}
	return res.Data, nil
}

// createSplitChild does the (blob put + document row + post-ingest
// job) triple in one tx so a child either fully lands or not at all.
func (h *Handler) createSplitChild(ctx context.Context, log *slog.Logger, parent *splitParent, parentID int64, index int, seg docsplit.Segment, segBytes []byte) error {
	ref, err := h.cas.Put(bytes.NewReader(segBytes))
	if err != nil {
		return fmt.Errorf("cas put: %w", err)
	}
	// Title suffix disambiguates children in list views. Trim any
	// existing " (part N/M)" suffix if we're re-splitting a doc.
	title := fmt.Sprintf("%s (part %d/%d)", parent.Title, index, seg.PageCount())
	if parent.Title == "" {
		title = fmt.Sprintf("Untitled (part %d)", index)
	}

	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at,
				split_parent_id, split_index
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, parent.OwnerID, ref.SHA256, ref.Size, title, parent.MIME,
			parent.JDCategoryID, now, now, now,
			parentID, index)
		if err != nil {
			return err
		}
		childID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(postIngestPayload{
			SHA256: ref.SHA256, Size: ref.Size, MIME: parent.MIME,
		})
		if err := jobs.Enqueue(ctx, tx, Kind, childID, string(payload)); err != nil {
			return err
		}
		log.Info("post-ingest.scan_split.child_created",
			"parent_id", parentID, "child_id", childID,
			"index", index, "pages", seg.PageCount())
		return nil
	})
}

func (h *Handler) softDeleteParent(ctx context.Context, docID int64) error {
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE documents SET trashed_at = ?, updated_at = ? WHERE id = ?
		`, time.Now().Unix(), time.Now().Unix(), docID)
		return err
	})
}

// trimBlankPages analyses pdfBytes with pageanalyze and, if any pages
// are blank, returns the qpdf-trimmed working copy. Returns (nil, nil)
// when there's nothing to trim so the caller can keep the input.
//
// Never destroys data — the CAS holds the original; this only shapes
// what feeds pdf-inspector + the OCR engine + archive_blob.
func (h *Handler) trimBlankPages(ctx context.Context, log *slog.Logger, pdfBytes []byte) ([]byte, error) {
	pa, err := pageanalyze.Analyze(ctx, pdfBytes, log, pageanalyze.Options{
		WhitenessThreshold: h.scanBlank.WhitenessThreshold,
	})
	if err != nil {
		return nil, err
	}
	if pa.Skipped {
		log.Info("post-ingest.scan_blank.skip.no_binary")
		return nil, nil
	}
	blanks := len(pa.Pages) - len(pa.NonBlank)
	if blanks == 0 {
		return nil, nil
	}
	if len(pa.NonBlank) == 0 {
		// Every page tripped the whiteness check — either a
		// pathologically empty scan or the threshold is off. Keep the
		// original so we don't produce an empty archive.
		log.Warn("post-ingest.scan_blank.all_blank",
			"pages", len(pa.Pages), "threshold", h.scanBlank.WhitenessThreshold)
		return nil, nil
	}
	log.Info("post-ingest.scan_blank.trim",
		"total", len(pa.Pages), "blanks", blanks, "keep", len(pa.NonBlank))
	res, err := qpdf.SelectPages(ctx, pdfBytes, pa.NonBlank, log, qpdf.Options{})
	if err != nil {
		return nil, err
	}
	if res.Skipped {
		return nil, nil
	}
	return res.Data, nil
}

// runOCR dispatches to the configured OCR engine. Returns (content,
// archiveBlobSHA, archiveBlobSize). archiveBlob is empty when the
// engine doesn't produce a searchable-PDF archive (tessocr) or when
// OCR was skipped.
//
// Engine selection:
//   - "tesseract": run tessocr; fall through with empty content if its
//     binaries are missing.
//   - "ocrmypdf": run ocrmypdf; skip if the binary is missing.
//   - "auto": prefer tessocr when both binaries are available, else fall
//     back to ocrmypdf. Zero-config on any image that ships either.
//
// The engine decision is logged once per invocation so operators can
// verify which path fired without turning on Debug.
func (h *Handler) runOCR(ctx context.Context, log *slog.Logger, pdfBytes []byte) (content, archiveBlob string, archiveSize int64, err error) {
	engine := h.ocrEngine
	if engine == OCREngineAuto {
		if tessocr.Available() {
			engine = OCREngineTesseract
		} else {
			engine = OCREngineOCRmyPDF
		}
	}

	switch engine {
	case OCREngineTesseract:
		res, err := tessocr.OCR(ctx, bytes.NewReader(pdfBytes), log, tessocr.Options{
			Languages:    h.langs,
			MaxTextBytes: h.limits.PDF,
		})
		if err != nil {
			return "", "", 0, fmt.Errorf("tessocr: %w", err)
		}
		if res.Skipped {
			log.Info("post-ingest.route.ocr.skipped",
				"engine", "tesseract",
				"reason", firstNonEmpty(res.StderrTail, "tessocr skipped"))
			return "", "", 0, nil
		}
		log.Info("post-ingest.route.ocr",
			"engine", "tesseract", "pages", res.Pages, "text_chars", len(res.Text))
		return res.Text, "", 0, nil

	case OCREngineOCRmyPDF:
		res, err := ocrmypdf.OCR(ctx, bytes.NewReader(pdfBytes), log, ocrmypdf.Options{
			Languages: h.langs,
		})
		if err != nil {
			return "", "", 0, fmt.Errorf("ocrmypdf: %w", err)
		}
		if res.Skipped || len(res.ArchivePDF) == 0 {
			log.Info("post-ingest.route.ocr.skipped",
				"engine", "ocrmypdf",
				"reason", firstNonEmpty(res.StderrTail, "ocrmypdf skipped"))
			return res.Text, "", 0, nil
		}
		ref, err := h.cas.Put(bytes.NewReader(res.ArchivePDF))
		if err != nil {
			return "", "", 0, fmt.Errorf("cas put archive: %w", err)
		}
		log.Info("post-ingest.route.ocr",
			"engine", "ocrmypdf", "archive_sha", ref.SHA256, "text_chars", len(res.Text))
		return res.Text, ref.SHA256, ref.Size, nil
	}
	return "", "", 0, fmt.Errorf("post-ingest: unknown OCR engine %q", engine)
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
