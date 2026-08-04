package tessocr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListPGMs_NumericOrder(t *testing.T) {
	// pdftoppm outputs "p-1.pgm", "p-2.pgm", ..., "p-10.pgm" — lexical
	// sort would put "p-10.pgm" before "p-2.pgm". Confirm we sort numerically.
	dir := t.TempDir()
	for _, name := range []string{"p-10.pgm", "p-2.pgm", "p-1.pgm", "unrelated.txt", "p-abc.pgm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listPGMs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "p-1.pgm"),
		filepath.Join(dir, "p-2.pgm"),
		filepath.Join(dir, "p-10.pgm"),
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestCountNonWhitespace(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"hello world": 10,
		"\n\t  \r":    0,
		"abc\n\ndef":  6,
	}
	for s, want := range cases {
		if got := countNonWhitespace(s); got != want {
			t.Errorf("%q → %d want %d", s, got, want)
		}
	}
}
