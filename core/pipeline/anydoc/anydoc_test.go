// SPDX-License-Identifier: AGPL-3.0-or-later

package anydoc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountNonWhitespace(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"hello world": 10,
		" \t\n":       0,
		"abc\n\ndef":  6,
	}
	for s, want := range cases {
		if got := countNonWhitespace(s); got != want {
			t.Errorf("countNonWhitespace(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestExtract_NoBinary asserts the graceful-degrade path: when the
// binary can't be found, Extract returns Skipped=true instead of
// erroring — same posture as djvu/ocrmypdf.
func TestExtract_NoBinary(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := strings.NewReader("not really a docx")
	res, err := Extract(ctx, src, log, Options{
		Binary: "/nonexistent/path/to/anydoc-that-does-not-exist",
		Ext:    ".docx",
	})
	if err != nil {
		t.Fatalf("Extract with missing binary should not error, got %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped should be true when binary is absent, got Result=%+v", res)
	}
}

// TestExtract_FakeBinary uses a bash stub as the "anydoc" CLI to
// exercise the full Extract path end-to-end without depending on the
// real Rust binary. The stub writes a fixed markdown string to stdout;
// we verify Extract picks it up, counts non-blank chars, and reports
// HasText correctly.
func TestExtract_FakeBinary(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; can't run fake-binary test")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "anydoc")
	err := os.WriteFile(stub, []byte(
		`#!/usr/bin/env bash
echo "# Test Document"
echo ""
echo "This is a converted document with some content that FTS should find."
`), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := bytes.NewReader([]byte("input bytes ignored by the stub"))
	res, err := Extract(ctx, src, log, Options{Binary: stub, Ext: ".docx"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Skipped {
		t.Fatalf("Skipped should be false with the fake binary, got %+v", res)
	}
	if !strings.Contains(res.Text, "Test Document") {
		t.Errorf("Text missing stub output: %q", res.Text)
	}
	if !res.HasText {
		t.Errorf("HasText should be true for %d non-blank chars", res.NonBlank)
	}
	if res.Truncated {
		t.Errorf("Truncated should be false for short output")
	}
}

// TestExtract_FakeBinary_Fails verifies non-zero exit → Skipped=true
// with stderr captured — matches djvu / ocrmypdf failure posture.
func TestExtract_FakeBinary_Fails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "anydoc")
	err := os.WriteFile(stub, []byte(
		`#!/usr/bin/env bash
echo "unsupported format" 1>&2
exit 2
`), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := strings.NewReader("junk")
	res, err := Extract(ctx, src, log, Options{Binary: stub, Ext: ".docx"})
	if err != nil {
		t.Fatalf("Extract should swallow exit errors, got %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped should be true on non-zero exit, got %+v", res)
	}
	if !strings.Contains(res.StderrTail, "unsupported format") {
		t.Errorf("StderrTail should carry the stub's stderr, got %q", res.StderrTail)
	}
}
