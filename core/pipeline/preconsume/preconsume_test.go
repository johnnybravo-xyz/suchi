package preconsume

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestRun_NoScriptConfigured(t *testing.T) {
	r, err := Run(context.Background(), []byte("hi"), 1, "text/plain", "u@ex.dev", silentLog(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Skipped {
		t.Fatal("want Skipped=true when Script is empty")
	}
}

func TestRun_ScriptMissingFile(t *testing.T) {
	r, err := Run(context.Background(), []byte("hi"), 1, "text/plain", "u@ex.dev", silentLog(),
		Options{Script: "/no/such/file"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Skipped {
		t.Fatal("missing script → Skipped=true")
	}
}

func TestRun_ScriptRewritesBodyAndEmitsJSON(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hook.sh")
	src := `#!/bin/sh
set -e
# Uppercase the input into SUCHI_OUTPUT.
tr '[:lower:]' '[:upper:]' < "$1" > "$SUCHI_OUTPUT"
# Emit tags + custom_fields JSON on stdout.
cat <<EOF
{"tags": ["from-hook","doc-$DOC_ID"], "custom_fields": {"mime": "$MIME_TYPE"}}
EOF
`
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), []byte("hello world"), 42,
		"text/plain", "u@ex.dev", silentLog(),
		Options{Script: script})
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped {
		t.Fatal("script exists — should not be skipped")
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", r.ExitCode, r.StderrTail)
	}
	if string(r.WorkingBytes) != "HELLO WORLD" {
		t.Errorf("body: got %q want %q", r.WorkingBytes, "HELLO WORLD")
	}
	if len(r.Tags) != 2 || r.Tags[0] != "from-hook" || r.Tags[1] != "doc-42" {
		t.Errorf("tags: got %v", r.Tags)
	}
	if r.CustomFields["mime"] != "text/plain" {
		t.Errorf("custom_fields: got %v", r.CustomFields)
	}
}

func TestRun_ScriptExitNonZero_KeepsIngestFlowing(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "boom.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'broke' >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), []byte("hi"), 1, "text/plain", "u@ex.dev", silentLog(),
		Options{Script: script})
	if err != nil {
		t.Fatalf("non-zero exit should NOT be an error: %v", err)
	}
	if r.ExitCode != 7 {
		t.Errorf("exit: got %d want 7", r.ExitCode)
	}
	if r.WorkingBytes != nil {
		t.Errorf("no body rewrite expected: got %q", r.WorkingBytes)
	}
}

func TestRun_ScriptWritesNothing_UsesInput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "noop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), []byte("hi"), 1, "text/plain", "u@ex.dev", silentLog(),
		Options{Script: script})
	if err != nil {
		t.Fatal(err)
	}
	if r.WorkingBytes != nil {
		t.Fatal("empty SUCHI_OUTPUT should yield WorkingBytes=nil (caller uses input)")
	}
}
