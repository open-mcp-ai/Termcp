package detach

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDetachAttributes(t *testing.T) {
	attr := &syscall.SysProcAttr{CreationFlags: windows.BELOW_NORMAL_PRIORITY_CLASS}
	cmd := &exec.Cmd{SysProcAttr: attr}
	Detach(cmd)
	want := uint32(windows.BELOW_NORMAL_PRIORITY_CLASS | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if attr.HideWindow || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != want {
		t.Fatal("Detach must merge flags and hide the window on a copy of the caller's attributes")
	}
}
