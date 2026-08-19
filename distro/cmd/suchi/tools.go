package main

import (
	"log/slog"
	"os/exec"
	"sort"
)

type pipelineTool struct {
	name      string
	fallbacks []string
	impact    string
	install   string
}

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
		name:      "magick",
		fallbacks: []string{"convert"},
		impact:    "HEIC → JPEG conversion for iPhone photo ingest",
		install:   "apt install imagemagick • brew install imagemagick",
	},
	{
		name:    "msgconvert",
		impact:  "Outlook .msg → .eml conversion",
		install: "apt install libemail-outlook-message-perl",
	},
}

func reportToolAvailability(log *slog.Logger) {
	var present, missing []string
	details := make(map[string]pipelineTool, len(pipelineTools))
	for _, t := range pipelineTools {
		if binary, ok := resolvePipelineTool(t, exec.LookPath); ok {
			present = append(present, binary)
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

func resolvePipelineTool(t pipelineTool, lookPath func(string) (string, error)) (string, bool) {
	for _, name := range append([]string{t.name}, t.fallbacks...) {
		if _, err := lookPath(name); err == nil {
			return name, true
		}
	}
	return "", false
}
