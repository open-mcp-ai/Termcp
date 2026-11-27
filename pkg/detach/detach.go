// Package detach starts a program as a background process and controls it by pid.
//
// IT EXISTS BECAUSE THIS IS THE PART THAT IS EASY TO GET WRONG AND IMPOSSIBLE TO GET
// RIGHT TWICE. Launching a daemon is three syscalls and a paragraph of platform
// knowledge — a Unix child needs its own session so it survives the parent's terminal
// (and gets no controlling terminal to be hung up on), a Windows child needs
// DETACHED_PROCESS so no console event reaches it; liveness is `kill(pid, 0)` on one
// platform and OpenProcess + GetExitCodeProcess on the other; and stopping is a signal
// on one and a hard kill on the other, because Windows has no signal a detached process
// can receive. Every project that needs a background process writes this code, and every
// copy of it drifts.
//
// NOTHING HERE KNOWS WHAT THE PROCESS IS. No configuration type, no protocol, no
// logging: a pid and an *exec.Cmd, so the caller keeps its own opinions (issue #309).
// The lifecycle on top of it — spawn, wait for readiness, stop — lives in
// github.com/open-mcp-ai/termcp/pkg/daemon and takes the same position.
package detach

import "os/exec"

// Detach configures cmd so the started process outlives this one.
//
// IT MUST BE CALLED BEFORE cmd.Start. On Unix it sets Setsid (a new session, so the
// child has no controlling terminal and is not killed when the parent's terminal goes
// away); on Windows it sets DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP and HideWindow, so
// no console event can reach the child and no window flashes up.
//
// The child's standard streams are the caller's business: a detached process that
// inherits the parent's stdout writes into a terminal that has moved on, which is why
// pkg/daemon points them at a log file.
func Detach(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	detach(cmd)
}
