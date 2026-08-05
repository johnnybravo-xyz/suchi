package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePageParams_Defaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/", nil)
	p := ParsePageParams(r, 25, 200)
	if p.Page != 1 {
		t.Errorf("Page: got %d, want 1", p.Page)
	}
	if p.PageSize != 25 {
		t.Errorf("PageSize: got %d, want 25", p.PageSize)
	}
	if p.Ordering != "" {
		t.Errorf("Ordering: got %q, want empty", p.Ordering)
	}
}

func TestParsePageParams_Clamps(t *testing.T) {
	cases := []struct {
		q                string
		wantPage         int
		wantPageSize     int
		wantOrdering     string
		defaultSizeInput int
		maxSizeInput     int
	}{
		{"page=0", 1, 25, "", 25, 200},
		{"page=-5", 1, 25, "", 25, 200},
		{"page=3", 3, 25, "", 25, 200},
		{"page_size=0", 1, 25, "", 25, 200},
		{"page_size=500", 1, 200, "", 25, 200},
		{"page_size=50", 1, 50, "", 25, 200},
		{"ordering=-created", 1, 25, "-created", 25, 200},
		{"ordering=%20title%20", 1, 25, "title", 25, 200},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/things/?"+tc.q, nil)
			p := ParsePageParams(r, tc.defaultSizeInput, tc.maxSizeInput)
			if p.Page != tc.wantPage {
				t.Errorf("Page: got %d, want %d", p.Page, tc.wantPage)
			}
			if p.PageSize != tc.wantPageSize {
				t.Errorf("PageSize: got %d, want %d", p.PageSize, tc.wantPageSize)
			}
			if p.Ordering != tc.wantOrdering {
				t.Errorf("Ordering: got %q, want %q", p.Ordering, tc.wantOrdering)
			}
		})
	}
}

func TestBuildEnvelope_FirstPage(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/?page=1", nil)
	p := PageParams{Page: 1, PageSize: 10}
	rows := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	env := BuildEnvelope(r, 25, p, rows)
	if env.Count != 25 {
		t.Errorf("Count: got %d, want 25", env.Count)
	}
	if env.Previous != "" {
		t.Errorf("Previous on page 1 should be empty, got %q", env.Previous)
	}
	if !strings.Contains(env.Next, "page=2") {
		t.Errorf("Next should point to page=2, got %q", env.Next)
	}
	if len(env.Results) != 10 {
		t.Errorf("Results len: got %d, want 10", len(env.Results))
	}
}

func TestBuildEnvelope_MiddlePage(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/?page=2&ordering=-date", nil)
	p := PageParams{Page: 2, PageSize: 10, Ordering: "-date"}
	rows := make([]int, 10)
	env := BuildEnvelope(r, 25, p, rows)
	if !strings.Contains(env.Previous, "page=1") {
		t.Errorf("Previous should be page=1, got %q", env.Previous)
	}
	if !strings.Contains(env.Next, "page=3") {
		t.Errorf("Next should be page=3, got %q", env.Next)
	}
	// Query params carry across pages — the ordering filter must survive.
	if !strings.Contains(env.Next, "ordering=") {
		t.Errorf("Next should preserve ordering param, got %q", env.Next)
	}
}

func TestBuildEnvelope_LastPage(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/?page=3", nil)
	p := PageParams{Page: 3, PageSize: 10}
	rows := make([]int, 5) // 25 total; page 3 has 5
	env := BuildEnvelope(r, 25, p, rows)
	if env.Next != "" {
		t.Errorf("Next on last page should be empty, got %q", env.Next)
	}
	if !strings.Contains(env.Previous, "page=2") {
		t.Errorf("Previous should be page=2, got %q", env.Previous)
	}
}

func TestBuildEnvelope_Empty(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/", nil)
	p := PageParams{Page: 1, PageSize: 10}
	env := BuildEnvelope[int](r, 0, p, nil)
	if env.Count != 0 {
		t.Errorf("Count: got %d, want 0", env.Count)
	}
	if env.Next != "" || env.Previous != "" {
		t.Errorf("Neighbors on empty should be empty, got next=%q prev=%q",
			env.Next, env.Previous)
	}
	if env.Results == nil {
		t.Errorf("Results should be [] not null")
	}
}

func TestParseCSVInts(t *testing.T) {
	cases := []struct {
		q    string
		want []int64
	}{
		{"", nil},
		{"tags__id__in=", nil},
		{"tags__id__in=1", []int64{1}},
		{"tags__id__in=1,2,3", []int64{1, 2, 3}},
		{"tags__id__in=+1+,+2+,+3+", []int64{1, 2, 3}}, // + decodes to space
		{"tags__id__in=1,invalid,3", []int64{1, 3}},
		{"tags__id__in=0,-1", nil}, // 0 and negative dropped
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			url := "/api/things/"
			if tc.q != "" {
				url += "?" + tc.q
			}
			r := httptest.NewRequest("GET", url, nil)
			got := ParseCSVInts(r, "tags__id__in")
			if len(got) != len(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
				return
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestOrderingToSQL(t *testing.T) {
	allow := map[string]string{
		"created": "created_at",
		"title":   "title",
	}
	cases := map[string]string{
		"":                            "",
		"created":                     "created_at ASC",
		"-created":                    "created_at DESC",
		"title":                       "title ASC",
		"-title":                      "title DESC",
		"password":                    "", // not on allowlist
		"'; DROP TABLE documents; --": "", // sql injection attempt
	}
	for input, want := range cases {
		got := OrderingToSQL(input, allow)
		if got != want {
			t.Errorf("OrderingToSQL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPageURL_PreservesOtherParams(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/?ordering=-created&tags__id__in=1,2", nil)
	url := pageURL(r, 3)
	if !strings.Contains(url, "page=3") {
		t.Errorf("missing page: %q", url)
	}
	if !strings.Contains(url, "ordering=") {
		t.Errorf("missing ordering: %q", url)
	}
	if !strings.Contains(url, "tags__id__in=") {
		t.Errorf("missing filter: %q", url)
	}
}

// Contract test — an empty envelope serialized to JSON must have a
// zero count and an empty results array, no null. Mobile clients
// iterate results directly and would crash on null.
func TestEnvelope_JSON_EmptyShape(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/things/", nil)
	env := BuildEnvelope[int](r, 0, PageParams{Page: 1, PageSize: 10}, nil)
	body := renderJSON(t, env)
	if !strings.Contains(body, `"count":0`) {
		t.Errorf("count field missing: %q", body)
	}
	if !strings.Contains(body, `"results":[]`) {
		t.Errorf("results should be [], got: %q", body)
	}
}

// Small helper: encode a value with the standard http.Server JSON
// path so we exercise the same tags/omitempty behavior real handlers
// use. httptest recorder mimics the wire.
func renderJSON(t *testing.T, v any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s := &Server{}
	s.writeJSON(rec, http.StatusOK, v)
	return rec.Body.String()
}
