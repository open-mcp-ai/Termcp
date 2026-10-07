//go:build windows

package sshserver

import (
	"log/slog"
	"os"
	"os/exec"

	"github.com/charmbracelet/ssh"
	"golang.org/x/sys/windows"
)

func setPtySysProcAttr(cmd *exec.Cmd) {}

// waitChild waits for the child and returns its exit code.
//
// With a PTY, exec.Cmd.Wait must not be called: pty.Start does not manage the
// child through exec.Cmd on Windows. charmbracelet/ssh's own start goroutine
// reaps cmd.Process and writes cmd.ProcessState (and cmd.Err) itself, so a
// second Wait here was a double reap — two unsynchronized writers of
// cmd.ProcessState, which -race reports. The loser of that race also saw "Wait
// was already called" with a nil state, so a Windows PTY session reported the
// 127 fallback instead of the child's real exit code.
//
// Waiting on our own handle to the same process touches no library-owned field.
// If the process cannot be opened, the exit code is unknown and 127 is
// reported, as before.
//
// Why that cannot normally happen: ConPTY's Spawn returns the process handle and
// nothing ever closes it (x/conpty closes only pi.Thread), so the kernel process
// object — and with it the pid — stays alive past the library's own reap, and
// OpenProcess keeps succeeding. That makes this fix depend on an upstream
// handle leak, so the failure is logged instead of swallowed: if a future
// x/conpty closes that handle, every Windows PTY session would report 127 rather
// than its real exit code, with nothing else to notice. TestServer_PtyExitCode
// asserts the number and is the loud half of the guard.
func waitChild(cmd *exec.Cmd, ptyChild bool) int {
	if !ptyChild || cmd.Process == nil {
		// Pipe mode: exec.Cmd started the process itself, so it owns the reap.
		_ = cmd.Wait()
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode()
		}
		return 127
	}
	pid := cmd.Process.Pid
	p, err := os.FindProcess(pid)
	if err != nil {
		slog.Warn("pty child reap: cannot open the process for its exit code", "pid", pid, "err", err)
		return 127
	}
	state, err := p.Wait()
	if err != nil || state == nil {
		slog.Warn("pty child reap: waiting on the process failed", "pid", pid, "err", err)
		return 127
	}
	return state.ExitCode()
}

// applyWindow applies a window-change to the session's PTY.
//
// It must not go through ssh.Pty.Resize: that lands in conpty.ConPty.Resize,
// which caches the new geometry in c.size — an unlocked field.
// charmbracelet/ssh runs its own winch drain calling the same method, and
// drainWindowChanges deliberately runs alongside it (see server.go), so two
// consumers applying windows concurrently wrote that field in parallel and
// -race flagged it.
//
// ResizePseudoConsole is the entire effect anyway; the skipped cache exists for
// ConPty.Size, which nothing calls. The pseudo console handle is fixed at
// allocation, so reading it needs no synchronization, and the Win32 call itself
// is thread-safe.
func applyWindow(pty ssh.Pty, win ssh.Window) error {
	return windows.ResizePseudoConsole(
		windows.Handle(pty.Fd()),
		windows.Coord{X: int16(win.Width), Y: int16(win.Height)},
	)
}
