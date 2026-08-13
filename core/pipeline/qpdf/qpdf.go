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
	"strings"
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

	// Passwords carries candidate user passwords to try if the empty
	// password fails. The empty password is always tried first (it's
	// the common owner-password-restrictions case), so this list only
	// covers PDFs with a real user password. Order matters — hot
	// passwords should come first for the fastest fanout.
	Passwords []string
}

// Result carries the normalized bytes plus a diagnostic trail.
//
// Skipped=true means the binary was missing on this system OR the
// input wasn't recognized as a PDF that qpdf could rewrite. The
// caller should treat Data as identical to the input in that case
// — pipeline steps downstream still work, they just don't get the
// normalized form.
//
// NeedsPassword=true is a distinct failure mode: the input IS a PDF
// but qpdf couldn't unlock it with any of Options.Passwords + the
// empty password. Data holds the original encrypted bytes; the
// caller should stash the doc in encryption_state='encrypted' and
// wait for an operator-supplied password.
//
// PasswordIndex reports which slot succeeded:
//
//	-1 → no decryption needed OR empty password sufficed
//	 k → Options.Passwords[k] worked
type Result struct {
	Data          []byte
	Skipped       bool
	NeedsPassword bool
	StderrTail    string
	Duration      time.Duration
	PasswordIndex int
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
		log.Warn("qpdf.skip.no_binary", "binary", binary, "bytes", len(data))
		return &Result{
			Data: data, Skipped: true,
			StderrTail:    "qpdf binary not on PATH",
			PasswordIndex: -1,
		}, nil
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
	// Try empty password first (the common owner-restrictions case),
	// then each candidate in order. First success wins.
	//
	// qpdf exit codes we care about:
	//   0 → clean success
	//   3 → success with warnings (banks that ship spec-nonconformant
	//       /Perms are the recurring one — decrypt did work). We
	//       treat 3 as success as long as stdout is non-empty.
	//   2 → real error — wrong password OR corrupt PDF OR not-a-PDF.
	//       Sniff stderr to decide which.
	tryDecrypt := func(password string) (*sandbox.Result, bool, bool, error) {
		res, err := sandbox.Run(ctx, sandbox.Opts{
			Args: []string{
				binary, "--remove-restrictions", "--password=" + password,
				"--decrypt", "--", "in.pdf", "-",
			},
			Timeout:   timeout,
			MaxStdout: maxSize,
			Dir:       dir,
		})
		// warnings-with-output is a success shape for us.
		if err != nil && res != nil && res.ExitCode == 3 && len(res.Stdout) > 0 {
			return res, false, true, nil
		}
		return res, isPasswordError(res.Stderr), false, err
	}

	res, passwordErr, hadWarnings, err := tryDecrypt("")
	usedIndex := -1
	if err != nil {
		if errors.Is(err, sandbox.ErrTimeout) {
			return nil, fmt.Errorf("qpdf timeout after %s: %s", res.Duration, tail(res.Stderr))
		}
		if passwordErr && len(opts.Passwords) > 0 {
			for i, pw := range opts.Passwords {
				res, passwordErr, hadWarnings, err = tryDecrypt(pw)
				if err == nil {
					usedIndex = i
					break
				}
				if !passwordErr || errors.Is(err, sandbox.ErrTimeout) {
					break
				}
			}
		}
	}
	if err != nil {
		if errors.Is(err, sandbox.ErrTimeout) {
			return nil, fmt.Errorf("qpdf timeout after %s: %s", res.Duration, tail(res.Stderr))
		}
		data, _ := os.ReadFile(inputPath)
		if passwordErr {
			log.Info("qpdf.needs_password",
				"tried_candidates", len(opts.Passwords),
				"stderr", tail(res.Stderr))
			return &Result{
				Data:          data,
				NeedsPassword: true,
				StderrTail:    tail(res.Stderr),
				Duration:      res.Duration,
				PasswordIndex: -1,
			}, nil
		}
		log.Info("qpdf.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{
			Data: data, Skipped: true,
			StderrTail: tail(res.Stderr), Duration: res.Duration,
			PasswordIndex: -1,
		}, nil
	}

	if res.StdoutTruncated {
		return nil, fmt.Errorf("qpdf: output exceeded cap %d bytes", maxSize)
	}
	if hadWarnings {
		log.Info("qpdf.done.with_warnings",
			"stderr_tail", tail(res.Stderr),
			"password_index", usedIndex)
	}
	log.Debug("qpdf.done",
		"in_bytes", fileSize(inputPath),
		"out_bytes", len(res.Stdout),
		"took", time.Since(start).String(),
		"password_index", usedIndex,
	)
	return &Result{
		Data:          res.Stdout,
		StderrTail:    tail(res.Stderr),
		Duration:      res.Duration,
		PasswordIndex: usedIndex,
	}, nil
}

