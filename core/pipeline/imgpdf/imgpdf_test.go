package imgpdf_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/imgpdf"
)

func TestRecognized(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/tiff", true},
		{"image/webp", true},
		{"image/gif", true},
		{"image/bmp", true},
		{"IMAGE/JPEG", true},
		{"image/jpeg; charset=binary", true},
		{"image/heic", true},
		{"image/heif", true},
		{"image/heic-sequence", true},
		{"image/heif-sequence", true},
		{"image/svg+xml", false},
		{"application/pdf", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := imgpdf.Recognized(tc.mime); got != tc.want {
			t.Errorf("Recognized(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}

func TestExtFromMIME(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":                 "jpg",
		"image/png":                  "png",
		"image/tiff":                 "tiff",
		"image/webp":                 "webp",
		"image/gif":                  "gif",
		"image/bmp":                  "bmp",
		"image/jpeg; charset=binary": "jpg",
		"image/heic":                 "heic",
		"image/heif":                 "heif",
		"image/heic-sequence":        "heic",
		"image/heif-sequence":        "heif",
	}
	for mime, want := range cases {
		if got := imgpdf.ExtFromMIME(mime); got != want {
			t.Errorf("ExtFromMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

// Exercise real encoders and parse their output, not just a PDF-looking name.
func TestConvert(t *testing.T) {
	binary, err := exec.LookPath(imgpdf.DefaultBinary)
	if err != nil {
		binary, err = exec.LookPath(imgpdf.FallbackBinary)
		if err != nil {
			t.Skip("ImageMagick not available")
		}
	}
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("pdfinfo not available")
	}
	for _, tc := range []struct {
		mime   string
		frames int
		pages  int
	}{
		{"image/png", 1, 1},
		{"image/jpeg", 1, 1},
		{"image/heic", 1, 1},
		{"image/heif-sequence", 2, 1},
		{"image/tiff", 2, 2},
	} {
		t.Run(tc.mime, func(t *testing.T) {
			dir := t.TempDir()
			ext := imgpdf.ExtFromMIME(tc.mime)
			inputPath := filepath.Join(dir, "in."+ext)
			args := []string{"-size", "64x64", "xc:red"}
			if tc.frames == 2 {
				args = append(args, "xc:blue")
			}
			args = append(args, inputPath)
			if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
				t.Skipf("ImageMagick cannot synthesize %s: %v: %s", tc.mime, err, out)
			}
			info, err := exec.Command(binary, inputPath, "-format", "%n\n", "info:").CombinedOutput()
			if err != nil || len(strings.Fields(string(info))) != tc.frames {
				t.Fatalf("want %d input frames, got %q: %v", tc.frames, info, err)
			}
			src, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			res, err := imgpdf.Convert(context.Background(), bytes.NewReader(src),
				slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Ext: ext})
			if err != nil || res.Skipped {
				t.Fatalf("Convert: result=%+v err=%v", res, err)
			}
			cmd := exec.Command("pdfinfo", "-")
			cmd.Stdin = bytes.NewReader(res.PDF)
			info, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("pdfinfo: %v: %s", err, info)
			}
			pages := regexp.MustCompile(`(?m)^Pages:\s+(\d+)$`).FindSubmatch(info)
			if len(pages) != 2 || string(pages[1]) != strconv.Itoa(tc.pages) {
				t.Fatalf("want %d PDF pages, got: %s", tc.pages, info)
			}
		})
	}
}

func TestConvertRejectsNonPDFOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	// Reproduce ImageMagick's successful exit with PNG bytes in out.pdf.
	binary := filepath.Join(t.TempDir(), "magick")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
for output; do :; done
printf '\211PNG\r\n\032\n' > "${output#PDF:}"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := imgpdf.Convert(context.Background(), bytes.NewReader([]byte("fixture")),
		slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || len(res.PDF) != 0 || res.StderrTail != "ImageMagick did not produce a PDF" {
		t.Fatalf("non-PDF output was not rejected: %+v", res)
	}
}
