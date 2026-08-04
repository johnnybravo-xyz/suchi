package qpdf

import "testing"

// Internal test for the range collapser used by SelectPages. Kept in
// this file (package qpdf, not qpdf_test) so we can exercise the
// unexported helper directly.
func TestBuildPageRanges(t *testing.T) {
	cases := []struct {
		pages []int
		want  string
	}{
		{nil, ""},
		{[]int{1}, "1"},
		{[]int{1, 2, 3}, "1-3"},
		{[]int{1, 3, 5}, "1,3,5"},
		{[]int{1, 2, 3, 5, 7, 8}, "1-3,5,7-8"},
		{[]int{2, 3, 4, 6}, "2-4,6"},
		{[]int{10}, "10"},
	}
	for _, c := range cases {
		if got := buildPageRanges(c.pages); got != c.want {
			t.Errorf("%v → %q want %q", c.pages, got, c.want)
		}
	}
}
