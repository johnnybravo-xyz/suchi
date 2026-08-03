package sandbox_test

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/suchi-dms/suchi/core/sandbox"
)

// Tests rely on /bin/sh being available. Skip elsewhere — Windows CI
// exercises the compile path via the build matrix, not this test.
func requireSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
}

func TestRunExitZero(t *testing.T) {
	requireSh(t)
	res, err := sandbox.Run(context.Background(), sandbox.Opts{
		Args:    []string{"/bin/sh", "-c", "echo hello; echo err 1>&2"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hello" {
		t.Errorf("stdout=%q, want hello", got)
	}
	if got := strings.TrimSpace(string(res.Stderr)); got != "err" {
		t.Errorf("stderr=%q, want err", got)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit=%d, want 0", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut should be false")
	}
}

func TestRunExitNonZeroKeepsOutput(t *testing.T) {
	requireSh(t)
	res, err := sandbox.Run(context.Background(), sandbox.Opts{
		Args:    []string{"/bin/sh", "-c", "echo halfway; exit 3"},
		Timeout: 2 * time.Second,
	})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("want *exec.ExitError, got %T (%v)", err, err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit=%d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(string(res.Stdout)) != "halfway" {
		t.Errorf("stdout=%q, want halfway", res.Stdout)
	}
}

func TestRunTimeout(t *testing.T) {
	requireSh(t)
	start := time.Now()
	res, err := sandbox.Run(context.Background(), sandbox.Opts{
		Args:    []string{"/bin/sh", "-c", "echo before; sleep 30"},
		Timeout: 250 * time.Millisecond,
	})
	dur := time.Since(start)
	if !errors.Is(err, sandbox.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut should be true")
	}
	if dur > 3*time.Second {
		t.Errorf("took %v — kill didn't reach the child", dur)
	}
	// "before" should still be in stdout — the child had time to emit it.
	if !strings.Contains(string(res.Stdout), "before") {
		t.Errorf("stdout=%q, want to contain 'before'", res.Stdout)
	}
}

func TestOutputCapTruncates(t *testing.T) {
	requireSh(t)
	// Write ~64 KiB to stdout; cap it at 4 KiB.
	res, err := sandbox.Run(context.Background(), sandbox.Opts{
		Args:      []string{"/bin/sh", "-c", "head -c 65536 /dev/urandom | base64"},
		Timeout:   3 * time.Second,
		MaxStdout: 4096,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated should be true — cap didn't fire")
	}
	if int64(len(res.Stdout)) > 4096 {
		t.Errorf("stdout buffer = %d bytes, want <= 4096", len(res.Stdout))
	}
}

func TestChildDoesNotInheritParentEnv(t *testing.T) {
	requireSh(t)
	// Prove the child cannot see a var set in our environment. Some
	// shells inject their own defaults (PWD, SHLVL, IFS) so counting
	// env lines is unreliable — assert the specific parent leak we
	// care about instead.
	t.Setenv("SUCHI_PARENT_ONLY", "parent-secret")
	res, err := sandbox.Run(context.Background(), sandbox.Opts{
		Args:    []string{"/bin/sh", "-c", "echo ${SUCHI_PARENT_ONLY:-unset}"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "unset" {
		t.Errorf("child saw parent env: %q — want 'unset'", got)
	}
}

func TestEnvAllowlist(t *testing.T) {
	requireSh(t)
	res, err := sandbox.Run(context.Background(), sandbox.Opts{
		Args:    []string{"/bin/sh", "-c", "echo ${SUCHI_TEST:-unset}"},
		Timeout: 2 * time.Second,
		Env:     map[string]string{"SUCHI_TEST": "yep"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "yep" {
		t.Errorf("stdout=%q, want yep", res.Stdout)
	}
}

func TestBadOpts(t *testing.T) {
	if _, err := sandbox.Run(context.Background(), sandbox.Opts{Timeout: time.Second}); err == nil {
		t.Error("empty Args accepted")
	}
	if _, err := sandbox.Run(context.Background(), sandbox.Opts{Args: []string{"/bin/true"}}); err == nil {
		t.Error("zero Timeout accepted")
	}
}
