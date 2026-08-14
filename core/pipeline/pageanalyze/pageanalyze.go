// Package pageanalyze rasterizes a PDF's pages to greyscale and
// computes a per-page whiteness score. Its one job is to distinguish
// blank pages (feeder back-sides, separator sheets left in a stack)
// from pages that carry actual content.
//
// Uses pdftoppm at low DPI — the analysis is a fast pre-flight, not
// a rendering path. A 100-page scan analyzed at 50 DPI is well under
// a second on modest hardware.
//
// The output feeds two callers today:
//
//   - Blank-page removal — post-ingest drops pages flagged IsBlank
//     from the archive PDF via qpdf --pages, before the OCR path
//     produces documents.content. Original blob stays verbatim.
//   - Future: multi-doc splitting — the same analysis will surface
//     separator sheets (QR-tagged) for a later feature.
//
// Contract:
//
//   - Missing pdftoppm on PATH → returns Skipped=true, no error;
//     caller treats pages as "unknown" and proceeds without trimming.
//   - Any qpdf/pdftoppm failure → error; caller falls back to using
//     the untrimmed input.
//   - Threshold defaults to 0.995 (99.5% white). Tune via
//     Options.WhitenessThreshold when you have a scanner that leaves
//     grey backgrounds.
package pageanalyze

import (
	"bufio"
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
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults. Timeout is overridable via SUCHI_PAGEANALYZE_TIMEOUT.
const (
	DefaultBinary             = "pdftoppm"
	DefaultDPI                = 50    // low; we only care about intensity
	DefaultWhitenessThreshold = 0.995 // mean pixel value / 255
)

// DefaultTimeout returns the effective per-invocation cap. Read at
// call time so a config file loaded from main.runServe reaches it.
func DefaultTimeout() time.Duration {
	return pipeconfig.Duration("SUCHI_PAGEANALYZE_TIMEOUT", 60*time.Second)
}

// Options carries per-call knobs.
type Options struct {
	Binary             string
	Timeout            time.Duration
	DPI                int
	WhitenessThreshold float64
}

// PageInfo summarises one page's whiteness metric.
type PageInfo struct {
	Number    int     // 1-indexed
	MeanWhite float64 // 0.0 (all black) → 1.0 (all white)
	IsBlank   bool
}

// Result is the analysis output. Pages is 1-indexed by PDF page order.
// Skipped=true when the binary is missing — Pages is nil in that case.
type Result struct {
	Pages   []PageInfo
	Skipped bool
	// NonBlank is the list of 1-indexed page numbers with IsBlank=false.
	// Convenience for the caller building a qpdf --pages range spec.
	NonBlank []int
}

// Analyze rasterizes pdfBytes and returns per-page whiteness scores.
// Idempotent, temp-directory-scoped, no network.
func Analyze(ctx context.Context, pdfBytes []byte, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "pageanalyze")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		log.Warn("pageanalyze.skip.no_binary", "binary", binary)
		return &Result{Skipped: true}, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout()
	}
	dpi := opts.DPI
	if dpi == 0 {
		dpi = DefaultDPI
	}
	threshold := opts.WhitenessThreshold
	if threshold == 0 {
		threshold = DefaultWhitenessThreshold
	}

	dir, err := os.MkdirTemp("", "suchi-pageanalyze-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inputPath, pdfBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write input: %w", err)
	}
	prefix := filepath.Join(dir, "p")

	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args: []string{
			binary, "-r", strconv.Itoa(dpi), "-gray",
			inputPath, prefix,
		},
		Timeout:   timeout,
		MaxStdout: 1024, // pdftoppm writes to disk, stdout is tiny
		Dir:       dir,
	})
	if err != nil {
		return nil, fmt.Errorf("pdftoppm: exit %d: %s", res.ExitCode, tail(res.Stderr))
	}

	pages, err := listPGMs(dir)
	if err != nil {
		return nil, fmt.Errorf("list rasterized pages: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("pageanalyze: no pages produced")
	}

	out := &Result{Pages: make([]PageInfo, 0, len(pages))}
	for _, p := range pages {
		mw, err := meanWhitePGM(p.path)
		if err != nil {
			return nil, fmt.Errorf("mean %s: %w", p.path, err)
		}
		info := PageInfo{Number: p.n, MeanWhite: mw, IsBlank: mw >= threshold}
		out.Pages = append(out.Pages, info)
		if !info.IsBlank {
			out.NonBlank = append(out.NonBlank, p.n)
		}
	}
	log.Debug("pageanalyze.done",
		"pages", len(out.Pages), "non_blank", len(out.NonBlank))
	return out, nil
}

