// Package anydoc extracts text from office documents (Word,
// PowerPoint, Excel, OpenDocument, RTF, CSV) via the firecrawl/anydoc
// CLI. Same shape as core/pipeline/djvu — degrades gracefully when the
// binary isn't installed, returns HasText true only when we pulled
// enough content to trust.
//
// Upstream: MIT-licensed Rust library from Firecrawl. anydoc converts
// documents to clean GitHub-flavored Markdown at ~5ms per document.
// suchi shells out to the standalone CLI via core/sandbox; stdout
// markdown lands as documents.content and FTS5 indexes it just like
// OCR'd PDF text.
//
// PDF and EPUB are deliberately NOT routed here — suchi has its own
// PDF pipeline (qpdf → pdf-inspector → ocrmypdf) and a purpose-built
// EPUB extractor. anydoc handles them upstream but we don't want two
// paths for the same MIME.
package anydoc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

const (
	// DefaultBinary is the CLI name expected on PATH. The Docker image
	// integration installs the anydoc CLI (see Dockerfile / follow-up
	// sourcing PR); bare-metal installs need to put it there manually.
	DefaultBinary = "anydoc"
	// DefaultTimeout bounds each conversion. anydoc's own median is
	// ~5ms per doc; 30s is a big safety margin for pathological files.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxTextBytes matches EPUB's cap: office docs can be very
	// long. Overridden per-instance via ANYDOC_MAX_CONTENT_BYTES.
	DefaultMaxTextBytes = 32 * 1024 * 1024
	// HasTextThreshold matches djvu — small enough that a nearly-empty
	// doc still reports HasText=true; big enough that noise doesn't.
	HasTextThreshold = 32
)

// Options carries per-call knobs. Zero-value uses Default* above.
type Options struct {
	Binary       string
	Timeout      time.Duration
	MaxTextBytes int64
	// Ext hints anydoc at the format for CSV (no magic bytes) or files
	// where the extension has been lost. Empty is fine — anydoc sniffs
	// from bytes for everything except CSV.
	Ext string
}

// Result mirrors djvu.Result for a familiar caller contract.
type Result struct {
	Text       string
	HasText    bool
	Skipped    bool
	Truncated  bool // stdout exceeded MaxTextBytes; kept what fit
	NonBlank   int
	Duration   time.Duration
	StderrTail string
}

// supportedMIMEs is the anydoc coverage set, keyed to the MIME types
// suchi's sniffer emits for these formats. Kept as an exact-match
// allowlist so a wildcard trailing charset (e.g. "text/csv; charset=utf-8")
// still routes correctly via Recognized() below, which strips params
// before lookup.
//
// EPUB and PDF are omitted: they have dedicated extractors upstream.
var supportedMIMEs = map[string]bool{
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
	// CSV
	"text/csv": true,
}

// Recognized reports whether mime is one anydoc converts. Strips
// content-type params (`; charset=…`) before lookup.
func Recognized(mime string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	return supportedMIMEs[mime]
}

// Available reports whether anydoc is on PATH. Postingest checks this
// to log a specific route.no_binary hint instead of a generic skip.
func Available() bool {
	_, err := exec.LookPath(DefaultBinary)
	return err == nil
}

// Extract streams src to a tempfile and runs anydoc against it.
// Skipped=true when the binary is missing — safe caller behavior is
// "leave content empty; install anydoc + re-ingest later".
func Extract(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "anydoc")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		_, _ = io.Copy(io.Discard, src)
		log.Info("anydoc.skip.no_binary", "binary", binary)
		return &Result{Skipped: true, StderrTail: "anydoc not available"}, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxText := opts.MaxTextBytes
	if maxText == 0 {
		maxText = DefaultMaxTextBytes
	}

	dir, err := os.MkdirTemp("", "suchi-anydoc-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	// Extension in the tempfile name matters: anydoc sniffs by content
	// for most formats, but CSV has no magic bytes and needs the
	// extension (or -f csv). Keep the operator-supplied extension when
	// present; else fall back to .bin (works for everything but CSV).
	ext := opts.Ext
	if ext == "" {
		ext = ".bin"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	inputPath := filepath.Join(dir, "in"+ext)
	if err := writeAll(inputPath, src); err != nil {
		return nil, err
	}

	// anydoc's CLI (example/convert.rs upstream) writes GFM to stdout
	// when no -o is given. Args are minimal on purpose: interface
	// stability of examples/ is not guaranteed by upstream, so we lean
	// on the smallest possible surface.
	args := []string{binary, inputPath}

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      args,
		Timeout:   timeout,
		MaxStdout: maxText,
		Dir:       dir,
	})
	dur := time.Since(start)
	if err != nil {
		log.Info("anydoc.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res.Stderr), Duration: dur}, nil
	}
	if res.StdoutTruncated {
		log.Warn("anydoc.truncated", "cap_bytes", maxText)
	}

	text := string(res.Stdout)
	nonBlank := countNonWhitespace(text)
	return &Result{
		Text:       text,
		HasText:    nonBlank >= HasTextThreshold,
		Truncated:  res.StdoutTruncated,
		NonBlank:   nonBlank,
		Duration:   dur,
		StderrTail: tail(res.Stderr),
	}, nil
}

// ExtFromMIME picks a reasonable filename extension for a MIME so the
// tempfile written for anydoc keeps the format hint. Postingest passes
// this through so the caller doesn't have to know the MIME→ext table.
func ExtFromMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch mime {
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-word.document.macroenabled.12":
		return ".docm"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/vnd.ms-powerpoint.presentation.macroenabled.12":
		return ".pptm"
	case "application/vnd.openxmlformats-officedocument.presentationml.slideshow":
		return ".ppsx"
	case "application/vnd.ms-powerpoint.slideshow.macroenabled.12":
		return ".ppsm"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.ms-excel.sheet.macroenabled.12":
		return ".xlsm"
	case "application/vnd.ms-excel.sheet.binary.macroenabled.12":
		return ".xlsb"
	case "application/vnd.oasis.opendocument.text":
		return ".odt"
	case "application/vnd.oasis.opendocument.spreadsheet":
		return ".ods"
	case "application/vnd.oasis.opendocument.presentation":
		return ".odp"
	case "application/rtf", "text/rtf":
		return ".rtf"
	case "text/csv":
		return ".csv"
	}
	return ""
}

func countNonWhitespace(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func writeAll(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Sync()
}

func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(bytes.TrimSpace(b[len(b)-n:])))
}
