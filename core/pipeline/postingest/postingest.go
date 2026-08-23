// Package postingest runs extraction, classification, and rendering after a
// document lands. Optional external tools degrade to storing the original.
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
	"os"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/db"
	ingestmeta "github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/anydoc"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/barcode"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/djvu"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/docsplit"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/eml"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/heic"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/imgpdf"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/msg"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/ocrmypdf"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/pageanalyze"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/pdfinspector"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/preconsume"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/qpdf"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/tessocr"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/thumb"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/zugferd"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/slug"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// postIngestPayload mirrors the api-side struct — kept as a value-type
// duplicate rather than a shared package because the shape is tiny
// and the API is the only other producer today.
type postIngestPayload struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime_type"`

	// Consumption-trigger context. Producers populate whichever of
	// these they have; automations.ApplyOnConsumption filters against
	// them. Zero values disable the corresponding filter.
	Filename   string `json:"filename,omitempty"`     // upload multipart / basename of source path
	SourcePath string `json:"source_path,omitempty"`  // absolute path when the doc came from fs-watch
	MailRuleID int64  `json:"mail_rule_id,omitempty"` // set by mail-intake plugins that own a rule id
}

// PostClassifyKind is the job kind the LLM classifier plugin's Subscriber
// picks up. Post-ingest enqueues one at the tail of Handle when the live
// classifier state reports enabled.
const PostClassifyKind = "post-classify"

// Kind is the job.kind value the outbox uses.
const Kind = "post-ingest"

// Pipeline versions. Written on every documents.content update so
// `suchi rescan --stale <kind>` can pick out docs that lag the
// current binary's signature. Bump the relevant constant here when
// a pipeline change would produce a materially different output.
//
// PipelineVersionContent — the qpdf → pdftotext / OCR chain. Bump
//
//	when the OCR engine changes, when a new content extractor lands,
//	or when the archive-blob renderer's semantics shift.
//
// PipelineVersionOCR     — narrower: OCR-specific. Bump when swapping
//
//	OCR engine or tesseract data.
//
// Zero (the schema default on ADD COLUMN) means "never processed by
// this pipeline" — a fresh row before its first post-ingest tick.
const (
	PipelineVersionContent = 2
	PipelineVersionOCR     = 1
)

