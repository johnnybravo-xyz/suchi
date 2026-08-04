//go:build unix

package sandbox

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setpgid gives the child its own process group so timeout kills reach
// any forks it spawned. Unix-only; the _windows.go stub is a no-op.
func setpgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// setCancel wires the context-cancel behavior for the child. Go's
// exec.CommandContext default only Kill()'s the LEADER pid; a child
// like `sleep 30` spawned via `sh -c '…; sleep 30'` gets reparented
// to init and outlives us. This Cancel replaces that with a
// process-group SIGKILL, which reaches every fork the child spawned
// (assuming Setpgid=true was applied by setpgid above).
//
// WaitDelay is the grace before Wait forcibly returns after Cancel.
// Small delay is enough for the reaper to notice; a longer delay
// would only extend a hostile process's ability to hold the sandbox
// open past deadline.
//
// This is the fix for the CI-only sandbox test flake: `sh -c 'sleep
// 30'` on Linux CI runners orphans the sleep to init when the shell
// is SIGKILL'd, so Wait blocks the full 30s. Group-kill via -pgid
// terminates the whole tree.
func setCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 100 * time.Millisecond
}

// killGroup best-effort SIGKILLs the process group leader.
// Retained for the belt-and-suspender kill after Run returns —
// setCancel handles the deadline case, but this covers the "we
// noticed a timeout after the fact" branch in Run.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
