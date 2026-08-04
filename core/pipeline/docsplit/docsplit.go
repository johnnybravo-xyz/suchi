// Package docsplit finds separator pages in a scanned PDF and returns
// the page ranges that make up each sub-document.
//
// Detection is QR-only and opt-in — users print separator sheets
// carrying a specific payload (default `SUCHI-SPLIT`), insert them
// between doc stacks in the feeder, and scan the whole pile. The
// pipeline drops the separators and fans out each segment to its own
// documents row.
//
// Why QR-only:
//
//   - Blank-page separation looks ergonomic but silently splits legit
//     multipage docs that happen to contain a mostly-empty page (a
//     signature panel, a chapter divider). QR is explicit: a user
//     who prints separator sheets intends the split.
//   - QR detection at 150 DPI is fast enough (~30ms per page on
//     modest hardware) that the pre-scan doesn't dominate ingest.
//
// Contract:
//
//   - Missing pdftoppm on PATH → Skipped=true, Segments empty, caller
//     treats input as a single doc.
//   - No separators found → one Segment spanning [1..N].
//   - Separators found → one Segment per contiguous non-separator
//     range; the separator pages themselves are dropped.
package docsplit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/barcode"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults.
const (
	DefaultBinary  = "pdftoppm"
	DefaultTimeout = 90 * time.Second
	// DPI needs to be high enough that a small QR (a few cm on the
	// printed page) survives rasterization. 150 is safe for modern
	// scanners; 100 works for larger QRs.
	DefaultDPI   = 150
	DefaultToken = "SUCHI-SPLIT"
)

// Options carries per-call knobs. Zero values fall back to Default*.
type Options struct {
	Binary  string
	Timeout time.Duration
	DPI     int

	// Token is the QR payload that marks a separator page. Case-
	// sensitive; whitespace trimmed. Empty falls back to DefaultToken.
	Token string
}

// Segment is a contiguous run of pages that make up one sub-document.
// Start and End are 1-indexed and inclusive.
type Segment struct {
	Start int
	End   int
}

// Result carries the analysis output.
type Result struct {
	// Segments is the list of non-separator page ranges in reading order.
	// When no separators are found, it's [{1, TotalPages}].
	Segments []Segment
	// SeparatorPages lists the 1-indexed pages that matched the token
	// and got dropped. Empty means "no splits, single doc".
	SeparatorPages []int
	// TotalPages is the raw page count of the input PDF.
	TotalPages int
	// Skipped=true when the pdftoppm binary is missing. Segments is
	// empty; caller keeps the input as a single doc.
	Skipped bool
}

// Analyze rasterizes pdfBytes and returns the split plan.
func Analyze(ctx context.Context, pdfBytes []byte, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "docsplit")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		log.Info("docsplit.skip.no_binary", "binary", binary)
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
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		token = DefaultToken
	}

	dir, err := os.MkdirTemp("", "suchi-docsplit-")
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
			binary, "-png", "-r", strconv.Itoa(dpi),
			inputPath, prefix,
		},
		Timeout:   timeout,
		MaxStdout: 1024,
		Dir:       dir,
	})
	if err != nil {
		return nil, fmt.Errorf("pdftoppm: exit %d: %s", res.ExitCode, tail(res.Stderr))
	}

	pngs, err := listPNGs(dir)
	if err != nil {
		return nil, fmt.Errorf("list rasterized pages: %w", err)
	}
	if len(pngs) == 0 {
		return nil, errors.New("docsplit: no pages produced")
	}

	out := &Result{TotalPages: len(pngs)}
	for _, p := range pngs {
		if isSeparator(p.path, token, log) {
			out.SeparatorPages = append(out.SeparatorPages, p.n)
		}
	}
	out.Segments = computeSegments(len(pngs), out.SeparatorPages)
	log.Debug("docsplit.done",
		"pages", len(pngs),
		"separators", len(out.SeparatorPages),
		"segments", len(out.Segments))
	return out, nil
}

// isSeparator returns true when any QR/DataMatrix/Aztec on the page
// matches token. Decode failures are logged at Debug and treated as
// "not a separator" — a genuinely broken page shouldn't force a split.
func isSeparator(pngPath, token string, log *slog.Logger) bool {
	data, err := os.ReadFile(pngPath)
	if err != nil {
		log.Debug("docsplit.page.read_failed", "path", pngPath, "err", err.Error())
		return false
	}
	codes, err := barcode.DecodeBytes(data)
	if err != nil {
		log.Debug("docsplit.page.decode_failed", "path", pngPath, "err", err.Error())
		return false
	}
	for _, c := range codes {
		if strings.TrimSpace(c.Text) == token {
			return true
		}
	}
	return false
}

// computeSegments walks 1..total and splits at each separator page,
// dropping separators themselves. Trailing/leading separators produce
// empty gaps which are filtered out. If there are no separators the
// result is [{1, total}].
func computeSegments(total int, separators []int) []Segment {
	sep := map[int]bool{}
	for _, s := range separators {
		sep[s] = true
	}
	var out []Segment
	start := 0
	for i := 1; i <= total; i++ {
		if sep[i] {
			if start > 0 {
				out = append(out, Segment{Start: start, End: i - 1})
				start = 0
			}
			continue
		}
		if start == 0 {
			start = i
		}
	}
	if start > 0 {
		out = append(out, Segment{Start: start, End: total})
	}
	return out
}

// Pages returns the 1-indexed page numbers for one segment.
func (s Segment) Pages() []int {
	out := make([]int, 0, s.End-s.Start+1)
	for i := s.Start; i <= s.End; i++ {
		out = append(out, i)
	}
	return out
}

// PageCount returns the number of pages in the segment.
func (s Segment) PageCount() int { return s.End - s.Start + 1 }

// ---------- helpers ----------

type pngFile struct {
	path string
	n    int
}

func listPNGs(dir string) ([]pngFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []pngFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".png") {
			continue
		}
		base := strings.TrimSuffix(name, ".png")
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
		out = append(out, pngFile{path: filepath.Join(dir, name), n: n})
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