// ContentLimits carries the per-format byte caps applied when writing
// documents.content. Zero-valued entries fall back to the extractor's
// package-level DefaultMaxTextBytes.
type ContentLimits struct {
	PDF    int64
	AnyDoc int64
	DjVu   int64
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
// automations, refreshes the rendered-view symlink, and
// (when classifyEnabled returns true) hands off to the LLM classifier via a
// post-classify job.
type Handler struct {
	db              *db.DB
	cas             *blob.CAS
	log             *slog.Logger
	langs           []string
	langChain       *lang.Chain     // optional — nil = language detection is a no-op
	render          *view.Renderer  // optional — nil disables rendered-view
	classifyEnabled func() bool     // optional live classifier state
	languageState   func() []string // optional live OCR-language state
	limits          ContentLimits
	ocrEngine       string // "auto" | "tesseract" | "ocrmypdf"
	scanBlank       ScanBlank
	scanSplit       ScanSplit
	decrypt         Decrypt
	preConsume      string // path to optional user script; empty → skip
	convertMSG      func(context.Context, io.Reader, *slog.Logger, msg.Options) (*msg.Result, error)
}

// Decrypt carries the per-Handler configuration for password-protected
// document handling. Zero-value = decrypt attempts still happen with
// the empty password (the common owner-restrictions case) but no
// operator-supplied candidates are consulted.
type Decrypt struct {
	// Key is the AEAD key used to open sealed passwords from the
	// decryption_passwords table. When nil, learned passwords are
	// skipped and only PasswordsFile candidates are tried.
	Key *suchicrypto.AEADKey
	// PasswordsFile is an optional path to a newline-separated list
	// of candidate passwords. Blank lines and lines starting with '#'
	// are skipped. Loaded lazily per-ingest so operators can update
	// the file without restarting suchi.
	PasswordsFile string
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

// WithLanguageState supplies the OCR languages at job execution time. It is
// used by the setup preferences reloader so future documents pick up a saved
// language change immediately.
func WithLanguageState(languages func() []string) Option {
	return func(h *Handler) { h.languageState = languages }
}

// WithRenderer wires in a rendered-view projection. Absent (nil) →
// no symlink tree is refreshed; useful for bare-metal or test setups.
func WithRenderer(r *view.Renderer) Option {
	return func(h *Handler) { h.render = r }
}

// WithLanguageChain wires in a language-detection chain (see
// core/lang). Detectors run at post-content time and stamp the
// dominant language onto documents.languages when confidence
// clears the built-in threshold. A nil chain (or an empty one)
// makes the detection step a no-op — the LLM classifier plugin
// can still write documents.languages on its own if enabled.
func WithLanguageChain(c *lang.Chain) Option {
	return func(h *Handler) { h.langChain = c }
}

// WithLLMClassifier tells post-ingest to enqueue a post-classify job
// after content lands. Pass true only when an llm-classifier plugin
// is actually registered on the dispatcher — otherwise the job goes
// dead.
func WithLLMClassifier(enabled bool) Option {
	return func(h *Handler) { h.classifyEnabled = func() bool { return enabled } }
}

// WithLLMClassifierState wires a live enabled check. Unlike the static option,
// this lets the setup API activate or disable classification without replacing
// the post-ingest handler.
func WithLLMClassifierState(enabled func() bool) Option {
	return func(h *Handler) { h.classifyEnabled = enabled }
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

// WithPreConsume enables the optional operator-defined pre-consume
// script. Empty path disables the feature entirely. When set,
// post-ingest hands the doc's bytes to the script BEFORE any built-in
// format-specific logic runs; the script can rewrite bytes and/or
// emit tags/custom-fields via a stdout JSON envelope.
func WithPreConsume(path string) Option {
	return func(h *Handler) { h.preConsume = path }
}

// WithDecrypt wires in the AEAD key + passwords-file location that
// post-ingest uses when a PDF returns qpdf.NeedsPassword. Absent → the
// empty-password path still runs (owner-restrictions unlock) but no
// candidate passwords are tried, and encrypted-with-real-password
// PDFs land in encryption_state='encrypted' with the operator asked
// to supply the password via the API/UI.
func WithDecrypt(cfg Decrypt) Option {
	return func(h *Handler) { h.decrypt = cfg }
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
		db:         d,
		cas:        cas,
		log:        log.With("component", "post-ingest"),
		langs:      []string{"eng"},
		ocrEngine:  OCREngineAuto,
		convertMSG: msg.Convert,
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

	// Consumption-trigger automations. Fire before any content
	// processing so filters that key on filename/source_path/mail_rule
	// can tag or route the doc up-front.
	consCtx := consumptionContextFromPayload(e.Payload)
	if err := automations.ApplyOnConsumption(ctx, h.db, log, e.DocID, consCtx); err != nil {
		return fmt.Errorf("consumption automations: %w", err)
	}

	origBlob, mime, ownerEmail, err := h.loadDoc(ctx, e.DocID)
	if err != nil {
		return fmt.Errorf("load doc: %w", err)
	}

	origBytes, err := h.readBlob(origBlob)
	if err != nil {
		return fmt.Errorf("cas get %s: %w", origBlob, err)
	}

	// Optional operator pre-consume hook. Runs BEFORE any built-in
	// format-specific logic — the script sees the raw bytes and can
	// rewrite them (SUCHI_OUTPUT) and/or emit tags + custom-fields
	// via a stdout JSON envelope. See docs/preconsume.mdx.
	if h.preConsume != "" {
		pc, err := preconsume.Run(ctx, origBytes, preconsume.Document{
			ID:         e.DocID,
			MIME:       mime,
			Filename:   consCtx.Filename,
			OwnerEmail: ownerEmail,
		}, log,
			preconsume.Options{Script: h.preConsume})
		if err != nil {
			log.Warn("post-ingest.preconsume.error", "err", err.Error())
		} else if !pc.Skipped {
			if len(pc.WorkingBytes) > 0 {
				log.Info("post-ingest.preconsume.body_rewritten",
					"input_bytes", len(origBytes), "output_bytes", len(pc.WorkingBytes))
				origBytes = pc.WorkingBytes
			}
			if len(pc.Tags) > 0 || len(pc.CustomFields) > 0 {
				if err := h.applyPreConsumeMetadata(ctx, e.DocID, pc.Tags, pc.CustomFields); err != nil {
					log.Warn("post-ingest.preconsume.apply_metadata", "err", err.Error())
				}
			}
		}
	}

	// Outlook .msg path: convert to RFC 822 via msgconvert, then fall
	// through to the same handleEmail() as .eml so Message-Id dedup,
	// attachment fanout, and correspondent inheritance work identically.
	// When msgconvert isn't installed, Skipped=true and
	// the doc stays as an opaque blob — same fallback shape as djvu.
	if msg.Recognized(mime) {
		res, mErr := h.convertMSG(ctx, bytes.NewReader(origBytes), log, msg.Options{})
		if mErr != nil {
			return fmt.Errorf("msg: %w", mErr)
		}
		if res.Skipped || len(res.EML) == 0 {
			log.Info("post-ingest.route.msg.skipped", "reason", res.StderrTail)
			if err := h.updateDoc(ctx, e.DocID, "", "", 0); err != nil {
				return err
			}
			return h.postContentSteps(ctx, log, e.DocID)
		}
		log.Info("post-ingest.route.msg", "eml_bytes", len(res.EML),
			"took", res.Duration.String())
		// Route the converted working bytes through the EML parser without
		// changing the source MIME. original_blob still contains CFB bytes.
		mime = "message/rfc822"
		origBytes = res.EML
		// Fall through to the shared EML path.
	}

	// Email path: parse the .eml, set title/correspondent/date on the
	// parent doc, fan out one child doc per attachment. See
	// core/pipeline/eml/ for the parser + docs/formats.mdx#email.
	if eml.Recognized(mime) {
		filesOnly := emailFilesOnlyFromPayload(e.Payload)
		retired, err := h.handleEmail(ctx, log, e.DocID, origBytes, filesOnly)
		if err != nil {
			return fmt.Errorf("email: %w", err)
		}
		if retired {
			// Duplicate and files-only source rows are staging documents.
			// Skip render/classify after they have been removed.
			return nil
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// HEIC / HEIF path: iPhones photograph documents in this format by
	// default. ImageMagick converts to a single-page PDF; that PDF then
	// flows through the standard OCR engine so a photograph of a
	// receipt becomes full-text searchable. Original HEIC bytes stay
	// in the CAS; archive_blob holds the converted PDF and has an embedded
	// text layer only when OCRmyPDF produced it. This route must precede
	// the generic barcode route, which accepts every image type.
	if heic.Recognized(mime) {
		res, herr := heic.Convert(ctx, bytes.NewReader(origBytes), log, heic.Options{})
		if herr != nil {
			return fmt.Errorf("heic: %w", herr)
		}
		if res.Skipped || len(res.PDF) == 0 {
			log.Info("post-ingest.route.heic.skipped", "reason", res.StderrTail)
			if err := h.updateDoc(ctx, e.DocID, "", "", 0); err != nil {
				return err
			}
			return h.postContentSteps(ctx, log, e.DocID)
		}
		log.Info("post-ingest.route.heic", "pdf_bytes", len(res.PDF), "took", res.Duration.String())

		// Keep the converted PDF for preview when the OCR engine does not
		// produce its own archive.
		content, archiveBlob, archiveSize, err := h.runOCR(ctx, log, res.PDF)
		if err != nil {
			return fmt.Errorf("heic.ocr: %w", err)
		}
		if archiveBlob == "" {
			ref, cerr := h.cas.Put(bytes.NewReader(res.PDF))
			if cerr != nil {
				return fmt.Errorf("heic.cas: %w", cerr)
			}
			archiveBlob = ref.SHA256
			archiveSize = ref.Size
			if strings.TrimSpace(content) == "" {
				log.Warn("post-ingest.route.heic.archive_no_ocr",
					"doc_id", e.DocID,
					"reason", "no OCR text was extracted; archive is a plain PDF wrapper",
					"hint", "install tesseract or ocrmypdf, then reingest")
			} else {
				log.Debug("post-ingest.route.heic.archive_plain", "doc_id", e.DocID,
					"reason", "OCR engine extracted text without rewriting the PDF")
			}
		}
		if err := h.updateDoc(ctx, e.DocID, content, archiveBlob, archiveSize); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// Raster image path: wrap to a single-page PDF, run OCR, and merge
	// with barcode tokens. Same "wrap → OCR" pattern as HEIC — standard
	// (tessocr) and full (ocrmypdf) both work; the archive PDF stays
	// as the preview when the OCR engine is absent. When magick is
	// missing entirely, falls through to barcode-only content, which
	// matches the pre-change behavior for these MIME types.
	if imgpdf.Recognized(mime) {
		bcs, berr := barcode.DecodeBytes(origBytes)
		if berr != nil {
			log.Info("post-ingest.image.decode_failed", "err", berr.Error())
		}
		barcodeTokens := barcode.TokensFor(bcs)

		pdfRes, perr := imgpdf.Convert(ctx, bytes.NewReader(origBytes), log,
			imgpdf.Options{Ext: imgpdf.ExtFromMIME(mime)})
		if perr != nil {
			return fmt.Errorf("imgpdf: %w", perr)
		}
		if pdfRes.Skipped || len(pdfRes.PDF) == 0 {
			log.Info("post-ingest.route.image.imgpdf_skipped",
				"mime", mime, "reason", pdfRes.StderrTail)
			if err := h.updateDoc(ctx, e.DocID, barcodeTokens, "", 0); err != nil {
				return err
			}
			return h.postContentSteps(ctx, log, e.DocID)
		}
		log.Info("post-ingest.route.image.wrapped",
			"mime", mime, "pdf_bytes", len(pdfRes.PDF),
			"took", pdfRes.Duration.String())

		ocrContent, archiveBlob, archiveSize, err := h.runOCR(ctx, log, pdfRes.PDF)
		if err != nil {
			return fmt.Errorf("image.ocr: %w", err)
		}
		if archiveBlob == "" {
			ref, cerr := h.cas.Put(bytes.NewReader(pdfRes.PDF))
			if cerr != nil {
				return fmt.Errorf("image.cas: %w", cerr)
			}
			archiveBlob = ref.SHA256
			archiveSize = ref.Size
			if strings.TrimSpace(ocrContent) == "" {
				log.Warn("post-ingest.route.image.archive_no_ocr",
					"mime", mime, "doc_id", e.DocID,
					"reason", "no OCR text was extracted; archive is a plain PDF wrapper",
					"hint", "install tesseract or ocrmypdf, then reingest")
			} else {
				log.Debug("post-ingest.route.image.archive_plain",
					"mime", mime, "doc_id", e.DocID,
					"reason", "OCR engine extracted text without rewriting the PDF")
			}
		}
		content := ocrContent
		if barcodeTokens != "" {
			if content != "" {
				content += "\n"
			}
			content += barcodeTokens
		}
		if err := h.updateDoc(ctx, e.DocID, content, archiveBlob, archiveSize); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// Fallback image path — any image/* type imgpdf doesn't handle
	// (SVG, x-icon, etc.) still gets a barcode decode pass so QR
	// values land in content. No OCR here — these formats either
	// don't rasterize well (SVG) or don't come with printable text.
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

	// EPUB and office documents route through anydoc. A missing binary is a
	// soft skip so the original can be reprocessed after installation.
	if anydoc.Recognized(mime) {
		res, err := anydoc.Extract(ctx, bytes.NewReader(origBytes), log,
			anydoc.Options{
				MaxTextBytes: h.limits.AnyDoc,
				Ext:          anydoc.ExtFromMIME(mime),
			})
		if err != nil {
			return fmt.Errorf("anydoc: %w", err)
		}
		log.Info("post-ingest.route.anydoc",
			"mime", mime, "non_blank", res.NonBlank,
			"skipped", res.Skipped, "truncated", res.Truncated)
		if err := h.updateDoc(ctx, e.DocID, res.Text, "", 0); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// Text sources — plain text, markdown, HTML. The blob IS the content;
	// no external tool is needed. Populating documents.content here means
	// FTS, list snippets, and similar-docs all work for these files
	// instead of matching on titles alone.
	if isTextMIME(mime) {
		const cap = 1 << 20 // 1 MiB — generous for markdown/html; caps runaway logs
		text := string(origBytes)
		truncated := false
		if len(text) > cap {
			text = text[:cap]
			truncated = true
		}
		log.Info("post-ingest.route.text",
			"mime", mime, "bytes", len(origBytes), "truncated", truncated)
		if err := h.updateDoc(ctx, e.DocID, text, "", 0); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	if !strings.HasPrefix(strings.ToLower(mime), "application/pdf") {
		log.Info("post-ingest.skip.non_pdf", "mime", mime)
		if err := h.updateDoc(ctx, e.DocID, "", "", 0); err != nil {
			return err
		}
		return h.postContentSteps(ctx, log, e.DocID)
	}

	// 1. qpdf normalize (with password candidates). Gathers candidates
	// from the operator's passwords file + the learned-passwords table
	// scoped to this doc's owner. First success wins; if all attempts
	// fail with a password error, the doc lands in state='encrypted'
	// and post-ingest bails out until the operator supplies one via
	// POST /api/documents/{id}/decrypt.
	candidates, pwdSources, err := h.gatherDecryptCandidates(ctx, log, e.DocID)
	if err != nil {
		log.Warn("post-ingest.decrypt.candidates_load_failed", "err", err.Error())
	}
	normalized, err := qpdf.Normalize(ctx, bytes.NewReader(origBytes), log,
		qpdf.Options{Passwords: candidates})
	if err != nil {
		return fmt.Errorf("qpdf: %w", err)
	}
	if normalized.NeedsPassword {
		log.Warn("post-ingest.decrypt.needs_password",
			"doc_id", e.DocID, "tried_candidates", len(candidates))
		return h.markEncrypted(ctx, e.DocID, normalized.StderrTail)
	}
	pdfBytes := normalized.Data
	// If a candidate password worked, promote the doc to decrypted
	// state, snapshot the decrypted bytes into the CAS as a second
	// blob (original is preserved verbatim), and bump last_used_at on
	// the winning learned-password row so hot passwords stay hot.
	//
	// The !Skipped guard is important: qpdf.Normalize returns
	// Skipped=true (with Data == raw input bytes) when the qpdf binary
	// is missing on PATH. Without the guard, we'd record those
	// still-encrypted bytes as `decrypted_blob` and stamp the row
	// 'decrypted' — the serving path would then hand the browser
	// ciphertext and the PDF viewer would prompt every fetch.
	if !normalized.Skipped && normalized.PasswordIndex >= 0 {
		if err := h.recordDecrypted(ctx, log, e.DocID, pdfBytes, pwdSources, normalized.PasswordIndex); err != nil {
			log.Warn("post-ingest.decrypt.record_failed", "err", err.Error())
		}
	}

	// 1a. Multi-doc split on QR separator sheets (opt-in). Runs
	// BEFORE blank removal so page-number references stay valid.
	// When separators are found, splitAndFanOut creates one sibling
	// document per segment (each with its own post-ingest job), soft-
	// deletes the parent, and returns fanOut=true — we short-circuit
	// the downstream chain because the children carry it forward.
	if h.scanSplit.Enabled {
		fanOut, err := h.splitAndFanOut(ctx, log, e.DocID, pdfBytes)
		if err != nil {
			return fmt.Errorf("scan split: %w", err)
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
	return h.fanOutSegments(ctx, log, parentID, pdfBytes, plan.Segments)
}

func (h *Handler) fanOutSegments(ctx context.Context, log *slog.Logger, parentID int64, pdfBytes []byte, segments []docsplit.Segment) (bool, error) {
	// Load enough of the parent's row to seed the children — owner,
	// title, mime, jd_category all copy through.
	parent, err := h.loadParentForSplit(ctx, parentID)
	if err != nil {
		return false, fmt.Errorf("load parent for split: %w", err)
	}

	for i, seg := range segments {
		exists, err := h.splitChildExists(ctx, parentID, i+1)
		if err != nil {
			return false, fmt.Errorf("check segment %d: %w", i+1, err)
		}
		if exists {
			continue
		}
		segBytes, err := h.extractSegment(ctx, log, pdfBytes, seg)
		if err != nil {
			return false, fmt.Errorf("extract segment %d: %w", i+1, err)
		}
		if err := h.createSplitChild(ctx, log, parent, parentID, i+1, len(segments), seg, segBytes); err != nil {
			return false, fmt.Errorf("create segment %d: %w", i+1, err)
		}
	}
	// Soft-delete the parent so the workspace only shows children.
	// CAS blob is still referenced by the trashed row, so gc leaves it.
	if err := h.softDeleteParent(ctx, parentID); err != nil {
		return false, fmt.Errorf("soft-delete parent: %w", err)
	}
	return true, nil
}

func (h *Handler) splitChildExists(ctx context.Context, parentID int64, index int) (bool, error) {
	var exists bool
	err := h.db.Read.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM documents WHERE split_parent_id = ? AND split_index = ?
		)
	`, parentID, index).Scan(&exists)
	return exists, err
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

// createSplitChild writes the document row and post-ingest job atomically.
// The preceding CAS put may leave an unreferenced blob on failure; normal GC
// reclaims it.
func (h *Handler) createSplitChild(ctx context.Context, log *slog.Logger, parent *splitParent, parentID int64, index, total int, seg docsplit.Segment, segBytes []byte) error {
	ref, err := h.cas.Put(bytes.NewReader(segBytes))
	if err != nil {
		return fmt.Errorf("cas put: %w", err)
	}
	// Title suffix disambiguates children in list views.
	title := fmt.Sprintf("%s (part %d/%d)", parent.Title, index, total)
	if parent.Title == "" {
		title = fmt.Sprintf("Untitled (part %d/%d)", index, total)
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
		if err := ingestmeta.CopySources(ctx, tx, parentID, childID); err != nil {
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

// deleteEmailStagingParent removes an email row that only existed long enough
// to deduplicate or fan out attachments. User Trash is reserved for documents
// a user or automation can restore; files-only source messages are neither.
func (h *Handler) deleteEmailStagingParent(ctx context.Context, docID int64) error {
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, docID)
		return err
	})
}

// applyPreConsumeMetadata writes the tags + custom_fields produced by
// the pre-consume script. All-or-nothing per doc: one tx wraps every
// insert. Best-effort per row inside the tx — an unknown tag/field
// logs a Warn but doesn't fail the whole application.
func (h *Handler) applyPreConsumeMetadata(ctx context.Context, docID int64, tags []string, fields map[string]any) error {
	if len(tags) == 0 && len(fields) == 0 {
		return nil
	}
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		for _, name := range tags {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			// Upsert the tag by slugified name (matches automation
			// convention). Then attach.
			sl := slug.Make(name)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tags(name, slug, created_at, updated_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(name) DO UPDATE SET updated_at = excluded.updated_at
			`, name, sl, now, now); err != nil {
				return err
			}
			var tagID int64
			if err := tx.QueryRowContext(ctx,
				`SELECT id FROM tags WHERE name = ?`, name).Scan(&tagID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
				docID, tagID); err != nil {
				return err
			}
		}
		for name, raw := range fields {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			var (
				fieldID  int64
				dataType string
				extra    string
			)
			err := tx.QueryRowContext(ctx,
				`SELECT id, data_type, extra_data FROM custom_fields WHERE name = ?`, name).Scan(&fieldID, &dataType, &extra)
			if err != nil {
				h.log.Warn("post-ingest.preconsume.unknown_field", "name", name)
				continue
			}
			handler := customfield.Lookup(dataType)
			typed, err := handler.Validate(json.RawMessage(extra), raw)
			if err != nil {
				h.log.Warn("post-ingest.preconsume.bad_value",
					"field", name, "err", err.Error())
				continue
			}
			if err := handler.Write(ctx, tx, docID, fieldID, typed); err != nil {
				return err
			}
		}
		return nil
	})
}

// handleEmail parses an RFC-822 message, updates the parent doc's
// title / content / correspondent / created_at from the headers, and
// creates one child document per attachment. Each attachment gets its
// own CAS put + post-ingest job so the downstream chain (OCR,
// ZUGFeRD, automations, render) treats it like any other upload, while the
// parent's search index carries the email body text.
//
// Owner + jd_category for children inherit from the parent. Children
// point back via email_parent_id (migration 0014).
// handleEmail returns (retired, err). When retired=true the caller MUST skip
// render / classify because the staging row has been deleted after deduplication
// or attachment fanout.
//
// filesOnly=true (from the mailbox intake policy) tells us to delete the
// parent after successful attachment fanout, and to enhance each child with the
// email metadata that would otherwise have lived on the parent
// (subject prefix on title, email Date on source_mtime, sender via
// the existing correspondent inheritance).
func (h *Handler) handleEmail(ctx context.Context, log *slog.Logger, parentID int64, raw []byte, filesOnly bool) (bool, error) {
	parsed, err := eml.Parse(raw)
	if err != nil {
		log.Warn("post-ingest.email.parse_failed", "err", err.Error())
		return false, h.updateDoc(ctx, parentID, "", "", 0)
	}
	// Message-ID dedup. If ANOTHER doc under the same owner already
	// carries this Message-ID, this doc is a duplicate — remove the staging row
	// it and stop. Same guarantee the IMAP path enforces at insert;
	// the fs-watch/upload paths don't know Message-ID until we parse.
	if parsed.MessageID != "" {
		var existing int64
		err := h.db.Read.QueryRowContext(ctx, `
			SELECT id FROM documents
			WHERE email_message_id = ?
			  AND id != ?
			  AND owner_id = (SELECT owner_id FROM documents WHERE id = ?)
			  AND trashed_at IS NULL
			LIMIT 1
		`, parsed.MessageID, parentID, parentID).Scan(&existing)
		if err == nil {
			log.Info("post-ingest.email.dedup",
				"parent_id", parentID, "existing", existing,
				"message_id", parsed.MessageID)
			if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
				return ingestmeta.CopySources(ctx, tx, parentID, existing)
			}); err != nil {
				return true, err
			}
			return true, h.deleteEmailStagingParent(ctx, parentID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			log.Warn("post-ingest.email.dedup_check", "err", err.Error())
		}
	}
	log.Info("post-ingest.email.parsed",
		"parent_id", parentID,
		"subject", parsed.Subject,
		"from", parsed.FromEmail,
		"attachments", len(parsed.Attachments))

	// Parent doc: title + content + created_at + Message-Id.
	body := parsed.TextBody
	if body == "" {
		body = parsed.HTMLBody
	}
	// Prepend structured "header" lines so a search for "from:<x>"
	// or "subject:<x>" tokens still lands hits even when the body
	// itself doesn't repeat them.
	var head strings.Builder
	if parsed.Subject != "" {
		fmt.Fprintf(&head, "subject: %s\n", parsed.Subject)
	}
	if parsed.FromEmail != "" {
		fmt.Fprintf(&head, "from: %s\n", parsed.FromEmail)
	}
	for _, a := range parsed.ToList {
		fmt.Fprintf(&head, "to: %s\n", a)
	}
	if head.Len() > 0 {
		body = head.String() + "\n" + body
	}

	if err := h.updateEmailParent(ctx, parentID, parsed, body); err != nil {
		return false, fmt.Errorf("update parent: %w", err)
	}

	// Load owner + jd_category once for children.
	var (
		ownerID      int64
		jdCategoryID int64
	)
	if err := h.db.Read.QueryRowContext(ctx,
		`SELECT owner_id, jd_category_id FROM documents WHERE id = ?`, parentID,
	).Scan(&ownerID, &jdCategoryID); err != nil {
		return false, fmt.Errorf("load parent owner: %w", err)
	}

	// Upsert the From address as a correspondent + attach to parent
	// under role=sender. Best-effort — a failure here doesn't kill
	// the fanout.
	if parsed.FromEmail != "" || parsed.FromName != "" {
		if err := h.attachEmailCorrespondent(ctx, parentID, parsed); err != nil {
			log.Warn("post-ingest.email.correspondent", "err", err.Error())
		}
	}

	childrenCreated := 0
	for i, att := range parsed.Attachments {
		if att.Inline {
			// Inline images referenced from HTML bodies aren't docs
			// — skip. Real attachments carry a Content-Disposition:
			// attachment or a filename+non-inline disposition.
			continue
		}
		if err := h.createEmailAttachmentChild(ctx, log, parentID, ownerID, jdCategoryID, i+1, att, parsed, filesOnly); err != nil {
			log.Warn("post-ingest.email.attachment_failed",
				"index", i+1, "filename", att.Filename, "err", err.Error())
			continue
		}
		childrenCreated++
	}
	// files_only accounts: the operator only wanted the attachments filed.
	// Remove the temporary parent .eml row so it cannot appear in Documents or
	// Trash. Guarded on childrenCreated > 0 so a
	// misconfigured account (flag on but no attachments this poll,
	// which shouldn't happen because the gate would drop) still
	// leaves the operator with SOMETHING to look at rather than a
	// silent no-op.
	if filesOnly && childrenCreated > 0 {
		log.Info("post-ingest.email.parent_retired",
			"parent_id", parentID, "children", childrenCreated,
			"reason", "files_only")
		return true, h.deleteEmailStagingParent(ctx, parentID)
	}
	return false, nil
}

// updateEmailParent writes the parsed header fields back onto the
// documents row. created_at flips to the email's Date when present so
// the timeline UI shows the message date, not the ingest time.
//
// pipeline_version_ocr is stamped at the current constant even though
// no OCR ran — an .eml body is plain text and never will need OCR.
// Leaving the column at 0 would cause the boot-time rescan detector
// to flag every ingested email as "OCR stale" forever.
func (h *Handler) updateEmailParent(ctx context.Context, docID int64, e *eml.Email, body string) error {
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		created := now
		if !e.Date.IsZero() {
			created = e.Date.Unix()
		}
		var titleArg any
		if e.Subject != "" {
			titleArg = e.Subject
		}
		var msgIDArg any
		if e.MessageID != "" {
			msgIDArg = e.MessageID
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE documents
			SET content = ?,
			    title = COALESCE(?, title),
			    created_at = ?,
			    email_message_id = COALESCE(?, email_message_id),
			    pipeline_version_content = ?,
			    pipeline_version_ocr = ?,
			    updated_at = ?
			WHERE id = ?
		`, body, titleArg, created, msgIDArg,
			PipelineVersionContent, PipelineVersionOCR,
			now, docID)
		return err
	})
}

// attachEmailCorrespondent upserts the From address as a correspondent
// and attaches it under role=sender via the multi-correspondent
// junction. Mirrors the LLM classifier's semantics — one canonical
// path for "who sent this."
func (h *Handler) attachEmailCorrespondent(ctx context.Context, docID int64, e *eml.Email) error {
	name := e.FromName
	if name == "" {
		name = e.FromEmail
	}
	if name == "" {
		return nil
	}
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO correspondents(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET updated_at = excluded.updated_at
		`, name, slug.Make(name), now, now); err != nil {
			return err
		}
		var corID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM correspondents WHERE name = ?`, name).Scan(&corID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO document_correspondents(document_id, correspondent_id, role)
			VALUES (?, ?, 'sender')
		`, docID, corID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE documents SET correspondent_id = COALESCE(correspondent_id, ?) WHERE id = ?
		`, corID, docID)
		return err
	})
}

// createEmailAttachmentChild does the (blob put + document row +
// post-ingest job) triple for one attachment. Uses the parent's
// owner + JD category as defaults; the pipeline (automations, LLM) can
// reclassify later.
//
// parsed is the enclosing email — used for source_mtime (Date) and,
// under filesOnly, for the subject prefix on the child's title
// (the parent .eml gets deleted so subject would otherwise be
// lost). email_parent_id is set to nil when filesOnly so the
// child doesn't dangle-point at a trashed row.
func (h *Handler) createEmailAttachmentChild(ctx context.Context, log *slog.Logger, parentID, ownerID, jdCategoryID int64, index int, att eml.Attachment, parsed *eml.Email, filesOnly bool) error {
	ref, err := h.cas.Put(bytes.NewReader(att.Bytes))
	if err != nil {
		return fmt.Errorf("cas put: %w", err)
	}
	title := att.Filename
	if title == "" {
		title = fmt.Sprintf("attachment-%d", index)
	}
	// files_only: the parent row is about to be deleted, so
	// prefix the subject onto the child title so the operator can still
	// see which email it came from. Skip if subject is empty or already
	// equals the filename (e.g. "Invoice.pdf" mail with a "Invoice.pdf"
	// attachment — the prefix would just double up).
	if filesOnly && parsed != nil && parsed.Subject != "" && parsed.Subject != title {
		title = "[" + parsed.Subject + "] " + title
	}
	mime := att.ContentType
	if mime == "" {
		mime = "application/octet-stream"
	}
	// source_mtime = email Date. Cheap on the parent-lives path (child
	// timeline reflects when the mail landed, not when the pipeline
	// ran) and essential on the files_only path (the email row
	// is gone, so this is the only surviving "when" signal).
	var sourceMtime any
	if parsed != nil && !parsed.Date.IsZero() {
		sourceMtime = parsed.Date.Unix()
	}
	// Under filesOnly the staging parent gets deleted. Write NULL up-front so
	// attachment rows never rely on an ON DELETE side effect.
	var parentRef any
	if !filesOnly {
		parentRef = parentID
	}
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at,
				email_parent_id, source_mtime
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, ownerID, ref.SHA256, ref.Size, title, mime,
			jdCategoryID, now, now, now, parentRef, sourceMtime)
		if err != nil {
			return err
		}
		childID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := ingestmeta.CopySources(ctx, tx, parentID, childID); err != nil {
			return err
		}
		// Inherit the parent email's correspondents (sender + any
		// recipients that got upserted). Without this, every attachment
		// PDF renders with a blank "From" field even though the email
		// itself is correctly attributed.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_correspondents (document_id, correspondent_id, role, position)
			SELECT ?, correspondent_id, role, position
			FROM document_correspondents WHERE document_id = ?
		`, childID, parentID); err != nil {
			return fmt.Errorf("inherit correspondents: %w", err)
		}
		payload, _ := json.Marshal(postIngestPayload{
			SHA256: ref.SHA256, Size: ref.Size, MIME: mime,
		})
		if err := jobs.Enqueue(ctx, tx, Kind, childID, string(payload)); err != nil {
			return err
		}
		log.Info("post-ingest.email.attachment_created",
			"parent_id", parentID, "child_id", childID,
			"filename", att.Filename, "mime", mime, "bytes", len(att.Bytes))
		return nil
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
		log.Warn("post-ingest.scan_blank.skip.no_binary")
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

// pwdSource labels where a candidate came from so recordDecrypted can
// update the right row (or nothing, for file-only candidates).
type pwdSource struct {
	// LearnedID is the decryption_passwords row id when this candidate
	// came from the learned table; zero when the candidate came from
	// the passwords file (in which case there's no last_used_at bump).
	LearnedID int64
}

// gatherDecryptCandidates returns the ordered list of candidate
// passwords qpdf should try, plus a parallel slice of pwdSource for
// the "which row won?" post-decrypt bookkeeping. Order:
//
//  1. Learned passwords for this doc's owner, hottest first.
//  2. Passwords from the operator's PasswordsFile, top-of-file first.
//
// The empty password is NOT in this list — qpdf tries it unconditionally
// before iterating candidates.
func (h *Handler) gatherDecryptCandidates(ctx context.Context, log *slog.Logger, docID int64) ([]string, []pwdSource, error) {
	var candidates []string
	var sources []pwdSource

	// Learned passwords for this doc's owner.
	if h.decrypt.Key != nil {
		ownerID, err := h.loadOwnerID(ctx, docID)
		if err != nil {
			return nil, nil, fmt.Errorf("load owner: %w", err)
		}
		rows, err := h.db.Read.QueryContext(ctx, `
			SELECT id, ciphertext FROM decryption_passwords
			WHERE owner_id = ?
			ORDER BY last_used_at DESC NULLS LAST, id
		`, ownerID)
		if err != nil {
			return nil, nil, fmt.Errorf("query learned: %w", err)
		}
		for rows.Next() {
			var (
				id     int64
				sealed []byte
			)
			if err := rows.Scan(&id, &sealed); err != nil {
				rows.Close()
				return nil, nil, err
			}
			pt, err := h.decrypt.Key.Open(sealed)
			if err != nil {
				log.Warn("post-ingest.decrypt.stale_password",
					"id", id, "err", err.Error())
				continue
			}
			candidates = append(candidates, string(pt))
			sources = append(sources, pwdSource{LearnedID: id})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("iterate learned passwords: %w", err)
		}
		rows.Close()
	}

	// Operator's passwords file. Cheap re-read each time — the file
	// is small and this lets operators add passwords without restart.
	if h.decrypt.PasswordsFile != "" {
		fileWords, err := loadPasswordsFile(h.decrypt.PasswordsFile)
		if err != nil {
			log.Warn("post-ingest.decrypt.passwords_file_read",
				"path", h.decrypt.PasswordsFile, "err", err.Error())
		}
		for _, w := range fileWords {
			candidates = append(candidates, w)
			sources = append(sources, pwdSource{}) // file-source, no ID
		}
	}
	return candidates, sources, nil
}

// loadOwnerID pulls owner_id for a docID. Kept separate from loadDoc
// so the decrypt candidate lookup doesn't force a schema change to
// loadDoc's shape.
func (h *Handler) loadOwnerID(ctx context.Context, docID int64) (int64, error) {
	var owner int64
	err := h.db.Read.QueryRowContext(ctx,
		`SELECT owner_id FROM documents WHERE id = ?`, docID).Scan(&owner)
	return owner, err
}

// loadPasswordsFile reads a newline-separated password list. Blank
// lines and lines starting with '#' are skipped so operators can
// annotate the file. Trims trailing whitespace on each candidate.
func loadPasswordsFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// markEncrypted transitions a doc to encryption_state='encrypted' and
// stops the post-ingest chain. Called when every decrypt candidate
// (empty + file + learned) fails on password error.
func (h *Handler) markEncrypted(ctx context.Context, docID int64, stderr string) error {
	err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE documents
			SET encryption_state = 'encrypted', updated_at = ?
			WHERE id = ?
		`, time.Now().Unix(), docID)
		return err
	})
	if err != nil {
		return fmt.Errorf("mark encrypted: %w", err)
	}
	h.log.Info("post-ingest.decrypt.awaiting_password",
		"doc_id", docID, "qpdf_stderr_tail", stderr)
	return nil
}

// recordDecrypted writes the decrypted bytes into the CAS as a second
// blob, populates documents.decrypted_blob + decrypted_size + state,
// and bumps last_used_at on the winning learned password (if any).
func (h *Handler) recordDecrypted(ctx context.Context, log *slog.Logger, docID int64, pdfBytes []byte, sources []pwdSource, index int) error {
	ref, err := h.cas.Put(bytes.NewReader(pdfBytes))
	if err != nil {
		return fmt.Errorf("cas put decrypted: %w", err)
	}
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents
			SET encryption_state = 'decrypted',
			    decrypted_blob = ?, decrypted_size = ?, updated_at = ?
			WHERE id = ?
		`, ref.SHA256, ref.Size, time.Now().Unix(), docID); err != nil {
			return err
		}
		if index >= 0 && index < len(sources) {
			src := sources[index]
			if src.LearnedID != 0 {
				if _, err := tx.ExecContext(ctx, `
					UPDATE decryption_passwords
					SET last_used_at = ?, last_used_doc_id = ?
					WHERE id = ?
				`, time.Now().Unix(), docID, src.LearnedID); err != nil {
					return err
				}
				log.Info("post-ingest.decrypt.learned_hit",
					"doc_id", docID, "pwd_id", src.LearnedID)
			} else {
				log.Info("post-ingest.decrypt.file_hit",
					"doc_id", docID, "file_index", index)
			}
		}
		return nil
	})
}

func (h *Handler) ocrLanguages() []string {
	if h.languageState != nil {
		if languages := h.languageState(); len(languages) > 0 {
			return append([]string(nil), languages...)
		}
	}
	return append([]string(nil), h.langs...)
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
			Languages:    h.ocrLanguages(),
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
			Languages: h.ocrLanguages(),
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
// PDF and image paths: automations, rendered-view refresh, and the
// LLM classify handoff. Extracted so both entry paths share exactly
// one implementation.
func (h *Handler) postContentSteps(ctx context.Context, log *slog.Logger, docID int64) error {
	// Language detection — runs before the LLM classifier so the
	// stamped code can hint the prompt. No-op when the chain is
	// empty (default v1 build) or when the doc's languages are
	// user-locked. See core/lang for the interface + defaults.
	h.detectLanguages(ctx, log, docID)

	// Archive matching always runs locally before user automations and the
	// optional model. It only fills unresolved metadata, so explicit
	// automations remain authoritative when both find a match.
	if err := automations.ApplyFromArchive(ctx, h.db, log, docID); err != nil {
		log.Warn("post-ingest.archive_classification.error", "err", err.Error())
	}

	// Automations — trigger→conditions→actions on document_added.
	// Fail-soft: an automation error logs a warning and never blocks the
	// rest of the post-ingest chain. See core/automations for the shape.
	if err := automations.ApplyOnDocumentAdded(ctx, h.db, log, docID); err != nil {
		log.Warn("post-ingest.automations.error", "err", err.Error())
	}

	// Rendered-view projection — best-effort. A failed render logs
	// a warning; the doc row is already the source of truth.
	if h.render != nil {
		if _, err := h.render.Render(ctx, docID); err != nil {
			log.Warn("post-ingest.render.error", "err", err.Error())
		}
	}

	// First-page thumbnail. Best-effort — a missing pdftoppm skips, non-PDFs
	// fall through the same skip path. On success the endpoint
	// GET /api/documents/{id}/thumb/ serves it with an immutable ETag.
	h.generateThumb(ctx, log, docID)

	// LLM classification handoff: enqueue a post-classify job that
	// the llm-classifier plugin's Subscriber picks up. Only enqueue
	// when the plugin is actually registered — otherwise the job
	// would die as a "no subscriber for kind" dead-letter.
	if h.classifyEnabled != nil && h.classifyEnabled() {
		if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
			return jobs.Enqueue(ctx, tx, PostClassifyKind, docID, "{}")
		}); err != nil {
			log.Warn("post-ingest.enqueue_classify", "err", err.Error())
		}
	}

	// Notification feed: the pipeline has finished. document.create
	// was already audited at upload time; this event marks "content
	// is available, OCR/render done" — the state a user actually
	// waits for. GET /api/events/ projects this into the activity
	// drawer as "Ingested <title>". Best-effort; a failed audit
	// write doesn't fail the pipeline.
	var title sql.NullString
	_ = h.db.Read.QueryRowContext(ctx,
		`SELECT title FROM documents WHERE id = ?`, docID).Scan(&title)
	audit.Log(ctx, h.db, log, audit.Event{
		Action: "document.ingested", ObjectKind: "document", ObjectID: docID,
		After: map[string]any{"title": title.String},
	})
	return nil
}

// generateThumb renders page 1 of the doc's archive PDF (preferred)
// or original blob into a PNG thumbnail, stores it in CAS, and
// updates documents.thumb_sha. Every failure path (no archive_blob,
// non-PDF, pdftoppm missing, rasterize timeout) logs at Warn/Info
// and returns — thumbnails are a UX nicety, not an ingest invariant.
//
// Blob priority: archive → decrypted → original. A doc that was
// decrypted post-ingest (encryption_state='decrypted') has a working
// copy in decrypted_blob; pdftoppm on the raw original_blob would
// fail with "Incorrect password" because originals are preserved
// verbatim in the CAS (spec §immutability).
func (h *Handler) generateThumb(ctx context.Context, log *slog.Logger, docID int64) {
	var (
		archive sql.NullString
		orig    sql.NullString
		dec     sql.NullString
		mime    sql.NullString
	)
	if err := h.db.Read.QueryRowContext(ctx,
		`SELECT archive_blob, original_blob, decrypted_blob, mime_type FROM documents WHERE id = ?`,
		docID).Scan(&archive, &orig, &dec, &mime); err != nil {
		log.Warn("post-ingest.thumb.load_doc", "err", err.Error())
		return
	}
	// Only PDFs have a well-known page-1 concept. Non-PDF docs (image
	// originals, epub, msg) skip; the endpoint 404s and the SPA
	// renders its initials placeholder.
	var blobSHA string
	switch {
	case archive.Valid && archive.String != "":
		blobSHA = archive.String
	case dec.Valid && dec.String != "":
		blobSHA = dec.String
	case orig.Valid && orig.String != "" && mime.Valid && mime.String == "application/pdf":
		blobSHA = orig.String
	}
	if blobSHA == "" {
		return
	}
	rc, err := h.cas.Get(blobSHA)
	if err != nil {
		log.Warn("post-ingest.thumb.cas_get", "err", err.Error())
		return
	}
	pdfBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		log.Warn("post-ingest.thumb.read", "err", err.Error())
		return
	}

	res, err := thumb.Render(ctx, pdfBytes, log, thumb.Options{})
	if err != nil {
		log.Warn("post-ingest.thumb.render", "err", err.Error())
		return
	}
	if res.Skipped {
		return
	}
	ref, err := h.cas.Put(bytes.NewReader(res.PNG))
	if err != nil {
		log.Warn("post-ingest.thumb.cas_put", "err", err.Error())
		return
	}
	if _, err := h.db.ExecWrite(ctx,
		`UPDATE documents SET thumb_sha = ?, updated_at = unixepoch() WHERE id = ?`,
		ref.SHA256, docID); err != nil {
		log.Warn("post-ingest.thumb.write", "err", err.Error())
		return
	}
	log.Info("post-ingest.thumb.written", "doc_id", docID, "sha", ref.SHA256, "size", len(res.PNG))
}

