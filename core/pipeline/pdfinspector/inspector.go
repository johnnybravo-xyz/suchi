// Package pdfinspector routes a PDF between the "text-native shortcut"
// and the "scanned → OCR" paths.
//
// The design doc's primary implementation is Firecrawl's pdf-inspector
// (Rust, per-page classification + confidence + Markdown extraction).
// Phase 2's first cut ships the pdftotext-based fallback because it's
// available on every Linux box the pipeline runs on today and lets us
// wire the routing decision end-to-end without waiting on the
// vendored-binary story.
//
// Contract:
//
//   - Extract(src) returns the pulled text and a HasText bool.
//   - HasText=true means the caller can skip OCR entirely and shove
//     Text straight into documents.content.
//   - HasText=false means "not enough text was pulled to trust" — the
//     caller enqueues a post-ocr job.
//   - Skipped=true means no extraction tool was found on the box; the
//     safe default is HasText=false → OCR everything.
//
// Threshold for HasText: 32 non-whitespace characters. Legitimate
// scanned PDFs sometimes carry a stray text layer (a cover-page
// timestamp, an XMP watermark), so a small floor beats a strict
// "is anything text-native" check.
//
// Real pdf-inspector integration lands in a follow-up commit when the
// binary is pinned + vendored into the Docker "full" image. The
// interface here is stable enough that swap is a matter of extending
// Extract() with a second exec.LookPath probe.
package pdfinspector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults. Timeout is overridable via SUCHI_PDFINSPECTOR_TIMEOUT.
const (
	DefaultBinary       = "pdftotext"
	DefaultMaxTextBytes = 8 * 1024 * 1024 // 8 MiB of extracted text
	HasTextThreshold    = 32              // non-whitespace chars to trust extraction
)

// DefaultTimeout returns the effective per-invocation cap. Read at
// call time so a config file loaded from main.runServe reaches it.
func DefaultTimeout() time.Duration {
	return pipeconfig.Duration("SUCHI_PDFINSPECTOR_TIMEOUT", 30*time.Second)
}

// Options carries per-call knobs. Zero-value uses the Default* above.
type Options struct {
	Binary       string // overrides pdftotext discovery
	Timeout      time.Duration
	MaxTextBytes int64
}

// Result is what Extract returns.
//
// Text is the whole document's plain text (line-preserving; pdftotext
// -layout). Bytes counter measured on the untrimmed output for
// downstream logging; HasText compares against the trimmed content.
type Result struct {
	Text       string
	HasText    bool
	Skipped    bool // no extractor available
	NonBlank   int  // count of non-whitespace runes in Text
	Duration   time.Duration
	StderrTail string
	SourceTool string // "pdftotext" today; "pdf-inspector" in the future
}

// Extract streams src into a tmpfile and runs the extractor against it.
// Returns Skipped=true (never an error) when no extractor is available
// so the pipeline continues.
func Extract(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	if src == nil {
		return nil, errors.New("pdfinspector: src is nil")
	}
	log = log.With("component", "pdf-inspector")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		// Drain src so upstream callers aren't left holding it.
		_, _ = io.Copy(io.Discard, src)
		log.Warn("pdf-inspector.skip.no_binary", "binary", binary)
		return &Result{Skipped: true, StderrTail: "no extractor available", SourceTool: binary}, nil
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout()
	}
	maxText := opts.MaxTextBytes
	if maxText == 0 {
		maxText = DefaultMaxTextBytes
	}

	dir, err := os.MkdirTemp("", "suchi-inspector-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	if err := writeAll(inputPath, src); err != nil {
		return nil, err
	}

	// pdftotext -layout in.pdf -   →  stdout
	args := []string{binary, "-layout", "in.pdf", "-"}

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      args,
		Timeout:   timeout,
		MaxStdout: maxText,
		Dir:       dir,
	})
	dur := time.Since(start)

	// A pdftotext non-zero exit on an unrecognized file is our "not a
	// PDF, or corrupted" signal. Treat as skip — the caller may still
	// try OCR on the raw bytes (an image, an office doc).
	if err != nil {
		if errors.Is(err, sandbox.ErrTimeout) {
			return nil, fmt.Errorf("pdf-inspector timeout after %s", res.Duration)
		}
		log.Info("pdf-inspector.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res.Stderr), Duration: dur, SourceTool: binary}, nil
	}
	if res.StdoutTruncated {
		return nil, fmt.Errorf("pdf-inspector: extracted text exceeded cap %d bytes", maxText)
	}

	text := string(res.Stdout)
	nonBlank := countNonWhitespace(text)
	r := &Result{
		Text:       text,
		HasText:    nonBlank >= HasTextThreshold,
		NonBlank:   nonBlank,
		Duration:   dur,
		StderrTail: tail(res.Stderr),
		SourceTool: binary,
	}
	log.Debug("pdf-inspector.done",
		"has_text", r.HasText, "non_blank", nonBlank, "took", dur.String())
	return r, nil
}

// countNonWhitespace is our "there's real text here" heuristic. Cheap;
// runs over the whole extracted string once.
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
