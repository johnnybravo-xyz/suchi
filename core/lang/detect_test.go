package lang_test

import (
	"errors"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/lang"
)

func TestFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"de", ",de,"},
		{"DE", ",de,"},
		{"de, en, ", ",de,en,"},
		{"de,de,en", ",de,en,"},
		{"bad code", ""},
		{"en_US", ""}, // underscore isn't a valid separator
		{"kok", ",kok,"},
	}
	for _, c := range cases {
		if got := lang.Format(c.in); got != c.want {
			t.Errorf("Format(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{",de,", []string{"de"}},
		{",de,en,", []string{"de", "en"}},
	}
	for _, c := range cases {
		got := lang.Parse(c.in)
		if len(got) != len(c.want) {
			t.Errorf("Parse(%q): got %v want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Parse(%q)[%d]: got %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestPrimary(t *testing.T) {
	if lang.Primary("") != "" {
		t.Errorf("Primary(empty): expected empty string")
	}
	if got := lang.Primary(",de,en,"); got != "de" {
		t.Errorf("Primary(,de,en,): got %q want de", got)
	}
}

func TestFromPDFLang(t *testing.T) {
	cases := []struct{ in, want string }{
		{"en", "en"},
		{"en-US", "en"},
		{"DE-de", "de"},
		{"zh-Hant", "zh"},
		{"", ""},
		{"xx", "xx"},   // 2 letters — accepted
		{"xxx", "xxx"}, // 3 letters — accepted (ISO-639-3 codes like `kok`)
		{"xxxx", ""},   // 4+ letters — rejected
		{"1e", ""},     // must be letters
	}
	for _, c := range cases {
		if got := lang.FromPDFLang(c.in); got != c.want {
			t.Errorf("FromPDFLang(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestFromContentLanguage(t *testing.T) {
	cases := []struct{ in, want string }{
		{"de", "de"},
		{"de, en;q=0.5", "de"},
		{"en-US, de", "en"},
		{"", ""},
	}
	for _, c := range cases {
		if got := lang.FromContentLanguage(c.in); got != c.want {
			t.Errorf("FromContentLanguage(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// fakeDetector is a fixture that returns whatever it's told.
type fakeDetector struct {
	name    string
	results []lang.Result
	err     error
}

func (f fakeDetector) Name() string { return f.name }
func (f fakeDetector) Detect(_ string) ([]lang.Result, error) {
	return f.results, f.err
}

func TestChain_EmptyReturnsFalse(t *testing.T) {
	c := lang.NewChain(nil)
	if _, ok := c.BestAbove("hello", 0.5); ok {
		t.Fatal("empty chain must return ok=false")
	}
}

func TestChain_FirstWinnerAboveThreshold(t *testing.T) {
	first := fakeDetector{name: "first", results: []lang.Result{{Code: "de", Confidence: 0.9}}}
	second := fakeDetector{name: "second", results: []lang.Result{{Code: "en", Confidence: 0.99}}}
	c := lang.NewChain(nil, first, second)
	r, ok := c.BestAbove("hallo", 0.5)
	if !ok || r.Code != "de" {
		t.Fatalf("first-winner: got (%+v, %v)", r, ok)
	}
}

func TestChain_SkipsBelowThreshold(t *testing.T) {
	low := fakeDetector{name: "low", results: []lang.Result{{Code: "de", Confidence: 0.3}}}
	high := fakeDetector{name: "high", results: []lang.Result{{Code: "en", Confidence: 0.9}}}
	c := lang.NewChain(nil, low, high)
	r, ok := c.BestAbove("hallo", 0.5)
	if !ok || r.Code != "en" {
		t.Fatalf("skip-below: got (%+v, %v)", r, ok)
	}
}

func TestChain_SkipsErroringDetector(t *testing.T) {
	broken := fakeDetector{name: "broken", err: errors.New("boom")}
	good := fakeDetector{name: "good", results: []lang.Result{{Code: "fr", Confidence: 0.9}}}
	c := lang.NewChain(nil, broken, good)
	r, ok := c.BestAbove("bonjour", 0.5)
	if !ok || r.Code != "fr" {
		t.Fatalf("skip-error: got (%+v, %v)", r, ok)
	}
}

func TestChain_RegisterAppends(t *testing.T) {
	c := lang.NewChain(nil)
	c.Register(fakeDetector{name: "one"})
	c.Register(fakeDetector{name: "two"})
	if c.Len() != 2 {
		t.Fatalf("Register: expected Len=2, got %d", c.Len())
	}
}

func TestChain_RegisterNilNoOp(t *testing.T) {
	c := lang.NewChain(nil)
	c.Register(nil)
	if c.Len() != 0 {
		t.Fatalf("Register(nil): expected Len=0, got %d", c.Len())
	}
}
