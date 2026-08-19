package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Sanity check: the report unconditionally emits an INFO summary and
// a WARN line per missing tool. Runs against the current PATH so we
// can't pre-declare "anydoc will be missing" — we assert on the
// structural shape (both log lines exist somewhere in the output).
func TestReportToolAvailability(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	reportToolAvailability(log)
	out := buf.String()
	if !strings.Contains(out, "main.tools.check") {
		t.Fatalf("expected summary INFO line, got:\n%s", out)
	}
	// If any tool is missing, we should see at least one WARN.
	if strings.Contains(out, "missing=[") && !strings.Contains(out, "missing=[]") {
		if !strings.Contains(out, "main.tools.missing") {
			t.Fatalf("summary listed missing tools but no WARN line fired:\n%s", out)
		}
	}
}

func TestResolvePipelineToolUsesFallback(t *testing.T) {
	tool := pipelineTool{name: "magick", fallbacks: []string{"convert"}}
	got, ok := resolvePipelineTool(tool, func(name string) (string, error) {
		if name == "convert" {
			return "/usr/bin/convert", nil
		}
		return "", errors.New("not found")
	})
	if !ok || got != "convert" {
		t.Fatalf("resolvePipelineTool() = %q, %v; want convert, true", got, ok)
	}
}
