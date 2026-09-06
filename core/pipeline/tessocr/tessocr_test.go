package tessocr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

func sparseWord(block, line int, confidence, word string) string {
	return fmt.Sprintf("5\t1\t%d\t1\t%d\t1\t0\t0\t10\t10\t%s\t%s\n", block, line, confidence, word)
}

func TestConfidentSparseLines(t *testing.T) {
	tsv := "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		sparseWord(1, 1, "60", "CONFIDENT") + sparseWord(1, 1, "80", "LABEL") +
		sparseWord(1, 2, "65", "uncertain") +
		sparseWord(2, 1, "99", "A") + sparseWord(2, 2, "99", "!?--") +
		sparseWord(3, 1, "95", "भारतदेश") +
		sparseWord(4, 1, "invalid", "ignore") + sparseWord(4, 2, "-1", "ignore") +
		"malformed row\n" + sparseWord(5, 1, "NaN", "ignore")
	if got, want := confidentSparseLines([]byte(tsv)), []string{"CONFIDENT LABEL", "भारतदेश"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestOCRImageSupplementPreservesTextAndFiltersNoise(t *testing.T) {
	tsv := sparseWord(1, 1, "99", "Shared") + sparseWord(2, 1, "90", "GENERAL") +
		sparseWord(2, 1, "90", "CONSULATE") + sparseWord(3, 1, "99", "A") +
		sparseWord(4, 1, "25", "garbled noise")
	for _, imageInput := range []bool{false, true} {
		t.Run(fmt.Sprint(imageInput), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "supplement-used")
			raster := testCommand(t, `if [ "$#" -eq 5 ]; then
`+onePage+`
else
  test "$#" -eq 12
  test "$7" = -singlefile
  test "$8" = -scale-to
  test "$9" = 2400
  printf 'P2\n1 1\n255\n255\n' > "${12}.pgm"
fi`)
			ocr := testCommand(t, fmt.Sprintf(`if [ "$#" -eq 4 ]; then
  printf 'Baseline line\nShared'
else
  test "$5" = --psm
  test "$6" = 11
  test "$7" = tsv
  printf used > '%s'
  printf '%%s' '%s'
fi`, marker, tsv))
			res, err := OCR(context.Background(), strings.NewReader("pdf"),
				slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
					Pdftoppm: raster, Tesseract: ocr, ImageInput: imageInput,
				})
			if err != nil {
				t.Fatal(err)
			}
			want := "Baseline line\nShared"
			if imageInput {
				want += "\nGENERAL CONSULATE"
			}
			if res.Text != want || res.Skipped {
				t.Fatalf("result=%+v, want %q", res, want)
			}
			_, markerErr := os.Stat(marker)
			if (markerErr == nil) != imageInput {
				t.Fatalf("supplement called = %t, want %t", markerErr == nil, imageInput)
			}
		})
	}
}

func TestOCRImageSupplementFailurePreservesBaseline(t *testing.T) {
	res, err := OCR(context.Background(), strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm: testCommand(t, `if [ "$#" -ne 5 ]; then exit 1; fi
`+onePage),
			Tesseract: testCommand(t, "printf 'Baseline text'"), ImageInput: true,
		})
	if err != nil || res.Text != "Baseline text" || res.Skipped {
		t.Fatalf("result=%+v err=%v, want preserved baseline", res, err)
	}
}

func TestOCRImageSupplementDoesNotReusePreviousPageRaster(t *testing.T) {
	missingRaster := filepath.Join(t.TempDir(), "missing-raster")
	raster := testCommand(t, `if [ "$#" -eq 5 ]; then
  printf page > "$5-1.pgm"
  printf page > "$5-2.pgm"
elif [ "$4" -eq 1 ]; then
  printf page > "${12}.pgm"
fi`)
	ocr := testCommand(t, fmt.Sprintf(`if [ "$#" -eq 4 ]; then
  printf 'Baseline page'
elif [ -f "$1" ]; then
  printf '%%s' '%s'
else
  printf missing > '%s'
  exit 1
fi`, sparseWord(1, 1, "90", "Sparse label"), missingRaster))
	res, err := OCR(context.Background(), strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm: raster, Tesseract: ocr, ImageInput: true,
		})
	if err != nil || res.Pages != 2 || res.Text != "Baseline page\nSparse label\nBaseline page" {
		t.Fatalf("result=%+v err=%v, want two baseline pages and first supplement", res, err)
	}
	if _, err := os.Stat(missingRaster); err != nil {
		t.Fatal("second sparse raster reused the first page instead of failing independently")
	}
}

func TestOCRImageSupplementOutputCap(t *testing.T) {
	for _, tc := range []struct {
		name, baseline, word, errorText string
		cap                             int64
	}{
		{"tsv", "Text", "Sparse", "sparse TSV exceeded cap", 10},
		{"merged", strings.Repeat("b", 100), strings.Repeat("s", 80), "text exceeded cap", 160},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := OCR(context.Background(), strings.NewReader("pdf"),
				slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
					Pdftoppm: testCommand(t, `if [ "$#" -eq 5 ]; then
`+onePage+`
fi`),
					Tesseract:  testCommand(t, fmt.Sprintf(`if [ "$#" -eq 4 ]; then printf '%%s' '%s'; else printf '%%s' '%s'; fi`, tc.baseline, sparseWord(1, 1, "90", tc.word))),
					ImageInput: true, MaxTextBytes: tc.cap,
				})
			if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("error = %v, want %q", err, tc.errorText)
			}
		})
	}
}

func TestOCRImageSupplementSharesDeadline(t *testing.T) {
	_, err := OCR(context.Background(), strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm: testCommand(t, `if [ "$#" -eq 5 ]; then
`+onePage+`
else /bin/sleep 0.08; fi`),
			Tesseract: testCommand(t, `/bin/sleep 0.08
if [ "$#" -eq 4 ]; then printf 'Baseline text'; fi`),
			ImageInput: true, Timeout: 200 * time.Millisecond,
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want shared deadline error", err)
	}
}

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
			name:    "failed-page-without-stderr",
			body:    `exit 1`,
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

func TestOCROversizedWhitespaceCannotBeReplacedByRetry(t *testing.T) {
	_, err := OCR(context.Background(), strings.NewReader("pdf"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
			Pdftoppm:     testCommand(t, onePage),
			Tesseract:    testCommand(t, `if [ "$#" -eq 4 ]; then printf '            '; else printf 'OK'; fi`),
			MaxTextBytes: 4,
		})
	if err == nil || !strings.Contains(err.Error(), "text exceeded cap") {
		t.Fatalf("got %v, want output cap error before retry", err)
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
