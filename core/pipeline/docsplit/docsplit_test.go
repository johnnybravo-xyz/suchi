// SPDX-License-Identifier: AGPL-3.0-or-later

package docsplit

import (
	"errors"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/barcode"
)

func TestComputeSegments(t *testing.T) {
	cases := []struct {
		name       string
		total      int
		separators []int
		want       []Segment
	}{
		{"no separators", 5, nil, []Segment{{Start: 1, End: 5}}},
		{"single separator middle", 5, []int{3}, []Segment{{Start: 1, End: 2}, {Start: 4, End: 5}}},
		{"leading separator", 5, []int{1}, []Segment{{Start: 2, End: 5}}},
		{"trailing separator", 5, []int{5}, []Segment{{Start: 1, End: 4}}},
		{"two separators", 7, []int{3, 6}, []Segment{{Start: 1, End: 2}, {Start: 4, End: 5}, {Start: 7, End: 7}}},
		{"adjacent separators", 6, []int{3, 4}, []Segment{{Start: 1, End: 2}, {Start: 5, End: 6}}},
		{"all separators", 3, []int{1, 2, 3}, nil},
		{"empty pdf", 0, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeSegments(c.total, c.separators)
			if len(got) != len(c.want) {
				t.Fatalf("len(got)=%d want %d: %+v", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("[%d] got %+v want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestSegment_PagesAndCount(t *testing.T) {
	s := Segment{Start: 3, End: 5}
	if s.PageCount() != 3 {
		t.Errorf("PageCount = %d want 3", s.PageCount())
	}
	pages := s.Pages()
	want := []int{3, 4, 5}
	if len(pages) != 3 || pages[0] != want[0] || pages[1] != want[1] || pages[2] != want[2] {
		t.Errorf("Pages = %v want %v", pages, want)
	}
}

func TestPlanASNAssign(t *testing.T) {
	opts := Options{ASNMode: ASNModeAssign, ASNPrefix: "ASN"}
	pages := classifyPages(opts,
		[]barcode.Barcode{{Text: "unrelated", Format: "QR"}},
		nil,
		[]barcode.Barcode{{Text: "ASN000042", Format: "QR"}},
	)
	got, err := planPages(3, pages, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := Segment{Start: 1, End: 3, ASN: 42, MarkerPage: 3}
	if got.Mode != ModeASNAssign || len(got.Segments) != 1 || got.Segments[0] != want {
		t.Fatalf("plan = %+v, want segment %+v", got, want)
	}
}

func TestPlanASNAssignRejectsSecondMarker(t *testing.T) {
	opts := Options{ASNMode: ASNModeAssign, ASNPrefix: "ASN"}
	pages := classifyPages(opts,
		[]barcode.Barcode{{Text: "ASN000042", Format: "QR"}},
		[]barcode.Barcode{{Text: "ASN42", Format: "QR"}},
	)
	assertValidationCode(t, pages, opts, CodeMultipleASN)
}

func TestPlanASNSplitRetainsMarkerPages(t *testing.T) {
	opts := Options{ASNMode: ASNModeSplit, ASNPrefix: "ASN"}
	pages := classifyPages(opts,
		[]barcode.Barcode{
			{Text: "ASN000042", Format: "QR"},
			{Text: "ASN999", Format: "DATA_MATRIX"},
			{Text: "https://example.invalid", Format: "QR"},
		},
		nil,
		[]barcode.Barcode{{Text: "ASN43", Format: "QR"}},
		nil,
	)
	got, err := planPages(4, pages, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []Segment{
		{Start: 1, End: 2, ASN: 42, MarkerPage: 1},
		{Start: 3, End: 4, ASN: 43, MarkerPage: 3},
	}
	if got.Mode != ModeASNSplit || len(got.Segments) != len(want) {
		t.Fatalf("plan = %+v", got)
	}
	for i := range want {
		if got.Segments[i] != want[i] {
			t.Fatalf("segment %d = %+v, want %+v", i, got.Segments[i], want[i])
		}
	}
}

func TestPlanASNValidation(t *testing.T) {
	opts := Options{ASNMode: ASNModeSplit, ASNPrefix: "ASN"}
	tests := []struct {
		name  string
		pages [][]barcode.Barcode
		code  string
	}{
		{
			name:  "missing leading",
			pages: [][]barcode.Barcode{nil, {{Text: "ASN42", Format: "QR"}}},
			code:  CodeMissingLeadingASN,
		},
		{
			name: "multiple on page",
			pages: [][]barcode.Barcode{{
				{Text: "ASN42", Format: "QR"},
				{Text: "ASN43", Format: "QR"},
			}},
			code: CodeMultipleASNOnPage,
		},
		{
			name: "repeated canonical value",
			pages: [][]barcode.Barcode{
				{{Text: "ASN000042", Format: "QR"}},
				{{Text: "ASN42", Format: "QR"}},
			},
			code: CodeDuplicateASN,
		},
		{
			name:  "malformed",
			pages: [][]barcode.Barcode{{{Text: "ASN+42", Format: "QR"}}},
			code:  CodeMalformedASN,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertValidationCode(t, classifyPages(opts, tc.pages...), opts, tc.code)
		})
	}
}

func TestPlanRejectsMixedMarkers(t *testing.T) {
	opts := Options{
		SeparatorEnabled: true,
		Token:            DefaultToken,
		ASNMode:          ASNModeSplit,
		ASNPrefix:        "ASN",
	}
	pages := classifyPages(opts,
		[]barcode.Barcode{{Text: "ASN42", Format: "QR"}},
		[]barcode.Barcode{{Text: DefaultToken, Format: "QR"}},
	)
	assertValidationCode(t, pages, opts, CodeMixedMarkers)
}

func TestPlanLegacySeparatorsUnchanged(t *testing.T) {
	opts := Options{SeparatorEnabled: true, Token: DefaultToken, ASNMode: ASNModeOff}
	pages := classifyPages(opts,
		nil,
		[]barcode.Barcode{{Text: " " + DefaultToken + " ", Format: "AZTEC"}},
		nil,
	)
	got, err := planPages(3, pages, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeSeparator || len(got.Segments) != 2 ||
		got.Segments[0] != (Segment{Start: 1, End: 1}) ||
		got.Segments[1] != (Segment{Start: 3, End: 3}) {
		t.Fatalf("legacy plan = %+v", got)
	}
}

func classifyPages(opts Options, symbols ...[]barcode.Barcode) []pageSymbols {
	token := opts.Token
	if token == "" {
		token = DefaultToken
	}
	pages := make([]pageSymbols, len(symbols))
	for i, codes := range symbols {
		pages[i] = classifyPage(i+1, codes, opts, token)
	}
	return pages
}

func assertValidationCode(t *testing.T, pages []pageSymbols, opts Options, code string) {
	t.Helper()
	_, err := planPages(len(pages), pages, opts)
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != code {
		t.Fatalf("plan error = %v, want validation code %q", err, code)
	}
}
