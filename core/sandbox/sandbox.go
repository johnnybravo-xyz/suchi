// Package sandbox is the shared subprocess-invocation wrapper.
//
// Every child process that parses user-supplied bytes — pdf-inspector,
// ocrmypdf, qpdf, anydoc — runs through Run(ctx, opts). The wrapper
// enforces:
//
//   - a hard context timeout, kill-the-whole-process-group on expiry
//     (child forks don't outlive us);
//   - bounded stdout/stderr capture so a runaway child can't OOM the
//     server with decompression output;
//   - an EMPTY environment by default (opts.Env is an allowlist —
//     PATH is not implicit);
//   - a fresh working directory when opts.Dir is empty, so the child
//     can't scribble over the DB or blob store by accident.
//
// Not yet enforced (opt-in via later Opts fields, arriving with the
// first consumer that needs them):
//
//   - RLIMIT_AS / RLIMIT_CPU / RLIMIT_FSIZE (via prlimit(1) on Linux)
//   - Network isolation (via unshare -n; needs privileges most self-
//     hosters don't want to grant, so it's opt-in per invocation)
//
// The design principle: whatever is universally cheap is on by default.
// Anything OS-specific or privilege-gated waits for a real hostile
// consumer to justify the complexity.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Default caps for output capture. Small enough that a compressor
// bomb doesn't OOM us; big enough that legitimate tools (an ocrmypdf
// invocation, a pdf-inspector JSON blob) fit.
const (
	DefaultStdoutCap = 4 * 1024 * 1024 // 4 MiB
	DefaultStderrCap = 512 * 1024      // 512 KiB
)

// Opts describes a subprocess invocation. Zero-value is not runnable.
type Opts struct {
	// Args is the argv for the child. Args[0] is the executable path
	// (or a name resolvable through PATH — the wrapper does NOT set PATH
	// automatically; if you need PATH, put it in Env).
	Args []string

	// Timeout bounds the child's wall-clock time. Required — no default
	// because a "sensible" default here is a footgun (any consumer that
	// forgot to set it would inherit an ambiguous ceiling).
	Timeout time.Duration

	// Stdin optional. nil means /dev/null.
	Stdin io.Reader

	// MaxStdout / MaxStderr cap the captured output per stream. 0 uses
	// the DefaultStdout/StderrCap. Excess bytes are dropped from the
	// captured buffer but ARE drained from the child's pipe so the
	// child doesn't stall on a full pipe.
	MaxStdout int64
	MaxStderr int64

	// Env is the allowlist. Empty (or nil) means the child inherits no
	// environment at all. Keys are copied verbatim into the child.
	Env map[string]string

	// Dir is the child's working directory. Empty means create a fresh
	// os.TempDir()-rooted directory (removed after Run returns).
	Dir string
}

// Result carries what a completed run produced. Populated even when
// Run returns an error (timeout, non-zero exit) — inspect the fields to
// see what the child managed to emit before it died.
type Result struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int
	Duration        time.Duration
	StdoutTruncated bool
	StderrTruncated bool
	// TimedOut is true when the context (or Timeout) expired before the
	// child finished.
	TimedOut bool
}

// ErrTimeout is returned by Run when the child was killed for timing
// out. Result is still populated so callers can log what was emitted.
var ErrTimeout = errors.New("sandbox: child exceeded timeout")

// Run executes the child described by opts, blocking until it exits or
// the timeout kills it.
//
// Return contract:
//
//   - opts.Args empty / Timeout <= 0                → returns an error
//     without spawning anything.
//   - Child exits 0 within the deadline             → nil error, Result
//     carries stdout/stderr/duration.
//   - Child exits non-zero                          → *exec.ExitError,
//     Result carries what the child wrote.
//   - Deadline expires                              → ErrTimeout, Result
//     carries partial output, TimedOut=true.
//   - Anything else (start failure, pipe error)     → wrapped error.
func Run(ctx context.Context, opts Opts) (*Result, error) {
	if len(opts.Args) == 0 {
		return nil, errors.New("sandbox: Args is empty")
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("sandbox: Timeout must be > 0")
	}
	maxStdout := opts.MaxStdout
	if maxStdout <= 0 {
		maxStdout = DefaultStdoutCap
	}
	maxStderr := opts.MaxStderr
	if maxStderr <= 0 {
		maxStderr = DefaultStderrCap
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// A fresh dir for the child, unless the caller pinned one. Removed
	// on return so a crashing tool doesn't leak scratch.
	dir := opts.Dir
	if dir == "" {
		td, err := os.MkdirTemp("", "suchi-sandbox-")
		if err != nil {
			return nil, fmt.Errorf("mkdtemp: %w", err)
		}
		dir = td
		defer os.RemoveAll(td)
	}

	cmd := exec.CommandContext(runCtx, opts.Args[0], opts.Args[1:]...)
	cmd.Dir = dir
	cmd.Env = envFromMap(opts.Env)
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}

	// Own process group so a timeout kills the whole tree, and wire
	// the context-cancel to signal the whole group (not just the
	// leader). Without setCancel, a child like `sh -c '...; sleep 30'`
	// would leave `sleep` orphaned to init and the sandbox would
	// block until sleep's natural exit — the exact bug CI caught.
	setpgid(cmd)
	setCancel(cmd)

	stdout := &capBuf{max: maxStdout}
	stderr := &capBuf{max: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	// On timeout, exec.CommandContext already SIGKILL'd the leader. We
	// belt-and-suspender with a group kill in case a fork slipped away.
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if timedOut {
		killGroup(cmd)
	}

	res := &Result{
		Stdout:          stdout.buf.Bytes(),
		Stderr:          stderr.buf.Bytes(),
		Duration:        dur,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		TimedOut:        timedOut,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if timedOut {
		return res, ErrTimeout
	}
	return res, runErr
}

// envFromMap flattens the allowlist into the "KEY=VALUE" slice
// exec.Cmd wants. A nil map yields nil, which means the child inherits
// nothing (NOT the parent's env — exec.Cmd's contract is that nil Env
// means "use os.Environ()", so we explicitly return an empty slice
// there to force isolation).
func envFromMap(m map[string]string) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// capBuf is a bounded io.Writer. Above the cap it discards bytes but
// keeps counting so callers can see how much overflowed.
type capBuf struct {
	max       int64
	n         int64
	buf       bytes.Buffer
	truncated bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	remain := c.max - c.n
	c.n += int64(len(p))
	switch {
	case remain <= 0:
		c.truncated = true
	case int64(len(p)) > remain:
		c.buf.Write(p[:remain])
		c.truncated = true
	default:
		c.buf.Write(p)
	}
	// Pretend we consumed everything — otherwise the child sees a short
	// write and may retry / SIGPIPE.
	return len(p), nil
}
