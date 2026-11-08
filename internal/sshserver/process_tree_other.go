//go:build !windows

package sshserver

import "os/exec"

// processTree is the Windows Job Object that owns a session's process tree.
//
// POSIX needs none: `sh -c cmd` REPLACES the shell with cmd (a shell only forks when
// the command is a compound one), so a session there usually owns exactly one process
// and the kill on the connection going away already ends it. cmd.exe and PowerShell
// never exec — they always spawn and wait — which is the reason the mechanism exists
// and the reason it is Windows-only. See process_tree_windows.go.
type processTree struct{}

// newProcessTree is a no-op where there is no job object to create.
func newProcessTree(cmd *exec.Cmd) *processTree { return nil }

// close is a no-op where nothing owns a tree.
func (t *processTree) close() {}