// isPasswordError sniffs qpdf's stderr for markers that specifically
// mean "the input is encrypted and the provided password (or empty
// password) is wrong". qpdf writes "invalid password" for a wrong
// password and mentions "encrypted" / "user password" in various
// permission contexts. False positives here just make us try more
// candidates than needed; false negatives leak a would-be-decryptable
// doc into the "unrecognized input" bucket. Neither is catastrophic.
func isPasswordError(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "invalid password") ||
		strings.Contains(s, "password is not correct") ||
		strings.Contains(s, "cannot open encrypted") ||
		strings.Contains(s, "requires a password")
}

// SelectPages returns pdfBytes with only the pages listed in `pages`
// (1-indexed, ascending) kept. Empty or nil pages is a no-op (returns
// the input verbatim). Missing binary → Skipped=true, input passed
// through unchanged so callers never lose data.
//
// Uses qpdf's --pages selector: `qpdf --pages in.pdf 1,3-5 -- out.pdf`.
// The selector is built from `pages` deterministically (contiguous
// runs collapse into "3-5" ranges) so a re-run against the same input
// produces identical bytes — Byte-equal, dedup-friendly.
func SelectPages(ctx context.Context, pdfBytes []byte, pages []int, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "qpdf.select-pages")
	if len(pages) == 0 {
		return &Result{Data: pdfBytes, Skipped: true, StderrTail: "no pages to select"}, nil
	}

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		log.Warn("qpdf.select-pages.skip.no_binary", "binary", binary)
		return &Result{Data: pdfBytes, Skipped: true, StderrTail: "qpdf binary not on PATH"}, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxSize := opts.MaxSize
	if maxSize == 0 {
		maxSize = DefaultMaxSize
	}

	dir, err := os.MkdirTemp("", "suchi-qpdf-select-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inputPath, pdfBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write input: %w", err)
	}

	selector := buildPageRanges(pages)
	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      []string{binary, "in.pdf", "--pages", ".", selector, "--", "-"},
		Timeout:   timeout,
		MaxStdout: maxSize,
		Dir:       dir,
	})
	if err != nil {
		if errors.Is(err, sandbox.ErrTimeout) {
			return nil, fmt.Errorf("qpdf select-pages timeout after %s", res.Duration)
		}
		log.Info("qpdf.select-pages.skip.exit_nonzero",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{Data: pdfBytes, Skipped: true, StderrTail: tail(res.Stderr), Duration: res.Duration}, nil
	}
	if res.StdoutTruncated {
		return nil, fmt.Errorf("qpdf select-pages: output exceeded cap %d bytes", maxSize)
	}
	return &Result{Data: res.Stdout, Duration: time.Since(start)}, nil
}

// buildPageRanges collapses [1,2,3,5,7,8] → "1-3,5,7-8". Assumes input
// is sorted ascending with no duplicates (caller's contract).
func buildPageRanges(pages []int) string {
	var parts []string
	i := 0
	for i < len(pages) {
		start := pages[i]
		end := start
		for i+1 < len(pages) && pages[i+1] == end+1 {
			end++
			i++
		}
		if start == end {
			parts = append(parts, fmt.Sprintf("%d", start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, end))
		}
		i++
	}
	return strings.Join(parts, ",")
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
