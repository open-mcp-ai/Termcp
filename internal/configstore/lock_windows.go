//go:build windows

package configstore

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockExclusive(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
}

func unlockExclusive(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

func syncDir(string) error {
	// Windows does not expose a directory fsync with the same semantics. File
	// replacement itself is flushed through the platform rename implementation.
	return nil
}
