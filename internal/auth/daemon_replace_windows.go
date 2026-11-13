//go:build windows

package auth

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

func replaceLocalDaemonCredential(tempName, target string) error {
	from, err := windows.UTF16PtrFromString(tempName)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	const flags = windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH
	for attempt := 0; attempt < 6; attempt++ {
		err = windows.MoveFileEx(from, to, flags)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		time.Sleep(time.Duration(5*(1<<attempt)) * time.Millisecond)
	}
	return err
}
