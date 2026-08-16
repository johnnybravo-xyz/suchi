// Package imgpdf wraps a raster image (JPEG, PNG, TIFF, WebP, GIF,
// BMP) into a single-page PDF via ImageMagick.
//
// This exists so bare images can flow through the same OCR pipeline
// PDFs and HEICs use — postingest converts the image to a PDF, hands
// it to runOCR, and the result is a searchable archive with extracted
// text. Without this step, non-HEIC images land with content = only
// barcode tokens (empty when no barcode present), which blinds search,
// list snippets, and similar-docs.
//
// Unlike HEIC (which needs a HEIC→PNG→PDF two-step because Alpine's
// ImageMagick fails single-call HEIC→PDF), regular raster formats
// convert directly in one magick invocation.
package imgpdf

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

const (
	// DefaultBinary is ImageMagick 7's unified CLI. Falls back to
	// "convert" (ImageMagick 6) when unavailable so alpine/deb both
	// work without config.
	DefaultBinary  = "magick"
	FallbackBinary = "convert"
	DefaultTimeout = 60 * time.Second
	// DefaultMaxOutputBytes caps the converted PDF size. 32 MiB is
	// generous for a high-DPI scan; real-world documents land in the
	// low single-digit MB range.
	DefaultMaxOutputBytes = 32 * 1024 * 1024
)

// Options carries per-call knobs. Zero-value uses Default* above.
type Options struct {
	Binary         string
	Timeout        time.Duration
	MaxOutputBytes int64
	// Quality tunes the JPEG-in-PDF quality (1-100). Zero uses
	// ImageMagick's default (~92 for JPEG). Lower = smaller archive.
	Quality int
	// Ext hints the input file extension so ImageMagick's format
	// decoder picks the right delegate. Optional — magick can usually
	// sniff from bytes, but a hint helps with ambiguous headers.
	Ext string
}

// Result carries the conversion outcome.
type Result struct {
	PDF        []byte
	Skipped    bool // binary not present or input unreadable/unsupported
	Duration   time.Duration
	StderrTail string
}

// Recognized reports whether mime is a raster image format we can
// wrap. Explicitly excludes vector formats (SVG) — they don't need
// image OCR. HEIC is handled by the dedicated heic package.
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	if idx := strings.IndexByte(m, ';'); idx > 0 {
		m = strings.TrimSpace(m[:idx])
	}
	switch m {
	case "image/jpeg", "image/jpg", "image/pjpeg",
		"image/png",
		"image/tiff", "image/x-tiff",
		"image/webp",
		"image/gif",
		"image/bmp", "image/x-bmp", "image/x-ms-bmp":
		return true
	}
	return false
}

// Available reports whether an ImageMagick binary is on PATH.
func Available() bool {
	if _, err := exec.LookPath(DefaultBinary); err == nil {
		return true
	}
	if _, err := exec.LookPath(FallbackBinary); err == nil {
		return true
	}
	return false
}

// Convert streams image bytes to a tempfile and runs ImageMagick to
// produce a PDF. Skipped=true when no ImageMagick is installed — safe
// caller behavior is "keep the image as-is; someone can install magick
// later and re-run".
func Convert(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "imgpdf")

	binary := opts.Binary
	if binary == "" {
		if _, err := exec.LookPath(DefaultBinary); err == nil {
			binary = DefaultBinary
		} else if _, err := exec.LookPath(FallbackBinary); err == nil {
			binary = FallbackBinary
		} else {
			_, _ = io.Copy(io.Discard, src)
			log.Warn("imgpdf.skip.no_binary", "binary", DefaultBinary)
			return &Result{Skipped: true, StderrTail: "ImageMagick not available"}, nil
		}
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxBytes := opts.MaxOutputBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxOutputBytes
	}

	dir, err := os.MkdirTemp("", "suchi-imgpdf-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	// Use the caller's extension hint when present so magick picks the
	// correct decoder. Fallback "img" lets magick sniff from bytes.
	inExt := opts.Ext
	if inExt == "" {
		inExt = "img"
	} else {
		inExt = strings.TrimPrefix(inExt, ".")
	}
	inputPath := filepath.Join(dir, "in."+inExt)
	outputPath := filepath.Join(dir, "out.pdf")
	if err := writeAll(inputPath, src); err != nil {
		return nil, err
	}

	// One-shot conversion — direct image → PDF works for jpeg/png/tiff/
	// webp/gif/bmp on both Alpine and Debian ImageMagick. Only HEIC
	// needs the two-step PNG intermediate (see the heic package).
	pdfArgs := []string{binary, inputPath}
	if opts.Quality > 0 && opts.Quality <= 100 {
		pdfArgs = append(pdfArgs, "-quality", fmt.Sprintf("%d", opts.Quality))
	}
	pdfArgs = append(pdfArgs, outputPath)

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:    pdfArgs,
		Timeout: timeout,
		Dir:     dir,
	})
	dur := time.Since(start)
	if err != nil {
		log.Info("imgpdf.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res.Stderr), Duration: dur}, nil
	}

	pdf, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		return &Result{Skipped: true, StderrTail: readErr.Error(), Duration: dur}, nil
	}
	if int64(len(pdf)) > maxBytes {
		log.Warn("imgpdf.truncated_output", "bytes", len(pdf), "cap", maxBytes)
		return &Result{Skipped: true,
			StderrTail: fmt.Sprintf("output %d bytes exceeded cap %d", len(pdf), maxBytes),
			Duration:   dur,
		}, nil
	}

	return &Result{
		PDF:        pdf,
		Duration:   dur,
		StderrTail: tail(res.Stderr),
	}, nil
}

// ExtFromMIME returns the conventional file extension for a MIME type
// so callers can pass a helpful Ext hint. Unknown returns "img".
func ExtFromMIME(mime string) string {
	m := strings.ToLower(mime)
	if idx := strings.IndexByte(m, ';'); idx > 0 {
		m = strings.TrimSpace(m[:idx])
	}
	switch m {
	case "image/jpeg", "image/jpg", "image/pjpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/tiff", "image/x-tiff":
		return "tiff"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp", "image/x-bmp", "image/x-ms-bmp":
		return "bmp"
	}
	return "img"
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
	return "..." + strings.TrimSpace(string(b[len(b)-n:]))
}
