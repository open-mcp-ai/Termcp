//go:build unix

package detach

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestDetachAttributes(t *testing.T) {
	attr := &syscall.SysProcAttr{Chroot: "/caller"}
	cmd := &exec.Cmd{SysProcAttr: attr}
	Detach(cmd)
	if attr.Setsid || !cmd.SysProcAttr.Setsid || cmd.SysProcAttr.Chroot != attr.Chroot {
		t.Fatal("Detach must set Setsid on a copy of the caller's attributes")
	}
}
