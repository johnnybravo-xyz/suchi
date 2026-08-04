//go:build windows

package sandbox

import "os/exec"

// setpgid is a no-op on Windows. CommandContext handles kills via
// TerminateProcess when the deadline fires — good enough until we
// ship a Windows-native ingest pipeline.
func setpgid(cmd *exec.Cmd) {}

// setCancel is a no-op on Windows — exec.CommandContext's default
// Kill() reaches the immediate child via TerminateProcess, and Job
// Objects handle the tree kill semantics we care about there. See
// setpgid.
func setCancel(cmd *exec.Cmd) {}

// killGroup is a no-op on Windows. See setpgid.
func killGroup(cmd *exec.Cmd) {}
