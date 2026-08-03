package pdfinspector_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/pipeline/pdfinspector"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestMissingBinaryReturnsSkipped(t *testing.T) {
	ctx := context.Background()
	res, err := pdfinspector.Extract(ctx, bytes.NewReader([]byte("dummy")), silentLog(), pdfinspector.Options{
		Binary: "/nonexistent/pdftotext",
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped=false, want true")
	}
	if res.HasText {
		t.Errorf("HasText=true on missing binary, want false")
	}
}

// TestHasTextThreshold uses a fake binary that emits a controlled
// amount of text so we can pin the HasText decision without depending
// on a real PDF fixture.
func TestHasTextThreshold(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()

	cases := []struct {
		name       string
		script     string
		wantHasTxt bool
	}{
		{
			name:       "plenty-of-text",
			script:     "#!/bin/sh\necho 'Bill for electricity supply period; total due 4523.'\n",
			wantHasTxt: true,
		},
		{
			name:       "sparse-text",
			script:     "#!/bin/sh\necho 'x'\n", // 1 non-whitespace char
			wantHasTxt: false,
		},
		{
			name:       "empty",
			script:     "#!/bin/sh\n",
			wantHasTxt: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "pdftotext")
			must(t, os.WriteFile(fake, []byte(c.script), 0o755))

			res, err := pdfinspector.Extract(ctx, bytes.NewReader([]byte("in")), silentLog(), pdfinspector.Options{Binary: fake})
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if res.HasText != c.wantHasTxt {
				t.Errorf("HasText=%v (non-blank=%d), want %v; text=%q", res.HasText, res.NonBlank, c.wantHasTxt, res.Text)
			}
		})
	}
}

// TestNonZeroExitSkips: pdftotext returns non-zero on non-PDFs; the
// wrapper's contract is to skip (not error) so ingest can still try OCR.
func TestNonZeroExitSkips(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()
	dir := t.TempDir()
	fake := filepath.Join(dir, "pdftotext-fails")
	must(t, os.WriteFile(fake, []byte("#!/bin/sh\necho 'not a pdf' 1>&2\nexit 1\n"), 0o755))

	res, err := pdfinspector.Extract(ctx, bytes.NewReader([]byte("junk")), silentLog(), pdfinspector.Options{Binary: fake})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped=false, want true on non-zero exit")
	}
	if res.HasText {
		t.Errorf("HasText=true on skip, want false")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
