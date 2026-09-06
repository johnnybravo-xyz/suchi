// Package tessocr is the lightweight OCR path: pdftoppm rasterizes
// each page to a PGM, tesseract reads it back as text, we concatenate.
// No Python interpreter, no pikepdf, no Pillow — just two C binaries
// invoked in sequence.
//
// Trade-offs vs core/pipeline/ocrmypdf:
//
//   - **No searchable-PDF archive.** The caller writes documents.content
//     but archive_blob stays NULL. FTS + full-text search still work
//     because they index documents.content, not the archive.
//   - **No inferred rotation or perspective correction.** imgpdf honors
//     camera EXIF orientation. Empty pages get one sparse-text retry;
//     low-confidence text orientation never rotates the document.
//   - **~4× smaller Docker image.** ocrmypdf pulls Python + pikepdf +
//     Pillow + reportlab (~300 MB after deps). This path needs only
//     poppler-utils (~25 MB) + tesseract-ocr (~10 MB) + eng data (~10 MB).
//
// Contract mirrors ocrmypdf.OCR: missing binary → Skipped, non-zero exit
// → Skipped with StderrTail, timeout → error, output cap → error. Every
// subprocess runs through core/sandbox (empty env, bounded
// output, hard timeout).
package tessocr

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
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipefile"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults. Timeout is overridable via SUCHI_TESSERACT_TIMEOUT.
const (
	DefaultPdftoppm     = "pdftoppm"
	DefaultTesseract    = "tesseract"
	DefaultRasterDPI    = 300 // matches ocrmypdf's default
	DefaultLanguages    = "eng"
	DefaultMaxTextBytes = 8 * 1024 * 1024 // 8 MiB, same cap as ocrmypdf's sidecar
)

// DefaultTimeout returns the effective per-invocation cap. Read at
// call time so a config file loaded from main.runServe reaches it.
func DefaultTimeout() time.Duration {
	return pipeconfig.Duration("SUCHI_TESSERACT_TIMEOUT", 10*time.Minute)
}

// Options carries per-call knobs.
type Options struct {
	Pdftoppm  string
	Tesseract string

	Languages    []string      // tesseract -l codes; default ["eng"]
	Timeout      time.Duration // whole-chain
	RasterDPI    int
	MaxTextBytes int64
}

// Result mirrors ocrmypdf.Result minus ArchivePDF — tessocr never
// produces a searchable-PDF archive.
type Result struct {
	Text       string
	Skipped    bool
	StderrTail string
	Pages      int // number of pages rasterized + OCR'd
	Duration   time.Duration
}

// Available reports whether both binaries are on PATH. Callers use
// this to decide between ocrmypdf and tessocr at engine dispatch time.
func Available() bool {
	if _, err := exec.LookPath(DefaultPdftoppm); err != nil {
		return false
	}
	if _, err := exec.LookPath(DefaultTesseract); err != nil {
		return false
	}
	return true
}