// emailFilesOnlyFromPayload extracts the email_files_only
// flag emailwatch stamps on its post-ingest payload. Dispatcher hands
// the raw JSON as e.Payload["raw"] (see core/jobs/jobs.go), so we
// unmarshal that string instead of reading a top-level map key.
// Missing / non-string raw / non-email producer → false.
func emailFilesOnlyFromPayload(p map[string]any) bool {
	if p == nil {
		return false
	}
	raw, ok := p["raw"].(string)
	if !ok || raw == "" {
		return false
	}
	var pl struct {
		EmailFilesOnly bool `json:"email_files_only"`
	}
	if err := json.Unmarshal([]byte(raw), &pl); err != nil {
		return false
	}
	return pl.EmailFilesOnly
}

// consumptionContextFromPayload pulls the trigger-filter fields
// producers put on the job payload. Missing fields become zero
// values, which map to "no filter" in automations.ApplyOnConsumption.
func consumptionContextFromPayload(p map[string]any) automations.Context {
	var c automations.Context
	if p == nil {
		return c
	}
	if s, ok := p["filename"].(string); ok {
		c.Filename = s
	}
	if s, ok := p["source_path"].(string); ok {
		c.SourcePath = s
	}
	switch n := p["mail_rule_id"].(type) {
	case float64:
		c.MailRuleID = int64(n)
	case int64:
		c.MailRuleID = n
	case int:
		c.MailRuleID = int64(n)
	}
	return c
}

