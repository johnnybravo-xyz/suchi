// Package qpdf is the PDF normalization pre-step.
//
// Every ingested PDF passes through qpdf --remove-restrictions --decrypt
// before it hits pdf-inspector or OCRmyPDF. The rewrite:
//
//   - strips owner-password / print / copy restrictions
//   - decrypts anything encrypted with an EMPTY user password
//   - lays the file out into a stable, tool-friendly form
//   - unbombs a decompression bomb inside qpdf's own memory instead
//     of ocrmypdf's, because qpdf has a hard --max-size ceiling
//     enforceable via the sandbox
//
// Runs in core/sandbox — empty env, no network, hard timeout, bounded
// output. If the qpdf binary is not on PATH, Normalize returns the
// original bytes with Skipped=true; the ingest pipeline continues
// with the untouched input. The design principle is: pre-processing
// improves quality but is never required for the pipeline to make
// forward progress.
package qpdf

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
	"time"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Default caps. The output cap is generous because legitimate PDFs
// routinely exceed the sandbox default; the timeout accounts for
// qpdf's linear-in-input cost on a busy VM.
const (
	DefaultBinary  = "qpdf"
	DefaultTimeout = 30 * time.Second
	DefaultMaxSize = 100 * 1024 * 1024 // 100 MiB
)

// Options carries the knobs. Zero-value fields use the Default* above.
type Options struct {
	Binary  string // resolved via PATH if not absolute
	Timeout time.Duration
	MaxSize int64
}

// Result carries the normalized bytes plus a diagnostic trail.
//
// Skipped=true means the binary was missing on this system OR the
// input wasn't recognized as a PDF that qpdf could rewrite. The
// caller should treat Data as identical to the input in that case
// — pipeline steps downstream still work, they just don't get the
// normalized form.
type Result struct {
	Data       []byte
	Skipped    bool
	StderrTail string
	Duration   time.Duration
}

// Normalize runs qpdf against src. Streams src to a tmpfile inside the
// sandbox's fresh working dir, invokes qpdf with the input path and
// stdout (`-`) as output, and returns the captured bytes.
//
// The read side of src is fully consumed before qpdf runs — we can't
// stream through qpdf because it seeks the input.
func Normalize(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	if src == nil {
		return nil, errors.New("qpdf: src is nil")
	}
	log = log.With("component", "qpdf")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		// Binary missing — return original bytes untouched.
		data, rerr := io.ReadAll(src)
		if rerr != nil {
			return nil, fmt.Errorf("read src: %w", rerr)
		}
		log.Info("qpdf.skip.no_binary", "binary", binary, "bytes", len(data))
		return &Result{Data: data, Skipped: true, StderrTail: "qpdf binary not on PATH"}, nil
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxSize := opts.MaxSize
	if maxSize == 0 {
		maxSize = DefaultMaxSize
	}

	// Persist src to a temp file so qpdf can seek it. Placed under the
	// sandbox dir the child sees (opts.Dir on sandbox.Opts). We create
	// it here and hand the sandbox a Dir arg, so sandbox does not
	// clobber it with its own MkdirTemp.
	dir, err := os.MkdirTemp("", "suchi-qpdf-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	if err := writeAll(inputPath, src); err != nil {
		return nil, err
	}

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		// --replace-input isn't safe with -; we take the more explicit
		// input + `-` output path.
		Args:      []string{binary, "--remove-restrictions", "--decrypt=", "--", "in.pdf", "-"},
		Timeout:   timeout,
		MaxStdout: maxSize,
		Dir:       dir,
	})
	if err != nil {
		// qpdf exits non-zero on unrecognized input (e.g. someone
		// uploaded a plain PNG). We treat that as "nothing to do" and
		// return the original bytes rather than fail ingest.
		if errors.Is(err, sandbox.ErrTimeout) {
			return nil, fmt.Errorf("qpdf timeout after %s: %s", res.Duration, tail(res.Stderr))
		}
		data, _ := os.ReadFile(inputPath)
		log.Info("qpdf.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Data: data, Skipped: true, StderrTail: tail(res.Stderr), Duration: res.Duration}, nil
	}

	if res.StdoutTruncated {
		return nil, fmt.Errorf("qpdf: output exceeded cap %d bytes", maxSize)
	}
	log.Debug("qpdf.done",
		"in_bytes", fileSize(inputPath),
		"out_bytes", len(res.Stdout),
		"took", time.Since(start).String(),
	)
	return &Result{Data: res.Stdout, StderrTail: tail(res.Stderr), Duration: res.Duration}, nil
}

// writeAll streams r into path. Overwrites on collision.
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

// tail returns the last few hundred bytes of stderr — enough to
// diagnose common failures without leaking large error dumps.
func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return string(bytes.TrimSpace(b))
	}
	return "..." + string(bytes.TrimSpace(b[len(b)-n:]))
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return fi.Size()
}
