// Package preconsume runs an optional operator-supplied script before
// any format-specific ingest logic (qpdf, docsplit, blank removal, OCR,
// ZUGFeRD, and friends). It's the generic escape hatch: whatever your
// scanner leaves behind that suchi doesn't handle natively — a
// polyglot ZIP that needs unpacking, a proprietary bank binary that
// needs decoding, a scan that needs custom deskewing — gets fixed here.
//
// Contract with the script:
//
//	argv[1]  = a scratch input path (readable) with the doc's bytes.
//	           The CAS original is NEVER handed to the script.
//	env      = DOC_ID, MIME_TYPE, OWNER_EMAIL, SUCHI_OUTPUT.
//	           SUCHI_OUTPUT is a scratch path the script MAY write
//	           modified bytes to. If it does, downstream ingest uses
//	           those bytes; if it doesn't, the input bytes flow through
//	           unchanged. Either way the CAS original is untouched.
//	stdout   = optional JSON: {"tags":["a","b"], "custom_fields":{"k":v}}.
//	           Applied by the caller after the script exits successfully.
//	exit 0   = success. Use SUCHI_OUTPUT if written; apply stdout JSON.
//	non-zero = log Warn, treat as "no changes". Ingest keeps flowing —
//	           a failing pre-consume never aborts a doc.
//
// The whole thing runs in core/sandbox: empty env (beyond the four
// vars above), no network, hard timeout, bounded stderr.
//
// Missing script (SCRIPT env var unset OR file not on disk) → Skipped=true,
// zero-cost no-op. Feature is genuinely opt-in.
package preconsume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults.
const (
	DefaultTimeout   = 5 * time.Minute
	DefaultMaxStdout = 1 * 1024 * 1024   // 1 MiB JSON output ceiling
	DefaultMaxOutput = 200 * 1024 * 1024 // 200 MiB for the modified doc bytes
)

// Options carries per-call knobs.
type Options struct {
	Script    string // path to executable; empty → Skipped
	Timeout   time.Duration
	MaxOutput int64 // max size of SUCHI_OUTPUT file
}

// Result carries the pre-consume output.
type Result struct {
	// WorkingBytes is the bytes downstream should operate on. Either
	// the SUCHI_OUTPUT file's contents (when the script wrote to it)
	// or nil to signal "use the input unchanged".
	WorkingBytes []byte

	// Tags is the list of tag names to attach. Applied by the caller.
	Tags []string

	// CustomFields is a name → value map. Types match customfield.Validate
	// inputs; the caller runs each through Lookup(dataType).Validate.
	CustomFields map[string]any

	// Skipped=true when no script was configured OR the script file
	// wasn't found. Not an error — pre-consume is opt-in.
	Skipped bool

	StderrTail string
	Duration   time.Duration
	ExitCode   int
}

// StdoutEnvelope is the JSON shape the script may print on stdout.
type StdoutEnvelope struct {
	Tags         []string       `json:"tags,omitempty"`
	CustomFields map[string]any `json:"custom_fields,omitempty"`
}

