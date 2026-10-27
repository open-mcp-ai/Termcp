//go:build !darwin && !linux && !windows

package configstore

import (
	"errors"
	"os"
)

func lockExclusive(*os.File) error {
	return errors.New("configstore: directory locking is not supported on this platform")
}

func unlockExclusive(*os.File) error {
	return nil
}

func syncDir(string) error {
	return nil
}
