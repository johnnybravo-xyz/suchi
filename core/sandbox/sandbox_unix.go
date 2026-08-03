//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// setpgid gives the child its own process group so timeout kills reach
// any forks it spawned. Unix-only; the _windows.go stub is a no-op.
func setpgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup best-effort SIGKILLs the process group leader.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
