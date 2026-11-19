//go:build !linux

package daemon

import "github.com/open-mcp-ai/termcp/pkg/detach"

func processExited(pid int) bool { return !detach.ProcessAlive(pid) }
