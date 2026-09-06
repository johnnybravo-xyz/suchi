package tessocr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCommand(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

const onePage = `printf 'P2\n1 1\n255\n255\n' > "$5-1.pgm"`

func TestOCRRetriesOnlyEmptySuccessfulPages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		text    string
		skipped bool
	}{
		{
			name: "empty-page",
			body: `if [ "$#" -eq 6 ] && [ "$5" = --psm ] && [ "$6" = 11 ]; then printf 'Sparse label'; fi`,
			text: "Sparse label",
		},
		{
			name: "whitespace-page",
			body: `if [ "$#" -eq 6 ]; then printf 'Sparse label'; else printf ' \n\t'; fi`,
			text: "Sparse label",
		},
		{
			name: "existing-text",
			body: `if [ "$#" -ne 4 ]; then exit 1; fi; printf 'Existing text'`,
			text: "Existing text",
		},
		{
			name:    "failed-page",
			body:    `if [ "$#" -eq 6 ]; then printf 'Must not retry'; else printf 'decode failed' >&2; exit 1; fi`,
			skipped: true,
		},
		{
			name: "still-empty-page",
			body: `exit 0`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := OCR(context.Background(), strings.NewReader("pdf"),
				slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
					Pdftoppm: testCommand(t, onePage), Tesseract: testCommand(t, tc.body),
				})
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != tc.text || res.Skipped != tc.skipped || res.Pages != 1 {
				t.Fatalf("result = %+v; want text %q, skipped %t, one page", res, tc.text, tc.skipped)
			}
		})
	}
}

func TestOCRSparseRetryRespectsOutputCap(t *testing.T) {
	_, err := OCR(context.Background(), strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm:     testCommand(t, onePage),
			Tesseract:    testCommand(t, `if [ "$#" -eq 6 ]; then printf 'Too much text'; fi`),
			MaxTextBytes: 4,
		})
	if err == nil || !strings.Contains(err.Error(), "text exceeded cap") {
		t.Fatalf("got %v, want output cap error", err)
	}
}

func TestOCRSharesDeadlineAcrossRasterAndRetry(t *testing.T) {
	_, err := OCR(context.Background(), strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm: testCommand(t, "/bin/sleep 0.08\n"+onePage),
			Tesseract: testCommand(t, `/bin/sleep 0.08
if [ "$#" -eq 6 ]; then printf 'Late sparse text'; fi`),
			Timeout: 200 * time.Millisecond,
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want shared deadline error", err)
	}
}

func TestOCRCancellationIsNotSuccessfulSkip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := OCR(ctx, strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm: testCommand(t, onePage), Tesseract: testCommand(t, "exit 0"),
		})
	if !errors.Is(err, context.Canceled) || res != nil {
		t.Fatalf("result=%+v err=%v, want cancellation error", res, err)
	}
}

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
