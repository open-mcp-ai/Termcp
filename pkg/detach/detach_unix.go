//go:build unix

package detach

import (
	"fmt"
	"math"
	"syscall"
)

const maxPID = math.MaxInt32 // pid_t

func setDetached(attr *syscall.SysProcAttr) { attr.Setsid = true }

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func terminate(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
