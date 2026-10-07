//go:build !windows

package sshserver

import (
	"os/exec"
	"syscall"

	"github.com/charmbracelet/ssh"
)

func setPtySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

// waitChild waits for the child and returns its exit code. ptyChild only matters
// on Windows, where pty.Start hands the child's lifecycle to ConPTY (see
// server_windows.go). On Unix pty.Start is plain c.Start, so exec.Cmd owns the
// process on every path: cmd.Wait is the single writer and its own reader.
func waitChild(cmd *exec.Cmd, _ bool) int {
	_ = cmd.Wait()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return 127
}

// applyWindow applies a window-change to the session's PTY. On Unix this is an
// ioctl on the master fd — no shared Go state — so it goes through the
// library's Resize. Windows has to route around that method (see
// applyWindow in server_windows.go).
func applyWindow(pty ssh.Pty, win ssh.Window) error {
	return pty.Resize(win.Width, win.Height)
}
