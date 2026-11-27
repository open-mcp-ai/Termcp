//go:build !windows

package detach

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// detach puts the child in its own session so it survives this process and has no
// controlling terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// ProcessAlive reports whether pid names an existing process.
//
// EPERM IS ALIVE: `kill(pid, 0)` on a process owned by another user fails with EPERM,
// which proves the process EXISTS — the question here is liveness, not permission. A
// pid of 0 or less is refused rather than passed to the kernel, where 0 means "every
// process in my group" and a negative pid means "a process group": either would answer
// about the wrong thing, and the asker wanted one process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Terminate asks the process to stop, and it is a REQUEST: SIGTERM is catchable, so a
// program with a shutdown path runs it (a server flushes, a session marks itself dead).
// Use Kill only when Terminate has been ignored and the process must go.
func Terminate(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("terminate: pid %d is not a process", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}

// Kill stops the process without asking.
func Kill(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("kill: pid %d is not a process", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
