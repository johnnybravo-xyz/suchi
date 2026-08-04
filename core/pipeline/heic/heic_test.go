package heic_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/pipeline/heic"
)

func TestRecognized(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"image/heic", true},
		{"image/heif", true},
		{"image/heic-sequence", true},
		{"image/heif-sequence", true},
		{"IMAGE/HEIC", true},
		{"image/jpeg", false},
		{"image/png", false},
		{"application/pdf", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := heic.Recognized(tc.mime); got != tc.want {
			t.Errorf("Recognized(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}

// TestConvert exercises the ImageMagick path end-to-end when a magick
// binary is on PATH. Skipped otherwise — the package's Skipped=true
// return is the fallback and doesn't need a binary to verify.
func TestConvert(t *testing.T) {
	if !heic.Available() {
		t.Skip("ImageMagick not available")
	}

	// Synthesize a tiny HEIC from a solid-color PNG via magick.
	dir := t.TempDir()
	heicPath := filepath.Join(dir, "in.heic")
	cmd := exec.Command("magick", "-size", "32x32", "xc:red", heicPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("magick can't produce HEIC in this build (%v): %s", err, out)
	}
	src, err := os.ReadFile(heicPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(src) < 32 {
		t.Fatalf("synthesized HEIC too small (%d bytes)", len(src))
	}

	res, err := heic.Convert(context.Background(), bytes.NewReader(src),
		slog.New(slog.NewTextHandler(io.Discard, nil)), heic.Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Skipped {
		t.Fatalf("Convert reported Skipped=true: %s", res.StderrTail)
	}
	if !bytes.HasPrefix(res.PDF, []byte("%PDF")) {
		t.Fatalf("output isn't a PDF (first 8 bytes: %q)", res.PDF[:min(8, len(res.PDF))])
	}
}
