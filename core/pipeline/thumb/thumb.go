// Package thumb renders a document's first-page thumbnail via
// pdftoppm.
//
// Design decisions:
//
//   - PNG output, straight from pdftoppm's `-png` flag. No Go-side
//     re-encode: pdftoppm's rendering is already at a chosen DPI, so
//     the file size is bounded by the DPI, not the source PDF's
//     page dimensions.
//   - DPI 40 by default — for US-letter that's ~340x440 pixels, a
//     natural "thumbnail" size without a Go image-resize step. An
//     operator can override via Options.DPI if they want sharper.
//   - Missing pdftoppm on PATH is a Skipped=true return, not an
//     error. Ingest never fails because a thumbnail didn't render.
//   - No batching, no page selection beyond page 1. Multi-page
//     preview lives in the UI's PDF viewer, not here.
//
// The pdftoppm process is sandboxed (see core/sandbox) with a 30s
// timeout and stdout cap — the binary writes the PNG to a temp file,
// stdout only carries progress noise.

package thumb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults.
const (
	DefaultBinary  = "pdftoppm"
	DefaultTimeout = 30 * time.Second
	DefaultDPI     = 40
)

// Options carries per-call knobs. Zero values mean defaults.
type Options struct {
	Binary  string
	Timeout time.Duration
	DPI     int
}

// Result is the rendered thumbnail, or Skipped=true when pdftoppm is
// missing.
type Result struct {
	PNG     []byte
	Skipped bool
}

// Render rasterizes page 1 of pdfBytes into a PNG at the configured
// DPI. Returns Skipped=true when pdftoppm isn't on PATH (bare install /
// pre-Docker setup); the caller treats that as "no thumb, that's OK."
func Render(ctx context.Context, pdfBytes []byte, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "thumb")
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		log.Warn("thumb.skip.no_binary", "binary", binary)
		return &Result{Skipped: true}, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	dpi := opts.DPI
	if dpi == 0 {
		dpi = DefaultDPI
	}

	dir, err := os.MkdirTemp("", "suchi-thumb-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inputPath, pdfBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write input: %w", err)
	}
	prefix := filepath.Join(dir, "thumb")

	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args: []string{
			binary, "-r", strconv.Itoa(dpi), "-f", "1", "-l", "1", "-png",
			inputPath, prefix,
		},
		Timeout:   timeout,
		MaxStdout: 4096,
		Dir:       dir,
	})
	if err != nil {
		return nil, fmt.Errorf("pdftoppm: exit %d: %w", res.ExitCode, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("pdftoppm exit %d: %s", res.ExitCode, tail(res.Stderr))
	}

	// pdftoppm writes <prefix>-<page>.png. For single-page renders it
	// varies: newer pdftoppm writes "thumb-1.png", older ones drop the
	// suffix. Try both.
	candidates := []string{
		prefix + "-1.png",
		prefix + "-01.png",
		prefix + ".png",
	}
	for _, path := range candidates {
		png, err := os.ReadFile(path)
		if err == nil {
			return &Result{PNG: png}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read output %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("pdftoppm produced no output under %s-*.png", prefix)
}

// tail returns the last ~512 bytes of the stderr buffer — enough for
// the interesting portion of pdftoppm's log without carrying the
// whole binary line home.
func tail(b []byte) string {
	if len(b) <= 512 {
		return string(b)
	}
	return "…" + string(b[len(b)-512:])
}
