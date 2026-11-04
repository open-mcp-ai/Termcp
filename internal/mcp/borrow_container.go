package mcp

import (
	"context"
	"log/slog"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// ensureBorrowedContainer returns the live `use:<host>` session for a host,
// creating it if one does not already exist (issue #255).
//
// THE CONTAINER IS A REAL CONNECTION. It carries no shell (NoShell), so nothing
// runs on the target until a command is recorded or someone drives it; its
// liveness is the connection, exactly like an extension's no-shell session. The
// first `run use:<host>` pays for the dial; every later one reuses it.
func (s *Server) ensureBorrowedContainer(ctx context.Context, host string) *session.Session {
	if s.sessMgr == nil {
		return nil
	}
	cfg := session.Config{
		Mode:      api.ModePipe,
		NoShell:   true,
		SSHConfig: host,
	}
	if !sshconfig.IsInternalName(host) && s.sshConfigs != nil {
		entry, err := s.sshConfigs.Load(host)
		if err == nil && entry != nil {
			remote, err := sshconfig.RemoteFromEntry(entry, s.sshConfigs.ConfigDir(host))
			if err == nil {
				cfg.Remote = remote
			}
		}
	}
	sess, err := s.sessMgr.FindBorrowed(host, cfg)
	if err != nil {
		// Visible is secondary to working: if the container cannot be started (a
		// dial failure on an unreachable profile), log it and let the pooled run
		// path attempt its own connection and report the real error.
		slog.Debug("could not ensure borrowed container", "host", host, "err", err)
		return nil
	}
	return sess
}

// recordBorrowedRun notes one finished command against the host's container.
//
// A MISSING CONTAINER IS NOT CREATED HERE. Creating one would dial the host after
// the command has already finished - a connection opened to file a record is a
// connection nobody asked for, and on an unreachable host it would turn a clean
// failure report into a second hang. The container starts on the next successful
// run; that run's record is the first one it holds.
func (s *Server) recordBorrowedRun(host, command string, exitCode int, startedAt int64) {
	if s.sessMgr == nil {
		return
	}
	container := s.sessMgr.LiveBorrowed(host)
	if container == nil {
		return
	}
	if err := container.RecordBorrowedCommand(command, exitCode, startedAt); err != nil {
		slog.Debug("record borrowed command failed", "host", host, "err", err)
	}
}
