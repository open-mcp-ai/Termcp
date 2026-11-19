// Package detach provides process detachment and PID operations for Unix and
// Windows. A PID can be reused; liveness alone never identifies its owner.
package detach

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Detach configures cmd to start in a new session on Unix, or without a
// console and in a new process group on Windows. Call it before cmd.Start.
// It preserves unrelated SysProcAttr settings and does not redirect standard
// streams. Callers should not request an incompatible terminal or group.
func Detach(cmd *exec.Cmd) {
	var attr syscall.SysProcAttr
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	setDetached(&attr)
	cmd.SysProcAttr = &attr
}

// ProcessAlive reports whether the OS sees pid as an existing process.
// Permission denied counts as present; on Unix an unreaped zombie does too.
func ProcessAlive(pid int) bool {
	return validPID(pid) && processAlive(pid)
}

// Terminate sends SIGTERM on Unix and forcibly terminates a process on
// Windows. Success means the OS accepted the request, not that the process
// has exited. Only call it for a process whose identity is known.
func Terminate(pid int) error {
	if !validPID(pid) {
		return fmt.Errorf("invalid process ID %d", pid)
	}
	return terminate(pid)
}

// Reject process-group selectors and values that truncate to a different
// pid_t/DWORD in the OS call.
func validPID(pid int) bool { return pid > 0 && uint64(pid) <= maxPID }
