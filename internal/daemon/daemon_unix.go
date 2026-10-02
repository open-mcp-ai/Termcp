//go:build !windows

package daemon

import (
	"fmt"
	"os/exec"
	"syscall"
)

// detach makes the child its own session leader so it survives this process
// and has no controlling terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// ProcessAlive reports whether pid names an existing process. EPERM means the
// process exists but belongs to another user.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// terminate asks the process to stop; termcp's signal handler then exits
// cleanly (the same path as Ctrl+C on the foreground server).
func terminate(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
