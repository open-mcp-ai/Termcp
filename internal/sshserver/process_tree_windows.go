//go:build windows

package sshserver

import (
	"log/slog"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree is the Windows Job Object that owns one session's process tree.
//
// THE PROBLEM IT SOLVES IS THAT WINDOWS HAS NO SIGHUP (#293). A session spawns one
// process, that process spawns others (`cmd.exe /c python …`), and closing the SSH
// channel only gives the session's stdout/stdin an EOF. A program that does not read
// stdin — a listening port, `ping`, an MCP server — never learns the channel is gone
// and keeps running with nobody able to address it: not `ls` (the channel IS closed),
// not `close` (already closed), and the port it holds makes the next task fail with
// `os error 10048`.
//
// Killing `cmd.Process` does not help, because it is the DIRECT child: killing `cmd.exe`
// or `sh.exe` leaves the grandchildren they spawned. A Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE is the kernel's own answer — every process
// assigned to the job, and everything they spawn afterwards, is terminated when the
// last handle to the job closes. One handle per session, closed when the session ends:
// the tree cannot outlive the channel because the kernel is the one enforcing it.
//
// SHELLS DO NOT EXEC. `sh -c cmd` replaces itself with cmd, so on POSIX a session
// usually owns exactly one process and killing it is the whole tree. cmd.exe and
// PowerShell never do that — they always spawn and wait — which is why this is
// Windows-only rather than a portable process-group kill.
type processTree struct {
	job  windows.Handle
	once sync.Once
}

// newProcessTree puts cmd's process in a kill-on-close job and returns the handle that
// keeps it alive.
//
// Called AFTER the process starts, because a job is assigned to a process handle and
// exec.Cmd does not expose one before Start. Assignment is best-effort by design: a
// wrong or unavailable job (a host policy that forbids nested jobs, an older Windows
// that refuses to add a process already in one) must not fail the session, it only
// means this session closes the way it always did — so the failure is logged and the
// caller gets nil.
func newProcessTree(cmd *exec.Cmd) *processTree {
	if cmd.Process == nil {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		slog.Warn("session tree kill: cannot create a job object", "pid", cmd.Process.Pid, "err", err)
		return nil
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		windows.CloseHandle(job)
		slog.Warn("session tree kill: cannot set kill-on-close", "pid", cmd.Process.Pid, "err", err)
		return nil
	}
	// OpenProcess by pid rather than reusing cmd's handle: os.Process keeps that one
	// private, and only the two rights this call needs are requested.
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		slog.Warn("session tree kill: cannot open the child for assignment", "pid", cmd.Process.Pid, "err", err)
		return nil
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		slog.Warn("session tree kill: cannot add the child to the job", "pid", cmd.Process.Pid, "err", err)
		return nil
	}
	return &processTree{job: job}
}

// close ends the session: closing the last handle is what terminates the tree, so this
// is the kill.
//
// Idempotent, because the two callers are not ordered against each other: a signal
// request can arrive while the session is already ending normally, and closing a
// Handle twice is a double close of a kernel object, not a no-op.
func (t *processTree) close() {
	if t == nil {
		return
	}
	t.once.Do(func() { windows.CloseHandle(t.job) })
}
