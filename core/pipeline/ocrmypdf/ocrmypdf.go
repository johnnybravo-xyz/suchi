// Package ocrmypdf wraps the ocrmypdf CLI for the ingest pipeline.
//
// The post-ingest handler routes here when pdf-inspector says
// HasText=false — the PDF is scanned or its embedded text isn't
// trustworthy enough to skip OCR. OCRmyPDF rewrites the PDF with
// searchable text layers, and dumps a plain-text sidecar we write
// straight into documents.content.
//
// Contract (see also core/pipeline/qpdf and core/pipeline/pdfinspector,
// which follow the same shape):
//
//   - Binary missing on the box → Skipped=true, no error. The caller
//     leaves documents.archive_blob NULL and documents.content ”.
//   - Non-zero exit             → Skipped=true, StderrTail carries the
//     error. Ingest continues so later classifiers can still run.
//   - Timeout                   → error. OCR runs are long by design
//     (default 10 min); a timeout means the file is pathological.
//   - Output cap exceeded       → error. Decompression / bomb defense.
//
// Runs in core/sandbox — empty env, no network, hard timeout, output
// capped, fresh workdir. Same posture as qpdf + pdf-inspector; every
// hostile-input tool goes through the same wrapper.
package ocrmypdf

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

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipefile"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults. OCR is slow — the timeout is minutes, not seconds. Output
// cap sized for a full-image full-scan document. Timeout is
// overridable via SUCHI_OCRMYPDF_TIMEOUT, searchable-PDF output cap
// via SUCHI_OCRMYPDF_MAX_ARCHIVE. Sidecar text cap is driven by
// PDF_MAX_CONTENT_BYTES at the caller site (postingest).
const (
	DefaultBinary    = "ocrmypdf"
	DefaultMaxText   = 8 * 1024 * 1024 // 8 MiB sidecar text (caller-overridden)
	DefaultLanguages = "eng"
)

// Overrides read at call time so a config file loaded from
// main.runServe reaches them — a package-init `var = pipeconfig.…(…)`
// would capture env BEFORE LoadFile ran.
func DefaultTimeout() time.Duration {
	return pipeconfig.Duration("SUCHI_OCRMYPDF_TIMEOUT", 10*time.Minute)
}
func DefaultMaxArchive() int64 {
	return pipeconfig.Bytes("SUCHI_OCRMYPDF_MAX_ARCHIVE", 200*1024*1024)
}

// Options carries per-call knobs.
type Options struct {
	Binary     string
	Languages  []string // tesseract langs; joined with '+' for ocrmypdf --language
	Timeout    time.Duration
	MaxArchive int64
	MaxText    int64
	// Args appended verbatim to the ocrmypdf command line; escape hatch
	// for operators who need --rotate-pages, --clean, etc. without a
	// code change.
	ExtraArgs []string
}

// Result carries the OCR output.
type Result struct {
	ArchivePDF []byte // rewritten PDF with searchable text — the "archive" blob
	Text       string // plain-text sidecar — goes into documents.content
	Skipped    bool
	StderrTail string
	Duration   time.Duration
}

// OCR runs ocrmypdf against src. Streams src to a tmpfile, invokes
// ocrmypdf with a sidecar output, then reads both the rewritten PDF
// and the sidecar text back.
func OCR(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	if src == nil {
		return nil, errors.New("ocrmypdf: src is nil")
	}
	log = log.With("component", "ocrmypdf")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		_, _ = io.Copy(io.Discard, src)
		log.Warn("ocrmypdf.skip.no_binary", "binary", binary)
		return &Result{Skipped: true, StderrTail: "ocrmypdf binary not on PATH"}, nil
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout()
	}
	maxArchive := opts.MaxArchive
	if maxArchive == 0 {
		maxArchive = DefaultMaxArchive()
	}
	maxText := opts.MaxText
	if maxText == 0 {
		maxText = DefaultMaxText
	}
	langs := opts.Languages
	if len(langs) == 0 {
		langs = []string{DefaultLanguages}
	}

	dir, err := os.MkdirTemp("", "suchi-ocr-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	outputPath := filepath.Join(dir, "out.pdf")
	sidecarPath := filepath.Join(dir, "text.txt")

	if err := pipefile.WriteAll(inputPath, src); err != nil {
		return nil, err
	}

	args := []string{
		binary,
		"--language", strings.Join(langs, "+"),
		"--sidecar", "text.txt",
		"--quiet",
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, "in.pdf", "out.pdf")

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:    args,
		Timeout: timeout,
		// stdout is small (--quiet), stderr carries diagnostics — bound
		// both modestly.
		MaxStdout: 512 * 1024,
		MaxStderr: 512 * 1024,
		Dir:       dir,
	})
	dur := time.Since(start)

	if err != nil {
		if errors.Is(err, sandbox.ErrTimeout) {
			return nil, fmt.Errorf("ocrmypdf timeout after %s", dur)
		}
		log.Info("ocrmypdf.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Skipped: true, StderrTail: tail(res.Stderr), Duration: dur}, nil
	}

	// Read output PDF, capping at maxArchive.
	archive, err := readCapped(outputPath, maxArchive)
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	// Read sidecar text, capping at maxText. Absent sidecar is not fatal —
	// ocrmypdf sometimes skips it on already-OCR'd input.
	text, err := readCapped(sidecarPath, maxText)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read sidecar: %w", err)
	}

	log.Debug("ocrmypdf.done",
		"in_bytes", fileSize(inputPath),
		"archive_bytes", len(archive),
		"text_bytes", len(text),
		"took", dur.String())
	return &Result{
		ArchivePDF: archive,
		Text:       string(text),
		StderrTail: tail(res.Stderr),
		Duration:   dur,
	}, nil
}

// readCapped reads path, refusing to return more than max bytes. The
// caller decides whether an oversize file is an error or a truncation.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limited := &io.LimitedReader{R: f, N: max + 1}
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeded %d bytes", path, max)
	}
	return b, nil
}

func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(bytes.TrimSpace(b[len(b)-n:])))
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return fi.Size()
}
