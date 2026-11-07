//go:build windows

// THESE ARE STRUCTURE TESTS, NOT EFFECT TESTS, and the distinction is the honest
// part of them.
//
// They assert the flags handed to CreateProcess. Whether a window appears is Windows
// kernel and terminal behaviour, a layer no unit test can reach: the same flag
// suppresses the window under a detached parent and is invisible under a console
// parent. The effect was checked by hand, before and after, on the path that
// reproduces it: `daemon start` (DETACHED_PROCESS) then
// `open <session> --pipe --argv ping ...`. Before the fix a WindowsTerminal window
// appeared, titled with the child's path (`C:\Program Files\Git\usr\bin\sh.exe`);
// after it, the list of titled windows is unchanged. Note that conhost.exe still
// shows up in Task Manager afterwards: the console is still allocated, it simply has
// no window.
//
// The build tag keeps these off ubuntu and macos. It does NOT mean CI misses them:
// .github/workflows/test.yml runs the suite on windows-latest too.

package sshserver

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// setPipeSysProcAttr must ADD the flag, not replace the struct.
//
// This is the case that catches an assignment: a preset flag from elsewhere in the
// process tree would be wiped, and the loss would only show up as a window that
// reappeared or a process group that stopped working — on one OS, long after.
func TestSetPipeSysProcAttrKeepsExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	cmd.SysProcAttr.CreationFlags = windows.CREATE_NEW_PROCESS_GROUP

	setPipeSysProcAttr(cmd)

	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("CREATE_NO_WINDOW was not set: a pipe child still gets a console window")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("an existing flag was wiped; setPipeSysProcAttr must OR, not assign")
	}
	if cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is set; it would also hide a GUI child (`--argv notepad`), which is why the flag is CREATE_NO_WINDOW")
	}
}

// A nil SysProcAttr has to be created first. An implementation that writes
// `cmd.SysProcAttr.CreationFlags |= ...` without the check panics here — and that
// is the state every pipe child is actually in, since nothing sets SysProcAttr on
// that path.
func TestSetPipeSysProcAttrCreatesTheStruct(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	if cmd.SysProcAttr != nil {
		t.Fatal("precondition: exec.Command left SysProcAttr set, so this is not the nil case")
	}

	setPipeSysProcAttr(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is still nil; the flag went nowhere")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("CREATE_NO_WINDOW was not set on the newly created struct")
	}
}
