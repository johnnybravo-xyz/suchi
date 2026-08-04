// Package djvu extracts text from DjVu files via djvutxt (from
// djvulibre-bin on Debian). Same shape as pdfinspector — degrades
// gracefully when the binary isn't installed, returns HasText true only
// when we pulled enough content to trust.
//
// DjVu is common in scanned-book archives (archive.org, university
// scans). Files carry a searchable text layer when the source was
// OCR-processed; djvutxt streams it out as UTF-8 text.
package djvu

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
	DefaultBinary       = "djvutxt"
	DefaultTimeout      = 30 * time.Second
	DefaultMaxTextBytes = 8 * 1024 * 1024
	HasTextThreshold    = 32
)

// Options carries per-call knobs. Zero-value uses Default* above.
type Options struct {
	Binary       string
	Timeout      time.Duration
	MaxTextBytes int64
}

// Result mirrors pdfinspector.Result for a familiar caller contract.
type Result struct {
	Text       string
	HasText    bool
	Skipped    bool
	NonBlank   int
	Duration   time.Duration
	StderrTail string
}

// Recognized reports whether mime looks like DjVu.
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	return m == "image/vnd.djvu" || m == "image/x-djvu" || m == "image/djvu"
}

// Extract streams src to a tempfile and runs djvutxt against it.
// Skipped=true when the binary is missing — safe caller behavior is
// "leave content empty; someone can ingest via OCR later once tools
// are installed".
func Extract(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "djvu")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		_, _ = io.Copy(io.Discard, src)
		log.Info("djvu.skip.no_binary", "binary", binary)
		return &Result{Skipped: true, StderrTail: "djvutxt not available"}, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxText := opts.MaxTextBytes
	if maxText == 0 {
		maxText = DefaultMaxTextBytes
	}

	dir, err := os.MkdirTemp("", "suchi-djvu-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.djvu")
	if err := writeAll(inputPath, src); err != nil {
		return nil, err
	}

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      []string{binary, inputPath},
		Timeout:   timeout,
		MaxStdout: maxText,
		Dir:       dir,
	})
	dur := time.Since(start)
	if err != nil {
		log.Info("djvu.skip.exit_nonzero", "exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res.Stderr), Duration: dur}, nil
	}
	if res.StdoutTruncated {
		return nil, fmt.Errorf("djvu: extracted text exceeded cap %d bytes", maxText)
	}

	text := string(res.Stdout)
	nonBlank := countNonWhitespace(text)
	return &Result{
		Text:       text,
		HasText:    nonBlank >= HasTextThreshold,
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
