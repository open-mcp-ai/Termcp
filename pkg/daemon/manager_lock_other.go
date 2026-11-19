//go:build unix && !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package daemon

import (
	"errors"
	"os"
)

func tryManagerLock(_ *os.File) error {
	return errors.New("daemon instance locking is not supported on this platform")
}
