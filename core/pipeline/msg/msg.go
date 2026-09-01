// Package msg converts Outlook .msg files to RFC 822 bytes via msgconvert.
package msg

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

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipefile"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

const (
	DefaultBinary         = "msgconvert"
	DefaultTimeout        = 60 * time.Second
	DefaultMaxOutputBytes = 64 * 1024 * 1024
)

// Options carries per-call knobs. Zero-value uses Default* above.
type Options struct {
	Binary         string
	Timeout        time.Duration
	MaxOutputBytes int64
}

// Result carries the conversion outcome.
type Result struct {
	EML        []byte
	Skipped    bool // binary not present or input unparseable
	Duration   time.Duration
	StderrTail string
}

// Recognized reports whether mime is an Outlook .msg variant.
// application/vnd.ms-outlook is the IANA-registered type; the others
// are seen in the wild.
func Recognized(mime string) bool {
	m := strings.ToLower(mime)
	return m == "application/vnd.ms-outlook" ||
		m == "application/x-outlook-msg" ||
		m == "application/ms-outlook"
}

// Convert streams src through msgconvert. A missing binary or invalid input is
// a soft skip so the original remains available for later reprocessing.
func Convert(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "msg")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		_, _ = io.Copy(io.Discard, src)
		log.Warn("msg.skip.no_binary", "binary", binary)
		return &Result{Skipped: true, StderrTail: "msgconvert not available"}, nil
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxBytes := opts.MaxOutputBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxOutputBytes
	}

	dir, err := os.MkdirTemp("", "suchi-msg-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.msg")
	outputPath := filepath.Join(dir, "in.eml")
	if err := pipefile.WriteAll(inputPath, src); err != nil {
		return nil, err
	}

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:    []string{binary, "--outfile", outputPath, inputPath},
		Timeout: timeout,
		Dir:     dir,
	})
	dur := time.Since(start)
	if err != nil {
		log.Info("msg.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res.Stderr), Duration: dur}, nil
	}

	eml, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		return &Result{Skipped: true, StderrTail: readErr.Error(), Duration: dur}, nil
	}
	if int64(len(eml)) > maxBytes {
		log.Warn("msg.truncated_output", "bytes", len(eml), "cap", maxBytes)
		return &Result{Skipped: true,
			StderrTail: fmt.Sprintf("output %d bytes exceeded cap %d", len(eml), maxBytes),
			Duration:   dur,
		}, nil
	}

	return &Result{
		EML:        eml,
		Duration:   dur,
		StderrTail: tail(res.Stderr),
	}, nil
}

func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(b[len(b)-n:]))
}
