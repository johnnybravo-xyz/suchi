package main

import (
	"log/slog"
	"os/exec"
	"sort"
)

// pipelineTool describes one external binary the pipeline shells out
// to. impact + install are user-facing text — kept flat here (not
// pulled from each pipeline package) so the whole diagnostic reads
// from one file at code-review time.
type pipelineTool struct {
	name    string
	impact  string
	install string
}

// pipelineTools mirrors the list `suchi doctor` walks so the two
// diagnostics agree. Order is intentional: PDF-plane tools first
// (most common ingest), then office (anydoc), then peripheral
// converters (HEIC, msg).
var pipelineTools = []pipelineTool{
	{
		name:    "qpdf",
		impact:  "PDF normalization + encrypted-PDF gate",
		install: "apt install qpdf • brew install qpdf",
	},
	{
		name:    "pdftotext",
		impact:  "text-native PDF extraction (no OCR needed)",
		install: "apt install poppler-utils • brew install poppler",
	},
	{
		name:    "pdftoppm",
		impact:  "PDF rasterization for tessocr + QR-split detection",
		install: "apt install poppler-utils • brew install poppler",
	},
	{
		name:    "tesseract",
		impact:  "OCR engine for scanned PDFs (tessocr path)",
		install: "apt install tesseract-ocr • brew install tesseract",
	},
	{
		name:    "ocrmypdf",
		impact:  "OCR engine that produces searchable-PDF archives",
		install: "apt install ocrmypdf • brew install ocrmypdf",
	},
	{
		name:    "djvutxt",
		impact:  "DjVu text extraction",
		install: "apt install djvulibre-bin • brew install djvulibre",
	},
	{
		name:    "anydoc",
		impact:  "office documents (docx/xlsx/pptx/odt/rtf/csv) content extraction — files still ingest, but FTS won't index the text",
		install: "https://github.com/johnnybravo-xyz/suchi/releases (anydoc-<os>-<arch>) or `bash <(curl -sfL .../install-anydoc.sh)`",
	},
	{
		name:    "magick",
		impact:  "HEIC → JPEG conversion for iPhone photo ingest (convert also accepted as legacy alias)",
		install: "apt install imagemagick • brew install imagemagick",
	},
	{
		name:    "msgconvert",
		impact:  "Outlook .msg → .eml conversion",
		install: "apt install libemail-outlook-message-perl",
	},
}

// reportToolAvailability walks the pipeline binary list and logs the
// state at boot: one INFO summary with present/missing lists, then one
// WARN per missing tool carrying the impact + install hint. Never
// fails boot — missing tools degrade features, they don't break the
// server.
//
// Wired from runServe() right after logEgressSurface — same shape as
// the egress diagnostic, and lands in the log stream operators
// already grep for boot-time info.
func reportToolAvailability(log *slog.Logger) {
	var present, missing []string
	details := make(map[string]pipelineTool, len(pipelineTools))
	for _, t := range pipelineTools {
		if _, err := exec.LookPath(t.name); err == nil {
			present = append(present, t.name)
		} else {
			missing = append(missing, t.name)
			details[t.name] = t
		}
	}
	sort.Strings(present)
	sort.Strings(missing)
	log.Info("main.tools.check",
		"present", present,
		"missing", missing)
	for _, name := range missing {
		t := details[name]
		log.Warn("main.tools.missing",
			"tool", name,
			"impact", t.impact,
			"install", t.install)
	}
}
