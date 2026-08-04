package docsplit

import "testing"

func TestComputeSegments(t *testing.T) {
	cases := []struct {
		name       string
		total      int
		separators []int
		want       []Segment
	}{
		{"no separators", 5, nil, []Segment{{1, 5}}},
		{"single separator middle", 5, []int{3}, []Segment{{1, 2}, {4, 5}}},
		{"leading separator", 5, []int{1}, []Segment{{2, 5}}},
		{"trailing separator", 5, []int{5}, []Segment{{1, 4}}},
		{"two separators", 7, []int{3, 6}, []Segment{{1, 2}, {4, 5}, {7, 7}}},
		{"adjacent separators", 6, []int{3, 4}, []Segment{{1, 2}, {5, 6}}},
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
