package imgpdf_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
		{"image/heic", false}, // handled by the heic package
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
		"image/heic":                 "img", // unknown here
	}
	for mime, want := range cases {
		if got := imgpdf.ExtFromMIME(mime); got != want {
			t.Errorf("ExtFromMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

// TestConvert exercises the ImageMagick path end-to-end when a magick
// binary is on PATH. Skipped otherwise — the package's Skipped=true
// return is the fallback and doesn't need a binary to verify.
func TestConvert(t *testing.T) {
	if !imgpdf.Available() {
		t.Skip("ImageMagick not available")
	}

	// Synthesize a tiny PNG via magick.
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "in.png")
	cmd := exec.Command("magick", "-size", "64x64", "xc:red", pngPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("magick can't produce PNG in this build (%v): %s", err, out)
	}
	src, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(src) < 32 {
		t.Fatalf("synthesized PNG too small (%d bytes)", len(src))
	}

	res, err := imgpdf.Convert(context.Background(), bytes.NewReader(src),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		imgpdf.Options{Ext: "png"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Skipped {
		t.Fatalf("Convert reported Skipped=true: %s", res.StderrTail)
	}
	if !bytes.HasPrefix(res.PDF, []byte("%PDF")) {
		n := 8
		if len(res.PDF) < n {
			n = len(res.PDF)
		}
		t.Fatalf("output isn't a PDF (first %d bytes: %q)", n, res.PDF[:n])
	}
}
