// SPDX-License-Identifier: AGPL-3.0-or-later

// Package anydoc extracts EPUB and office documents through the anydoc CLI.
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

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipefile"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

const (
	// DefaultBinary is the CLI name expected on PATH. The Docker image
	// integration installs the anydoc CLI (see Dockerfile / follow-up
	// sourcing PR); bare-metal installs need to put it there manually.
	DefaultBinary = "anydoc"
	// DefaultMaxTextBytes matches EPUB's cap: office docs can be very
	// long. Overridden per-instance via ANYDOC_MAX_CONTENT_BYTES.
	DefaultMaxTextBytes = 32 * 1024 * 1024
	// HasTextThreshold matches djvu — small enough that a nearly-empty
	// doc still reports HasText=true; big enough that noise doesn't.
	HasTextThreshold = 32
)

// DefaultTimeout bounds each conversion. anydoc's own median is ~5ms
// per doc; 30s is a big safety margin. Overridable via
// SUCHI_ANYDOC_TIMEOUT.
// DefaultTimeout returns the effective per-invocation cap. Read at
// call time so a config file loaded from main.runServe reaches it.
func DefaultTimeout() time.Duration {
	return pipeconfig.Duration("SUCHI_ANYDOC_TIMEOUT", 30*time.Second)
}

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
		log.Warn("anydoc.skip.no_binary", "binary", binary)
		return &Result{Skipped: true, StderrTail: "anydoc not available"}, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout()
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
	if err := pipefile.WriteAll(inputPath, src); err != nil {
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

func countNonWhitespace(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(bytes.TrimSpace(b[len(b)-n:])))
}
