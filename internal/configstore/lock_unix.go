//go:build darwin || linux

package configstore

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockExclusive(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func unlockExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Sync()
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	return err
}
