package daemon

// THE PLATFORM LAYER IS pkg/detach NOW (issue #309), and this file is the shim that
// keeps this package's callers — and its tests — on the same three functions.
//
// The implementations moved rather than being copied: `internal/daemon` was the only
// place in the tree that knew how to detach a process on Windows (DETACHED_PROCESS, no
// console, no window) and how to ask whether a pid is alive without treating "another
// user's process" as dead. A second copy in another project is exactly the drift that
// issue #309 is about, so the code lives where anyone can import it and this package
// delegates.

import (
	"os/exec"

	"github.com/open-mcp-ai/termcp/pkg/detach"
)

// ProcessAlive reports whether pid names an existing process.
func ProcessAlive(pid int) bool { return detach.ProcessAlive(pid) }

// detachCmd configures cmd so the started process outlives this one.
func detachCmd(cmd *exec.Cmd) { detach.Detach(cmd) }

// terminate asks the process to stop (SIGTERM on Unix, a hard kill on Windows).
func terminate(pid int) error { return detach.Terminate(pid) }
