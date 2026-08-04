package djvu

import "testing"

func TestRecognized(t *testing.T) {
	cases := map[string]bool{
		"image/vnd.djvu":  true,
		"image/x-djvu":    true,
		"image/DJVU":      true,
		"application/pdf": false,
		"image/png":       false,
	}
	for mime, want := range cases {
		if got := Recognized(mime); got != want {
			t.Errorf("Recognized(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestCountNonWhitespace(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"hello world": 10,
		" \t\n":       0,
		"abc\n\ndef":  6,
	}
	for s, want := range cases {
		if got := countNonWhitespace(s); got != want {
			t.Errorf("countNonWhitespace(%q) = %d want %d", s, got, want)
		}
	}
}
