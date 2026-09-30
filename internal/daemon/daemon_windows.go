//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is Windows' STILL_ACTIVE process exit code.
const stillActive = 259

// detach starts the child without a console window and in its own process
// group, so no console event can reach it and it outlives this process.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// ProcessAlive reports whether pid names a live process. Access denied still
// proves existence (the process belongs to another user).
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// terminate force-terminates the process. Windows offers no signal a detached
// process could receive (console control events need a shared console, and
// the service control manager is out of scope), so this is a hard kill rather
// than the graceful exit SIGTERM triggers elsewhere. It is only reached while
// cleaning up a child that failed to become ready — the user-facing stop goes
// over HTTP (POST /api/daemon/stop) on every platform. The data layer is
// crash-safe: log.bin is written unbuffered, reads tolerate truncation, and
// the next start reaps dead sessions.
func terminate(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	return nil
}