// Run executes the script against the provided input bytes.
//
// Steps:
//  1. Skip if no script configured or file missing.
//  2. Materialize input + a scratch SUCHI_OUTPUT path under a fresh
//     tmpdir.
//  3. Invoke the script in the sandbox with env DOC_ID, MIME_TYPE,
//     OWNER_EMAIL, SUCHI_OUTPUT.
//  4. On success: parse stdout as StdoutEnvelope (best-effort), and
//     read SUCHI_OUTPUT if the script wrote to it.
func Run(ctx context.Context, input []byte, docID int64, mime, ownerEmail string, log *slog.Logger, opts Options) (*Result, error) {
	log = log.With("component", "preconsume")

	if opts.Script == "" {
		return &Result{Skipped: true}, nil
	}
	// Missing/unexecutable file is not a hard error — the operator
	// might have moved the script mid-flight. Log Warn once at ingest
	// so it shows up in the log but don't fail the doc.
	if fi, err := os.Stat(opts.Script); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Warn("preconsume.skip.missing_script", "path", opts.Script)
			return &Result{Skipped: true, StderrTail: "script missing"}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", opts.Script, err)
	} else if fi.Mode()&0o111 == 0 {
		log.Warn("preconsume.skip.not_executable", "path", opts.Script)
		return &Result{Skipped: true, StderrTail: "script not executable"}, nil
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxOutput := opts.MaxOutput
	if maxOutput == 0 {
		maxOutput = DefaultMaxOutput
	}

	dir, err := os.MkdirTemp("", "suchi-preconsume-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "in")
	outputPath := filepath.Join(dir, "out")
	if err := os.WriteFile(inputPath, input, 0o600); err != nil {
		return nil, fmt.Errorf("write input: %w", err)
	}
	// Pre-touch outputPath so the script can `>` into it without
	// permission surprises. Empty content means "unchanged".
	if err := os.WriteFile(outputPath, nil, 0o600); err != nil {
		return nil, fmt.Errorf("touch output: %w", err)
	}

	start := time.Now()
	res, err := sandbox.Run(ctx, sandbox.Opts{
		Args: []string{opts.Script, inputPath},
		Env: map[string]string{
			"DOC_ID":       strconv.FormatInt(docID, 10),
			"MIME_TYPE":    mime,
			"OWNER_EMAIL":  ownerEmail,
			"SUCHI_OUTPUT": outputPath,
			// PATH kept minimal — enough for a shebang shell script to
			// resolve /usr/bin/env, python, curl, etc.
			"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		Timeout:   timeout,
		MaxStdout: DefaultMaxStdout,
		Dir:       dir,
	})
	dur := time.Since(start)
	if err != nil {
		if errors.Is(err, sandbox.ErrTimeout) {
			return &Result{
				StderrTail: tail(res.Stderr),
				Duration:   dur,
				ExitCode:   res.ExitCode,
			}, fmt.Errorf("preconsume timeout after %s", dur)
		}
		log.Warn("preconsume.script_failed",
			"exit", res.ExitCode, "stderr", tail(res.Stderr))
		return &Result{
			StderrTail: tail(res.Stderr),
			Duration:   dur,
			ExitCode:   res.ExitCode,
		}, nil
	}

	out := &Result{
		Duration:   dur,
		ExitCode:   res.ExitCode,
		StderrTail: tail(res.Stderr),
	}

	// Parse stdout JSON envelope. Empty stdout is fine — the script
	// might just be doing a body rewrite.
	trimmed := bytes.TrimSpace(res.Stdout)
	if len(trimmed) > 0 {
		var env StdoutEnvelope
		if err := json.Unmarshal(trimmed, &env); err != nil {
			log.Warn("preconsume.stdout.parse_failed",
				"err", err.Error(), "stdout_tail", tail(res.Stdout))
		} else {
			out.Tags = env.Tags
			out.CustomFields = env.CustomFields
		}
	}

	// If the script wrote modified bytes, use them downstream.
	if fi, err := os.Stat(outputPath); err == nil && fi.Size() > 0 {
		if fi.Size() > maxOutput {
			return nil, fmt.Errorf("preconsume: SUCHI_OUTPUT exceeded %d bytes (%d)", maxOutput, fi.Size())
		}
		b, err := os.ReadFile(outputPath)
		if err != nil {
			return nil, fmt.Errorf("read output: %w", err)
		}
		out.WorkingBytes = b
		log.Info("preconsume.body_rewritten", "bytes", len(b), "took", dur.String())
	} else {
		log.Debug("preconsume.no_body_change", "took", dur.String())
	}
	return out, nil
}

func tail(b []byte) string {
	const n = 512
	if len(b) <= n {
		return strings.TrimSpace(string(b))
	}
	return "..." + strings.TrimSpace(string(bytes.TrimSpace(b[len(b)-n:])))
}
