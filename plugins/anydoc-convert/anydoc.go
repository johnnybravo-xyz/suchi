// Package anydocconvert wraps the firecrawl/anydoc CLI to extract
// text from office documents (Word, PowerPoint, Excel, OpenDocument,
// RTF, EPUB, CSV) as GitHub-flavored Markdown. Markdown lands as
// documents.content so FTS5 indexes it same as OCR'd PDF text.
//
// Scoping-only: the runtime path is stubbed. This module registers
// the subscriber interface and MIME allowlist; wiring lands in the
// follow-up PR once anydoc is baked into the Docker images.
//
// Phase 3.5 — office document coverage. Upstream: MIT-licensed
// Rust library from Firecrawl (github.com/firecrawl/anydoc).
package anydocconvert

import (
	"context"
	"log/slog"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Plugin is the wire seam. Held minimal in the scoping PR — no config,
// no dispatcher registration, no sandbox call. The follow-up PR fills
// out Handle to invoke the anydoc CLI via core/sandbox and thread the
// stdout markdown into documents.content.
type Plugin struct {
	log *slog.Logger
	// bin is the path to the anydoc CLI. Empty means the plugin is
	// disabled — Handle returns nil without touching the doc.
	bin string
}

// Config is the plugin's boot-time knob set. Populated from env vars
// in the follow-up. Kept as a named type now so the runtime PR is a
// pure field-fill exercise, not a refactor of the constructor shape.
type Config struct {
	// Bin overrides the CLI path. Empty → look up "anydoc" on PATH at
	// New time.
	Bin string
	// TimeoutSec bounds each subprocess call. Zero → default 30s.
	TimeoutSec int
	// MaxOutputMB caps captured markdown; larger runs get truncated
	// with a warning. Zero → default 5.
	MaxOutputMB int
}

// New constructs the plugin. Returns a plugin with bin="" (disabled)
// when the CLI is not found — same graceful-degrade posture as the
// ocrmypdf plugin. The runtime PR fills this in.
func New(cfg Config, log *slog.Logger) (*Plugin, error) {
	return &Plugin{log: log.With("component", "anydoc-convert")}, nil
}

// Kinds returns the job kinds this subscriber handles. Follow-up PR
// wires "post-ingest" filtered on MIME, so anydoc only runs for the
// formats it understands. Empty here because the plugin is inactive.
func (p *Plugin) Kinds() []string { return nil }

// Handle is the subscriber entry point. Stub for now.
func (p *Plugin) Handle(_ context.Context, _ pluginapi.Event) error { return nil }

// SupportedMIMEs is the anydoc coverage set, cribbed from the upstream
// README (github.com/firecrawl/anydoc). Grouped so the follow-up PR
// can wire the dispatcher off one lookup.
//
// PDF is deliberately absent — suchi already has a PDF pipeline
// (qpdf → pdf-inspector → ocrmypdf); anydoc's PDF path would be
// redundant and we don't want two ways to extract PDF text.
var SupportedMIMEs = map[string]bool{
	// Word
	"application/msword": true, // .doc
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true, // .docx
	"application/vnd.ms-word.document.macroenabled.12":                        true, // .docm
	// PowerPoint
	"application/vnd.ms-powerpoint":                                             true, // .ppt, .pps, .pot
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true, // .pptx
	"application/vnd.ms-powerpoint.presentation.macroenabled.12":                true, // .pptm
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow":    true, // .ppsx
	"application/vnd.ms-powerpoint.slideshow.macroenabled.12":                   true, // .ppsm
	// Excel
	"application/vnd.ms-excel": true, // .xls
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true, // .xlsx
	"application/vnd.ms-excel.sheet.macroenabled.12":                    true, // .xlsm
	"application/vnd.ms-excel.sheet.binary.macroenabled.12":             true, // .xlsb
	// OpenDocument
	"application/vnd.oasis.opendocument.text":         true, // .odt
	"application/vnd.oasis.opendocument.spreadsheet":  true, // .ods
	"application/vnd.oasis.opendocument.presentation": true, // .odp
	// Rich Text
	"application/rtf": true, // .rtf
	"text/rtf":        true,
	// EPUB is left to the existing epub plugin — anydoc's coverage
	// overlaps but the current path already handles it.
	// CSV
	"text/csv": true,
}

// Handles reports whether anydoc claims coverage of the given MIME.
// Used by the follow-up PR's post-ingest dispatcher to route into
// this plugin instead of the OCR chain.
func Handles(mime string) bool { return SupportedMIMEs[mime] }
