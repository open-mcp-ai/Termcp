package daemon

import (
	"os/exec"

	platform "github.com/open-mcp-ai/termcp/pkg/detach"
)

func detach(cmd *exec.Cmd) { platform.Detach(cmd) }

// ProcessAlive reports whether the OS can see the process, not who owns it.
func ProcessAlive(pid int) bool { return platform.ProcessAlive(pid) }

// Only failed-start cleanup uses the platform signal/kill. The user-facing
// stop path asks the running termcp instance to shut down over HTTP.
func terminate(pid int) error { return platform.Terminate(pid) }
