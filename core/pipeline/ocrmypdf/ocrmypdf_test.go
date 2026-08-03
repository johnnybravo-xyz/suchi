package ocrmypdf_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/pipeline/ocrmypdf"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestMissingBinaryReturnsSkipped(t *testing.T) {
	ctx := context.Background()
	res, err := ocrmypdf.OCR(ctx, bytes.NewReader([]byte("in")), silentLog(), ocrmypdf.Options{
		Binary: "/nonexistent/ocrmypdf",
	})
	if err != nil {
		t.Fatalf("ocr: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped=false, want true")
	}
	if len(res.ArchivePDF) != 0 || res.Text != "" {
		t.Errorf("skipped result should be empty; got archive=%d text=%q", len(res.ArchivePDF), res.Text)
	}
}

// TestFakeBinaryHappyPath uses a shell-scripted stand-in that writes
// `out.pdf` and `text.txt` to the sandbox dir — matching the file
// contract ocrmypdf itself follows.
func TestFakeBinaryHappyPath(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()
	dir := t.TempDir()
	fake := filepath.Join(dir, "ocrmypdf")
	script := `#!/bin/sh
# Fake ocrmypdf: last two args are input + output PDF paths; --sidecar
# arg is the text path. We just write the outputs directly.
while [ $# -gt 0 ]; do
  case "$1" in
    --sidecar) shift; sidecar="$1" ;;
    --language) shift ;;
    --quiet) ;;
    *.pdf)
      if [ -z "$in" ]; then in="$1"; else out="$1"; fi ;;
  esac
  shift || true
done
cp "$in" "$out"                    # archive PDF ≈ input in the fake
echo "extracted text OK" > "$sidecar"
`
	must(t, os.WriteFile(fake, []byte(script), 0o755))

	src := []byte("%PDF-1.7 fake\n%%EOF\n")
	res, err := ocrmypdf.OCR(ctx, bytes.NewReader(src), silentLog(), ocrmypdf.Options{
		Binary:    fake,
		Languages: []string{"eng"},
	})
	if err != nil {
		t.Fatalf("ocr: %v", err)
	}
	if res.Skipped {
		t.Errorf("Skipped=true, want false (binary succeeded)")
	}
	if !bytes.Equal(res.ArchivePDF, src) {
		t.Errorf("ArchivePDF mismatch")
	}
	if got := string(bytes.TrimSpace([]byte(res.Text))); got != "extracted text OK" {
		t.Errorf("Text=%q, want 'extracted text OK'", res.Text)
	}
}

// TestFakeBinaryNonZeroExitSkips: OCRmyPDF returns non-zero on failures
// (unsupported input, unknown language). Wrapper's contract is Skip,
// not error — ingest continues.
func TestFakeBinaryNonZeroExitSkips(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()
	dir := t.TempDir()
	fake := filepath.Join(dir, "ocrmypdf-fails")
	must(t, os.WriteFile(fake, []byte("#!/bin/sh\necho 'unsupported' 1>&2\nexit 3\n"), 0o755))

	res, err := ocrmypdf.OCR(ctx, bytes.NewReader([]byte("junk")), silentLog(), ocrmypdf.Options{Binary: fake})
	if err != nil {
		t.Fatalf("ocr: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped=false, want true")
	}
	if res.StderrTail == "" {
		t.Error("StderrTail empty — expected ocrmypdf's error to be captured")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
