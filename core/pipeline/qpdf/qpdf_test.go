package qpdf_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/pipeline/qpdf"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestMissingBinaryReturnsOriginal covers the graceful-degrade path:
// when qpdf isn't installed on the box, Normalize returns the input
// untouched with Skipped=true. This is Phase 2's contract with the
// ingest pipeline — never fail ingest because a pre-processor is
// absent.
func TestMissingBinaryReturnsOriginal(t *testing.T) {
	ctx := context.Background()
	payload := []byte("hello, not-a-pdf")
	res, err := qpdf.Normalize(ctx, bytes.NewReader(payload), silentLog(), qpdf.Options{
		Binary: "/nonexistent/qpdf-that-cannot-be-run",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped=false, want true")
	}
	if !bytes.Equal(res.Data, payload) {
		t.Errorf("Data mismatch: got %q, want %q", res.Data, payload)
	}
}

// TestFakeBinaryHappyPath uses a shell-scripted stand-in for qpdf that
// echoes a marker to stdout. Proves the sandbox → qpdf → stdout capture
// path works without requiring a real qpdf install.
//
// The real qpdf integration test lives in the Docker "full" image
// stage of CI, where the binary is actually present.
func TestFakeBinaryHappyPath(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()

	// Write a mock qpdf that ignores its args and prints a known marker.
	dir := t.TempDir()
	fake := filepath.Join(dir, "qpdf")
	must(t, os.WriteFile(fake, []byte("#!/bin/sh\necho -n NORMALIZED\n"), 0o755))

	res, err := qpdf.Normalize(ctx, bytes.NewReader([]byte("original bytes")), silentLog(), qpdf.Options{
		Binary: fake,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if res.Skipped {
		t.Errorf("Skipped=true, want false (binary ran successfully)")
	}
	if string(res.Data) != "NORMALIZED" {
		t.Errorf("Data=%q, want NORMALIZED", res.Data)
	}
}

// TestFakeBinaryNonZeroExitReturnsOriginal: qpdf returns non-zero on
// non-PDF input; the wrapper's contract is to log + return the
// original untouched.
func TestFakeBinaryNonZeroExitReturnsOriginal(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()
	dir := t.TempDir()
	fake := filepath.Join(dir, "qpdf-fails")
	must(t, os.WriteFile(fake, []byte("#!/bin/sh\necho 'not a pdf' 1>&2\nexit 2\n"), 0o755))

	payload := []byte("this looks nothing like a pdf")
	res, err := qpdf.Normalize(ctx, bytes.NewReader(payload), silentLog(), qpdf.Options{
		Binary: fake,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped=false, want true on non-zero exit")
	}
	if !bytes.Equal(res.Data, payload) {
		t.Errorf("Data changed on non-zero exit — original should be preserved")
	}
	if res.StderrTail == "" {
		t.Error("StderrTail empty — expected qpdf's error to be captured")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
