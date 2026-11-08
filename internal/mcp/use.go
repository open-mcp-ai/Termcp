package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"golang.org/x/crypto/ssh"
)

// parseUseHost recognizes short-circuit connection targets (#237).
//
// `use <host>`, `use:<host>` and `connect <host>` are explicit locators; a bare
// configured host name is the concise form. An explicit locator always names the
// profile; for a bare name, an existing session id wins to preserve old behavior.
func (s *Server) parseUseHost(raw string) (string, bool) {
	t := strings.Trim(strings.TrimSpace(raw), "`\"'")
	for _, prefix := range []string{"use:", "connect:", "use ", "connect "} {
		if host, ok := strings.CutPrefix(t, prefix); ok {
			host = strings.TrimSpace(host)
			return host, host != ""
		}
	}

	if s.sessMgr != nil && s.sessMgr.Get(t) != nil {
		return "", false
	}
	if t == "internal" {
		return t, true
	}
	if s.sshConfigs != nil {
		if _, err := s.sshConfigs.Load(t); err == nil {
			return t, true
		}
	}
	return "", false
}

// Acquire acquires an SSH client for a session or a `use` host.
// Session targets carry a no-op release; `use` targets hold a pool lease that
// the caller must release after the operation completes.
func (s *Server) acquireSSHTransport(ctx context.Context, target string) (*ssh.Client, func(), error) {
	host, isUse := s.parseUseHost(target)
	if !isUse {
		sess := s.sessMgr.Get(target)
		if sess == nil {
			return nil, nil, fmt.Errorf("session %q not found", target)
		}
		cli := sess.SSHClient()
		if cli == nil {
			return nil, nil, fmt.Errorf("session %q has no active SSH connection", target)
		}
		return cli, func() {}, nil
	}

	cfg := session.Config{Ctx: ctx}
	if host != "internal" {
		if s.sshConfigs == nil {
			return nil, nil, fmt.Errorf("SSH config store is not available")
		}
		entry, err := s.sshConfigs.Load(host)
		if err != nil {
			if errors.Is(err, sshconfig.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				// A MISSING PROFILE IS A CALLER MISTAKE, NOT A SERVER FAULT, and the
				// answer has to say so: `use:nosuchhost` used to answer 500
				// operation_failed, which reads as an outage and invites a retry that
				// cannot succeed. The same error also answers `read`/`write`/`get`.
				return nil, nil, fmt.Errorf("%w: %q. `ls host` lists the saved profiles; a profile name is also accepted without the `use` prefix", sshconfig.ErrNotFound, host)
			}
			return nil, nil, err
		}
		if entry.Kind == sshconfig.KindInternal {
			if s.NoInternal {
				return nil, nil, fmt.Errorf("internal profile is disabled")
			}
		} else {
			remote, err := sshconfig.RemoteFromEntry(entry, s.sshConfigs.ConfigDir(host))
			if err != nil {
				return nil, nil, err
			}
			cfg.Remote = remote
			cfg.SSHConfig = host
		}
	} else if s.NoInternal {
		return nil, nil, fmt.Errorf("internal profile is disabled")
	}

	lease, err := s.sessMgr.AcquireTransport(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return lease.Client, lease.Release, nil
}