// OCR runs the pdftoppm → tesseract chain against src.
func OCR(ctx context.Context, src io.Reader, log *slog.Logger, opts Options) (*Result, error) {
	if src == nil {
		return nil, errors.New("tessocr: src is nil")
	}
	log = log.With("component", "tessocr")

	pdftoppm := opts.Pdftoppm
	if pdftoppm == "" {
		pdftoppm = DefaultPdftoppm
	}
	tesseract := opts.Tesseract
	if tesseract == "" {
		tesseract = DefaultTesseract
	}
	if _, err := exec.LookPath(pdftoppm); err != nil {
		_, _ = io.Copy(io.Discard, src)
		log.Info("tessocr.skip.no_pdftoppm")
		return &Result{Skipped: true, StderrTail: "pdftoppm not available"}, nil
	}
	if _, err := exec.LookPath(tesseract); err != nil {
		_, _ = io.Copy(io.Discard, src)
		log.Info("tessocr.skip.no_tesseract")
		return &Result{Skipped: true, StderrTail: "tesseract not available"}, nil
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dpi := opts.RasterDPI
	if dpi == 0 {
		dpi = DefaultRasterDPI
	}
	langs := opts.Languages
	if len(langs) == 0 {
		langs = []string{DefaultLanguages}
	}
	maxBytes := opts.MaxTextBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxTextBytes
	}

	dir, err := os.MkdirTemp("", "suchi-tessocr-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	if err := pipefile.WriteAll(inputPath, src); err != nil {
		return nil, err
	}

	start := time.Now()

	// 1) Rasterize: pdftoppm -r <dpi> -gray in.pdf <dir>/p
	//    → p-1.pgm, p-2.pgm, ...
	prefix := filepath.Join(dir, "p")
	rasterRes, err := sandbox.Run(ctx, sandbox.Opts{
		Args: []string{
			pdftoppm, "-r", fmt.Sprintf("%d", dpi), "-gray",
			inputPath, prefix,
		},
		Timeout:   timeout,
		MaxStdout: 1024, // pdftoppm writes to disk, not stdout — keep small
		Dir:       dir,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tessocr rasterize: %w", ctx.Err())
		}
		log.Info("tessocr.skip.pdftoppm_failed",
			"exit", rasterRes.ExitCode, "stderr", tail(rasterRes.Stderr))
		return &Result{Skipped: true, StderrTail: tail(rasterRes.Stderr), Duration: time.Since(start)}, nil
	}

	pages, err := listPGMs(dir)
	if err != nil {
		return nil, fmt.Errorf("list rasterized pages: %w", err)
	}
	if len(pages) == 0 {
		log.Info("tessocr.skip.no_pages")
		return &Result{Skipped: true, StderrTail: "pdftoppm produced no pages", Duration: time.Since(start)}, nil
	}

	// 2) OCR each page. Concatenate into buf, respecting maxBytes.
	//    Deadline is shared with rasterize so a slow document doesn't
	//    silently exceed the whole-chain budget.
	langArg := strings.Join(langs, "+")
	var (
		buf     bytes.Buffer
		lastErr string
	)
	for i, p := range pages {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tessocr page %d: %w", i+1, ctx.Err())
		}
		if int64(buf.Len()) >= maxBytes {
			return nil, fmt.Errorf("tessocr: text exceeded cap of %d bytes", maxBytes)
		}
		res, err := sandbox.Run(ctx, sandbox.Opts{
			Args:      []string{tesseract, p, "stdout", "-l", langArg},
			Timeout:   timeout,
			MaxStdout: maxBytes - int64(buf.Len()) + 1,
			Dir:       dir,
		})
		// Automatic page segmentation can miss isolated labels completely.
		// Retry only an empty successful page, preserving all existing text.
		if err == nil && len(bytes.TrimSpace(res.Stdout)) == 0 {
			log.Info("tessocr.page_retry_sparse", "page", i+1)
			res, err = sandbox.Run(ctx, sandbox.Opts{
				Args:      []string{tesseract, p, "stdout", "-l", langArg, "--psm", "11"},
				Timeout:   timeout,
				MaxStdout: maxBytes - int64(buf.Len()) + 1,
				Dir:       dir,
			})
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("tessocr page %d: %w", i+1, ctx.Err())
			}
			lastErr = tail(res.Stderr)
			log.Info("tessocr.page_failed",
				"page", i+1, "exit", res.ExitCode, "stderr", lastErr)
			continue // one bad page shouldn't kill the whole document
		}
		separatorBytes := int64(0)
		if buf.Len() > 0 {
			separatorBytes = 1
		}
		if res.StdoutTruncated || int64(buf.Len()+len(res.Stdout))+separatorBytes > maxBytes {
			return nil, fmt.Errorf("tessocr: text exceeded cap of %d bytes", maxBytes)
		}
		if separatorBytes != 0 {
			buf.WriteString("\n")
		}
		buf.Write(res.Stdout)
	}

	text := strings.TrimSpace(buf.String())
	dur := time.Since(start)

	// If every page failed we treat it as skipped rather than an empty
	// success — same effective behaviour as ocrmypdf on total failure.
	if text == "" && lastErr != "" {
		return &Result{Skipped: true, StderrTail: lastErr, Pages: len(pages), Duration: dur}, nil
	}
	log.Info("tessocr.done", "pages", len(pages), "chars", countNonWhitespace(text), "took", dur.String())
	return &Result{
		Text:       text,
		Skipped:    false,
		StderrTail: lastErr,
		Pages:      len(pages),
		Duration:   dur,
	}, nil
}

// listPGMs returns pdftoppm's output files in reading order. pdftoppm
// zero-pads only when it thinks it needs to, so "p-1.pgm" and "p-10.pgm"
// can coexist — sort by the numeric suffix, not lexically.
func listPGMs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type indexed struct {
		path string
		n    int
	}
	var out []indexed
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".pgm") {
			continue
		}
		// name looks like "p-<N>.pgm"
		base := strings.TrimSuffix(name, ".pgm")
		i := strings.LastIndex(base, "-")
		if i < 0 {
			continue
		}
		n := 0
		for _, r := range base[i+1:] {
			if r < '0' || r > '9' {
				n = -1
				break
			}
			n = n*10 + int(r-'0')
		}
		if n < 0 {
			continue
		}
		out = append(out, indexed{path: filepath.Join(dir, name), n: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	paths := make([]string, len(out))
	for i, x := range out {
		paths[i] = x.path
	}
	return paths, nil
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
