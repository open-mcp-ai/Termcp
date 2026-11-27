//go:build windows

package detach

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is Windows' STILL_ACTIVE process exit code.
const stillActive = 259

// detach starts the child without a console window and in its own process group, so no
// console event can reach it and it outlives this process.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// ProcessAlive reports whether pid names a live process.
//
// ACCESS DENIED IS ALIVE: a process owned by somebody else refuses the query handle, and
// that refusal proves it exists. Anything else (a handle, then a non-STILL_ACTIVE exit
// code) is the ordinary case.
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

// Terminate ends the process.
//
// IT IS A HARD KILL ON WINDOWS, and that is the platform's answer rather than a
// shortcut: a detached process has no console to send a control event to, and the
// service control manager only manages services. So a Windows daemon's graceful stop is
// an HTTP request (see pkg/daemon.Config.Stop), and this function is the fallback for a
// child that never became reachable — where there is nothing to be graceful about.
func Terminate(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("terminate: pid %d is not a process", pid)
	}
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

// Kill is the same operation on Windows, kept so a caller does not have to know which
// platform it is on: there is nothing softer to fall back to.
func Kill(pid int) error { return Terminate(pid) }
