// Package preconsume runs an optional operator script before built-in ingest.
// The script works on scratch files; the CAS original remains unchanged.
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

	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

// Defaults. Overrides read at call time so a config file loaded from
// main.runServe reaches them — a package-init `var = pipeconfig.…(…)`
// would capture env BEFORE LoadFile ran.
func DefaultTimeout() time.Duration {
	return pipeconfig.Duration("SUCHI_PRECONSUME_TIMEOUT", 5*time.Minute)
}
func DefaultMaxStdout() int64 {
	return pipeconfig.Bytes("SUCHI_PRECONSUME_MAX_STDOUT", 1*1024*1024)
}
func DefaultMaxOutput() int64 {
	return pipeconfig.Bytes("SUCHI_PRECONSUME_MAX_OUTPUT", 200*1024*1024)
}

// Options carries per-call knobs.
type Options struct {
	Script    string // path to executable; empty → Skipped
	Timeout   time.Duration
	MaxOutput int64 // max size of SUCHI_OUTPUT file
}

type Document struct {
	ID         int64
	MIME       string
	Filename   string
	OwnerEmail string
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
func Run(ctx context.Context, input []byte, doc Document, log *slog.Logger, opts Options) (*Result, error) {
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
		timeout = DefaultTimeout()
	}
	maxOutput := opts.MaxOutput
	if maxOutput == 0 {
		maxOutput = DefaultMaxOutput()
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
			"DOC_ID":       strconv.FormatInt(doc.ID, 10),
			"FILENAME":     doc.Filename,
			"MIME_TYPE":    doc.MIME,
			"OWNER_EMAIL":  doc.OwnerEmail,
			"SUCHI_OUTPUT": outputPath,
			// PATH kept minimal — enough for a shebang shell script to
			// resolve /usr/bin/env, python, curl, etc.
			"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		Timeout:   timeout,
		MaxStdout: DefaultMaxStdout(),
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