// meanWhitePGM reads a P5 (binary) PGM and returns the mean pixel
// value divided by the file's maxval — 1.0 means "all white". Uses a
// streaming reader so a large PGM never lands fully in memory.
func meanWhitePGM(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	br := bufio.NewReader(f)

	magic, err := readToken(br)
	if err != nil {
		return 0, err
	}
	if magic != "P5" {
		return 0, fmt.Errorf("pageanalyze: not a binary PGM (got %q)", magic)
	}
	widthS, err := readToken(br)
	if err != nil {
		return 0, err
	}
	heightS, err := readToken(br)
	if err != nil {
		return 0, err
	}
	maxvalS, err := readToken(br)
	if err != nil {
		return 0, err
	}
	// PGM spec: exactly one whitespace character follows maxval and
	// precedes the pixel data. bufio has already consumed it via
	// readToken's terminating whitespace.

	width, err := strconv.Atoi(widthS)
	if err != nil {
		return 0, err
	}
	height, err := strconv.Atoi(heightS)
	if err != nil {
		return 0, err
	}
	maxval, err := strconv.Atoi(maxvalS)
	if err != nil {
		return 0, err
	}
	if maxval == 0 {
		return 0, errors.New("pageanalyze: PGM maxval zero")
	}

	total := int64(width) * int64(height)
	if total == 0 {
		return 0, errors.New("pageanalyze: PGM zero-sized")
	}

	// Stream in 64 KiB chunks. Two-byte samples if maxval > 255 (rare
	// for pdftoppm's default output, but the spec allows it).
	var sum int64
	if maxval <= 255 {
		buf := make([]byte, 64*1024)
		read := int64(0)
		for read < total {
			n, err := br.Read(buf)
			if n > 0 {
				remaining := total - read
				take := int64(n)
				if take > remaining {
					take = remaining
				}
				for i := int64(0); i < take; i++ {
					sum += int64(buf[i])
				}
				read += take
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, err
			}
		}
	} else {
		buf := make([]byte, 64*1024)
		read := int64(0)
		bytesToRead := total * 2
		for read < bytesToRead {
			n, err := br.Read(buf)
			if n > 0 {
				// Big-endian 16-bit samples per spec.
				for i := 0; i+1 < n && read < bytesToRead; i += 2 {
					sum += int64(buf[i])<<8 | int64(buf[i+1])
					read += 2
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, err
			}
		}
	}
	return float64(sum) / (float64(total) * float64(maxval)), nil
}

// readToken reads whitespace-delimited PGM header tokens, skipping
// comments (lines starting with '#').
func readToken(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		r, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case r == '#':
			// Skip to end of comment line.
			for {
				c, err := br.ReadByte()
				if err != nil {
					return "", err
				}
				if c == '\n' {
					break
				}
			}
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if b.Len() > 0 {
				return b.String(), nil
			}
		default:
			b.WriteByte(r)
		}
	}
}

type pgmFile struct {
	path string
	n    int
}

// listPGMs returns pdftoppm's output files in reading order. Same
// numeric-suffix sort as core/pipeline/tessocr.
func listPGMs(dir string) ([]pgmFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []pgmFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".pgm") {
			continue
		}
		base := strings.TrimSuffix(name, ".pgm")
		i := strings.LastIndex(base, "-")
		if i < 0 {
			continue
		}
		n := 0
		bad := false
		for _, r := range base[i+1:] {
			if r < '0' || r > '9' {
				bad = true
				break
			}
			n = n*10 + int(r-'0')
		}
		if bad {
			continue
		}
		out = append(out, pgmFile{path: filepath.Join(dir, name), n: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out, nil
}

func tail(b []byte) string {
	const n = 256
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(bytes.TrimSpace(b[len(b)-n:])))
}
