// Package heic converts HEIC / HEIF images to PDF via ImageMagick.
//
// iPhones save photos as HEIC by default. Users routinely photograph
// receipts, ID cards, tax forms, etc. — a DMS that can't ingest HEIC
// silently loses that whole class of source. Shelling out to
// ImageMagick keeps the slim image ~15 MB heavier and CGO-free
// (a libheif binding would need CGO).
//
// Output is a single-page PDF sized to the input image. Post-ingest
// then feeds that PDF to the standard OCR path so a photograph of a
// receipt still becomes full-text searchable — that's the whole point
// vs. keeping it as an image.
package heic

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
	// generous even for a 48 MP HEIC — real-world photos of documents
	// land in the low single-digit MB range.
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
}

// Result carries the conversion outcome.
type Result struct {
	PDF        []byte
	Skipped    bool // binary not present or input unreadable
	Duration   time.Duration
	StderrTail string
}

// Recognized reports whether mime is a HEIC / HEIF variant we can
// convert. iOS also produces `image/heic-sequence` for Live Photos —
// ImageMagick renders those as their first frame (the still).
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	return m == "image/heic" ||
		m == "image/heif" ||
		m == "image/heic-sequence" ||
		m == "image/heif-sequence"
}

// Available reports whether an ImageMagick binary is on PATH. Cheap
// LookPath; safe to call at boot for the doctor summary.
func Available() bool {
	if _, err := exec.LookPath(DefaultBinary); err == nil {
		return true
	}
	if _, err := exec.LookPath(FallbackBinary); err == nil {
		return true
	}
	return false
}

// Convert streams the HEIC bytes to a tempfile and runs ImageMagick to
// produce a PDF. Skipped=true when no ImageMagick is installed — safe
// caller behavior is "keep the HEIC as-is; someone can install magick
// later and re-run".
func Convert(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "heic")

	binary := opts.Binary
	if binary == "" {
		if _, err := exec.LookPath(DefaultBinary); err == nil {
			binary = DefaultBinary
		} else if _, err := exec.LookPath(FallbackBinary); err == nil {
			binary = FallbackBinary
		} else {
			_, _ = io.Copy(io.Discard, src)
			log.Info("heic.skip.no_binary", "binary", DefaultBinary)
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

	dir, err := os.MkdirTemp("", "suchi-heic-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.heic")
	interPath := filepath.Join(dir, "step.png")
	outputPath := filepath.Join(dir, "out.pdf")
	if err := writeAll(inputPath, src); err != nil {
		return nil, err
	}

	// Two-step conversion: HEIC → PNG → PDF. Alpine's ImageMagick
	// fails the single-call HEIC→PDF with a misleading "no encode
	// delegate for HEIC" error (it's really the PDF encoder faltering
	// on a HEIC-derived raster). Splitting through PNG works on both
	// Alpine and Debian.
	start := time.Now()
	res1, err := sandbox.Run(ctx, sandbox.Opts{
		Args:    []string{binary, inputPath, interPath},
		Timeout: timeout,
		Dir:     dir,
	})
	if err != nil {
		dur := time.Since(start)
		log.Info("heic.skip.exit_nonzero.decode",
			"exit", res1.ExitCode, "stderr", tail(res1.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res1.Stderr), Duration: dur}, nil
	}

	pdfArgs := []string{binary, interPath}
	if opts.Quality > 0 && opts.Quality <= 100 {
		pdfArgs = append(pdfArgs, "-quality", fmt.Sprintf("%d", opts.Quality))
	}
	pdfArgs = append(pdfArgs, outputPath)
	res2, err := sandbox.Run(ctx, sandbox.Opts{
		Args:    pdfArgs,
		Timeout: timeout,
		Dir:     dir,
	})
	dur := time.Since(start)
	if err != nil {
		log.Info("heic.skip.exit_nonzero.encode",
			"exit", res2.ExitCode, "stderr", tail(res2.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res2.Stderr), Duration: dur}, nil
	}

	pdf, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		return &Result{Skipped: true, StderrTail: readErr.Error(), Duration: dur}, nil
	}
	if int64(len(pdf)) > maxBytes {
		log.Warn("heic.truncated_output", "bytes", len(pdf), "cap", maxBytes)
		return &Result{Skipped: true,
			StderrTail: fmt.Sprintf("output %d bytes exceeded cap %d", len(pdf), maxBytes),
			Duration:   dur,
		}, nil
	}

	return &Result{
		PDF:        pdf,
		Duration:   dur,
		StderrTail: tail(res2.Stderr),
	}, nil
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
