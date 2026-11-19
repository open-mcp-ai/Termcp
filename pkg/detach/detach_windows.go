package detach

import (
	"errors"
	"fmt"
	"math"
	"syscall"

	"golang.org/x/sys/windows"
)

const maxPID = math.MaxUint32 // DWORD

func setDetached(attr *syscall.SysProcAttr) {
	attr.CreationFlags |= windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	attr.HideWindow = true
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	const stillActive = 259
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

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
