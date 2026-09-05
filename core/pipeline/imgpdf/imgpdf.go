// Package imgpdf wraps raster images in a PDF so the standard OCR
// path can extract text. Only OCRmyPDF embeds that text in the PDF.
package imgpdf

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

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipefile"
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
// wrap. Explicitly excludes vector formats (SVG) — they don't need image OCR.
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	if idx := strings.IndexByte(m, ';'); idx > 0 {
		m = strings.TrimSpace(m[:idx])
	}
	switch m {
	case "image/jpeg", "image/jpg", "image/pjpeg",
		"image/png",
		"image/tiff", "image/x-tiff",
		"image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence",
		"image/webp",
		"image/gif",
		"image/bmp", "image/x-bmp", "image/x-ms-bmp":
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
	if err := pipefile.WriteAll(inputPath, src); err != nil {
		return nil, err
	}

	// HEIC/HEIF uses the still frame, not the rest of a photo sequence.
	// Other formats retain every page, including multipage TIFF scans.
	if inExt == "heic" || inExt == "heif" {
		inputPath += "[0]"
	}
	pdfArgs := []string{binary, inputPath}
	if opts.Quality > 0 && opts.Quality <= 100 {
		pdfArgs = append(pdfArgs, "-quality", fmt.Sprintf("%d", opts.Quality))
	}
	// An unavailable PDF coder can silently preserve the input format when
	// only the extension is supplied. Explicitly require PDF encoding.
	pdfArgs = append(pdfArgs, "PDF:"+outputPath)

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
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		log.Warn("imgpdf.skip.invalid_output", "bytes", len(pdf))
		return &Result{Skipped: true, StderrTail: "ImageMagick did not produce a PDF", Duration: dur}, nil
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
	case "image/heic", "image/heic-sequence":
		return "heic"
	case "image/heif", "image/heif-sequence":
		return "heif"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp", "image/x-bmp", "image/x-ms-bmp":
		return "bmp"
	}
	return "img"
}

func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(b[len(b)-n:]))
}