// loadDoc reads the fields needed to start processing. The trashed_at guard means
// a race between soft-delete and post-ingest gets us "not found" and
// the retry loop eventually parks the job dead — better than doing OCR
// on a document the user already trashed.
func (h *Handler) loadDoc(ctx context.Context, id int64) (origBlob, mime, ownerEmail string, err error) {
	var mimeNull sql.NullString
	err = h.db.Read.QueryRowContext(ctx, `
		SELECT d.original_blob, COALESCE(d.mime_type, ''), u.email
		FROM documents d
		JOIN users u ON u.id = d.owner_id
		WHERE d.id = ? AND d.trashed_at IS NULL
	`, id).Scan(&origBlob, &mimeNull, &ownerEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", fmt.Errorf("doc %d not found or trashed", id)
	}
	if mimeNull.Valid {
		mime = mimeNull.String
	}
	return
}

// readBlob loads one pipeline input into memory.
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
			SET content = ?, archive_blob = ?, archive_size = ?,
			    pipeline_version_content = ?, pipeline_version_ocr = ?,
			    updated_at = ?
			WHERE id = ?
		`, content, archBlobArg, archSizeArg,
			PipelineVersionContent, PipelineVersionOCR,
			time.Now().Unix(), id)
		return err
	})
}

// isTextMIME reports whether the MIME type identifies a text source
// whose bytes are already the extracted content (plain, markdown,
// HTML). Charset parameters are tolerated: `text/plain; charset=utf-8`
// matches.
func isTextMIME(mime string) bool {
	m := strings.ToLower(mime)
	if idx := strings.IndexByte(m, ';'); idx > 0 {
		m = strings.TrimSpace(m[:idx])
	}
	switch m {
	case "text/plain", "text/markdown", "text/html":
		return true
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// detectLanguages runs the registered detector chain against the
// doc's extracted content and, if a candidate clears the
// confidence threshold, writes it to documents.languages. A nil
// or empty chain makes this a no-op — the LLM classifier plugin
// can still write documents.languages independently.
//
// Best-effort: every failure path logs and returns. Language
// stamping is a nicety, never fatal to post-ingest.
func (h *Handler) detectLanguages(ctx context.Context, log *slog.Logger, docID int64) {
	if h.langChain == nil || h.langChain.Len() == 0 {
		return
	}
	var (
		content sql.NullString
		locked  int
	)
	if err := h.db.Read.QueryRowContext(ctx,
		`SELECT content, languages_locked FROM documents WHERE id = ?`,
		docID).Scan(&content, &locked); err != nil {
		log.Warn("post-ingest.lang.load", "err", err.Error())
		return
	}
	if locked != 0 {
		// User set the languages via PATCH — never overwrite.
		return
	}
	if !content.Valid || content.String == "" {
		// No text to detect against (image-only, extraction failed).
		return
	}
	r, ok := h.langChain.BestAbove(content.String, 0.5)
	if !ok {
		return
	}
	code := lang.Format(r.Code)
	if code == "" {
		return
	}
	if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET languages = ?, updated_at = ?
			 WHERE id = ? AND languages_locked = 0`,
			code, time.Now().Unix(), docID)
		return err
	}); err != nil {
		log.Warn("post-ingest.lang.write", "err", err.Error())
		return
	}
	log.Info("post-ingest.lang.detected", "doc_id", docID, "languages", code)
}
