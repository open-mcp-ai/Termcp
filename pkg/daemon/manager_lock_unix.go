//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryManagerLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrBusy
	}
	return err
}
