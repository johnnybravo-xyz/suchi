// SPDX-License-Identifier: AGPL-3.0-or-later

// Package docsplit plans opt-in PDF splitting from legacy disposable separator
// sheets or retained archive-serial-number QR labels.
//
// Legacy separator pages are dropped. In ASN split mode every ASN-bearing page
// starts a retained segment. ASN assign mode keeps the PDF intact and accepts
// one label on any page.
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

	"github.com/johnnybravo-xyz/suchi/core/asn"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/barcode"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults.
const (
	DefaultBinary  = "pdftoppm"
	DefaultTimeout = 90 * time.Second
	DefaultDPI     = 150
	DefaultToken   = "SUCHI-SPLIT"

	ASNModeOff    = "off"
	ASNModeAssign = "assign"
	ASNModeSplit  = "split"
)

// Mode describes the marker family selected for one PDF.
type Mode string

const (
	ModeNone      Mode = "none"
	ModeSeparator Mode = "separator"
	ModeASNAssign Mode = "asn_assign"
	ModeASNSplit  Mode = "asn_split"
)

// Stable validation codes surfaced through terminal jobs.
const (
	CodeMalformedASN      = "malformed_asn"
	CodeMixedMarkers      = "mixed_markers"
	CodeMultipleASN       = "multiple_asn"
	CodeMultipleASNOnPage = "multiple_asn_on_page"
	CodeDuplicateASN      = "duplicate_asn"
	CodeMissingLeadingASN = "missing_leading_asn"
)

// ValidationError is a deterministic marker-plan failure.
type ValidationError struct {
	Code string
	Page int
	ASN  int64
}

func (e *ValidationError) Error() string {
	message := e.Code
	if e.Page > 0 {
		message += fmt.Sprintf(" page=%d", e.Page)
	}
	if e.ASN > 0 {
		message += fmt.Sprintf(" asn=%d", e.ASN)
	}
	return message
}

// Options carries per-call knobs. Zero binary/timeout/DPI values fall back to
// defaults. Separator and ASN markers are independently opt-in.
type Options struct {
	Binary  string
	Timeout time.Duration
	DPI     int

	SeparatorEnabled bool
	Token            string
	ASNMode          string
	ASNPrefix        string
}

// Segment is a contiguous, 1-indexed inclusive page range. ASN and MarkerPage
// are set only for retained ASN plans.
type Segment struct {
	Start      int
	End        int
	ASN        int64
	MarkerPage int
}

// Result carries the analysis output.
type Result struct {
	Mode           Mode
	Segments       []Segment
	SeparatorPages []int
	TotalPages     int
	Skipped        bool
}

// Analyze rasterizes pdfBytes once, classifies every symbol, and returns the
// complete marker plan.
func Analyze(ctx context.Context, pdfBytes []byte, log *slog.Logger, opts Options) (*Result, error) {
	started := time.Now()
	log = log.With("component", "docsplit")
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		if asnEnabled(opts.ASNMode) {
			return nil, fmt.Errorf("docsplit: required rasterizer %q not found", binary)
		}
		log.Warn("docsplit.skip.no_binary", "binary", binary)
		return &Result{Mode: ModeNone, Skipped: true}, nil
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
		exitCode := -1
		var stderr []byte
		if res != nil {
			exitCode = res.ExitCode
			stderr = res.Stderr
		}
		return nil, fmt.Errorf("pdftoppm: exit %d: %s", exitCode, tail(stderr))
	}

	pngs, err := listPNGs(dir)
	if err != nil {
		return nil, fmt.Errorf("list rasterized pages: %w", err)
	}
	if len(pngs) == 0 {
		return nil, errors.New("docsplit: no pages produced")
	}

	pages := make([]pageSymbols, 0, len(pngs))
	for _, p := range pngs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		codes, decodeErr := decodePage(ctx, p.path)
		if decodeErr != nil {
			if asnEnabled(opts.ASNMode) {
				return nil, fmt.Errorf("docsplit: decode page %d: %w", p.n, decodeErr)
			}
			log.Debug("docsplit.page.decode_failed", "page", p.n, "err", decodeErr.Error())
		}
		pages = append(pages, classifyPage(p.n, codes, opts, token))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := planPages(len(pngs), pages, opts)
	if err != nil {
		return nil, err
	}
	log.Info("docsplit.plan",
		"mode", out.Mode,
		"pages", out.TotalPages,
		"segments", len(out.Segments),
		"asn_pages", canonicalPairs(out.Segments),
		"elapsed", time.Since(started))
	return out, nil
}

func validateOptions(opts Options) error {
	switch opts.ASNMode {
	case "", ASNModeOff:
	case ASNModeAssign, ASNModeSplit:
		if err := asn.ValidatePrefix(opts.ASNPrefix); err != nil {
			return fmt.Errorf("docsplit: invalid ASN prefix: %w", err)
		}
	default:
		return fmt.Errorf("docsplit: invalid ASN mode %q", opts.ASNMode)
	}
	return nil
}

func asnEnabled(mode string) bool {
	return mode == ASNModeAssign || mode == ASNModeSplit
}

func decodePage(ctx context.Context, pngPath string) ([]barcode.Barcode, error) {
	data, err := os.ReadFile(pngPath)
	if err != nil {
		return nil, err
	}
	return barcode.DecodeAllBytes(ctx, data)
}

type pageSymbols struct {
	page         int
	separator    bool
	asns         []int64
	asnLooking   bool
	malformedASN bool
}

func classifyPage(page int, codes []barcode.Barcode, opts Options, token string) pageSymbols {
	out := pageSymbols{page: page}
	for _, code := range codes {
		if opts.SeparatorEnabled && strings.TrimSpace(code.Text) == token {
			out.separator = true
		}
		if !asnEnabled(opts.ASNMode) || code.Format != "QR" {
			continue
		}
		value, matched, err := asn.ParseLabel(opts.ASNPrefix, code.Text)
		if !matched {
			continue
		}
		out.asnLooking = true
		if err != nil {
			out.malformedASN = true
			continue
		}
		out.asns = append(out.asns, value)
	}
	return out
}

func planPages(total int, pages []pageSymbols, opts Options) (*Result, error) {
	out := &Result{Mode: ModeNone, TotalPages: total}
	var (
		asnPages  []pageSymbols
		hasLegacy bool
		hasASN    bool
	)
	for _, page := range pages {
		if page.separator {
			hasLegacy = true
			out.SeparatorPages = append(out.SeparatorPages, page.page)
		}
		if page.asnLooking {
			hasASN = true
			asnPages = append(asnPages, page)
		}
	}
	if hasLegacy && hasASN {
		return nil, &ValidationError{Code: CodeMixedMarkers, Page: firstMixedPage(pages)}
	}
	if hasLegacy {
		out.Mode = ModeSeparator
		out.Segments = computeSegments(total, out.SeparatorPages)
		return out, nil
	}
	if !hasASN {
		if total > 0 {
			out.Segments = []Segment{{Start: 1, End: total}}
		}
		return out, nil
	}
	for _, page := range asnPages {
		if page.malformedASN {
			return nil, &ValidationError{Code: CodeMalformedASN, Page: page.page}
		}
	}

	switch opts.ASNMode {
	case ASNModeAssign:
		count := 0
		var selected pageSymbols
		for _, page := range asnPages {
			count += len(page.asns)
			if len(page.asns) > 0 {
				selected = page
			}
		}
		if count != 1 {
			page := asnPages[0].page
			if len(asnPages) > 1 {
				page = asnPages[1].page
			}
			return nil, &ValidationError{Code: CodeMultipleASN, Page: page}
		}
		out.Mode = ModeASNAssign
		out.Segments = []Segment{{
			Start: 1, End: total, ASN: selected.asns[0], MarkerPage: selected.page,
		}}
		return out, nil

	case ASNModeSplit:
		if len(asnPages[0].asns) == 0 || asnPages[0].page != 1 {
			return nil, &ValidationError{Code: CodeMissingLeadingASN, Page: 1}
		}
		seen := make(map[int64]struct{}, len(asnPages))
		for _, page := range asnPages {
			if len(page.asns) > 1 {
				return nil, &ValidationError{Code: CodeMultipleASNOnPage, Page: page.page}
			}
			value := page.asns[0]
			if _, exists := seen[value]; exists {
				return nil, &ValidationError{Code: CodeDuplicateASN, Page: page.page, ASN: value}
			}
			seen[value] = struct{}{}
		}
		out.Mode = ModeASNSplit
		out.Segments = make([]Segment, len(asnPages))
		for i, page := range asnPages {
			end := total
			if i+1 < len(asnPages) {
				end = asnPages[i+1].page - 1
			}
			out.Segments[i] = Segment{
				Start: page.page, End: end, ASN: page.asns[0], MarkerPage: page.page,
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("docsplit: ASN markers found while mode is %q", opts.ASNMode)
}

func firstMixedPage(pages []pageSymbols) int {
	for _, page := range pages {
		if page.separator || page.asnLooking {
			return page.page
		}
	}
	return 0
}

func canonicalPairs(segments []Segment) []string {
	var pairs []string
	for _, segment := range segments {
		if segment.ASN > 0 {
			pairs = append(pairs, fmt.Sprintf("%d@%d", segment.ASN, segment.MarkerPage))
		}
	}
	return pairs
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
