package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// refuseUseReview enforces a host profile's default review policy on
// operations the capability table marks reviewable.
func (s *Server) refuseUseReview(target, tool string) *mcp.CallToolResult {
	host, isUse := s.parseUseHost(target)
	if !isUse || s.sshConfigs == nil {
		return nil
	}
	c, known := capability.ForTool(tool)
	if !known || !c.Reviewable() {
		return nil
	}
	entry, err := s.sshConfigs.Load(host)
	if err != nil || entry == nil || !sshconfig.EffectiveApproval(entry) {
		return nil
	}
	return toolError(CodeForbidden, "%s", fmt.Sprintf(
		"host %q requires human review (default_approval), but a short-circuit operation has no session to hold that decision. "+
			"`connect %s` first: the operation then enters that session's review queue and a human decides it there.", host, host))
}

// checkUseRunAdmission sends a short-circuit run through the same capability
// admission exit used by every handler.
func (s *Server) checkUseRunAdmission(ctx context.Context, target string) *mcp.CallToolResult {
	req := mcp.CallToolRequest{}
	req.Params.Name = "shell_input"
	req.Params.Arguments = argsOf("shell_id", target)
	bad, _ := s.checkAdmission(ctx, "shell_input", req)
	return bad
}

// ensureUseContainer returns the live `use:<host>` session for a host,
// creating it if one does not already exist (issue #255).
func (s *Server) ensureUseContainer(ctx context.Context, host string) (*session.Session, error) {
	if s.sessMgr == nil {
		return nil, errors.New("no session manager")
	}
	cfg := session.Config{
		Mode:      "pipe",
		NoShell:   true,
		SSHConfig: host,
		UseHost:   true,
	}
	// AN UNKNOWN PROFILE MUST NOT SILENTLY BECOME LOOPBACK. If host is not
	// `internal`, Load must succeed; leaving cfg.Remote nil would start a loopback
	// session and run the caller's command on THIS machine under the typo's name.
	if !sshconfig.IsInternalName(host) {
		if s.sshConfigs == nil {
			return nil, fmt.Errorf("%w: ssh config store not initialized", sshconfig.ErrNotFound)
		}
		entry, err := s.sshConfigs.Load(host)
		if err != nil || entry == nil {
			return nil, fmt.Errorf("%w: %q. `ls host` lists the saved profiles; a profile name is also accepted without the `use` prefix", sshconfig.ErrNotFound, host)
		}
		remote, err := sshconfig.RemoteFromEntry(entry, s.sshConfigs.ConfigDir(host))
		if err != nil {
			return nil, fmt.Errorf("resolve profile %s: %w", host, err)
		}
		cfg.Remote = remote
	}
	return s.sessMgr.FindUseHost(host, cfg)
}

// runUse runs one command on a host's use container.
//
// THIS IS THE WHOLE OF THE SUGAR, and its length is the point. It creates the
// underlying container when the host has none — a `use` target names a host, and a
// host is not something this server already has a session for — spawns the command as a
// real channel inside it, and then hands that channel to the ordinary finish path.
// Validation, the wait, the read, the search, the headers and every error code are
// finishRun's and runOne's, reached through the same call, so the two roads cannot
// answer differently.
//
// WHAT USED TO LIVE HERE WAS A PARALLEL IMPLEMENTATION: a hand-written slot renderer, a
// second grep, and a second flag validator. They drifted exactly where nobody was
// comparing — a window that expired answered 504/`Status: stopped` on this road and
// 200/`Still-Running` on the other; an empty grep here was missing the Content-Range the
// other always sent; the same bad `--grep-limit` got two different sentences. Each was
// invisible to review because each file was internally consistent.
//
// A REAL CHANNEL, created with the LINE spelling: the command came from a person or an
// agent as shell syntax, so pipes and redirects in it must reach the remote shell
// unmangled. Everything downstream is the ordinary channel machinery — buffer, log,
// exit watcher, Web UI tab, `--later` accounting (#255).
func (s *Server) runUse(ctx context.Context, stmt directive.Statement, win termArgs, host string) envelope.Slot {
	// THE GATE IS THE SAME EXIT EVERY OTHER OPERATION USES. This path has no
	// handler to reach, so it asks admission directly with the capability the
	// command belongs to — a profile's default_approval must not depend on which
	// spelling reached the host.
	if bad := s.checkUseRunAdmission(ctx, "use:"+host); bad != nil {
		slot := failureSlot(failureFromResult(bad))
		slot.Headers["Path"] = stmt.Path
		return slot
	}
	// A FILTER MUST NOT RUN ITS COMMAND TO FIND OUT IT WAS INVALID, and this is the
	// shared validator both roads now use, so the refusal cannot arrive later here than
	// it does on a channel.
	if bad := s.checkRunFlags(stmt, win); bad != nil {
		return s.slotFromFailure(ctx, stmt, bad)
	}
	// A DEFERRED CLOSE STOPS NEW WORK (issue #255). `close use:<host> --later`
	// promised the container ends when its running commands do; dispatching one
	// more command would break that promise silently, because the caller sees a
	// normal 200 while the window it is writing into is on its way out. Refuse and
	// name the two ways forward.
	if c := s.sessMgr.LiveUseHost(host); c != nil && c.Closing() {
		return s.slotFromFailure(ctx, stmt, &toolFailure{
			code: CodeConflict,
			msg: fmt.Sprintf("use:%s is closing: `close --later` was called, so it no longer accepts new commands. "+
				"Wait for its running work to finish (it archives by itself), or start a fresh one by closing it now with `close %s`.",
				host, c.ID),
		})
	}
	container, err := s.ensureUseContainer(ctx, host)
	if err != nil {
		// A `USE:` TARGET FAILS IN TWO KINDS: the profile is not there (the
		// caller's argument, answered with the ssh-config code and a next step), or
		// the connection failed (the server's answer). Mapping both to 500
		// operation_failed made a typo in the host name read like an outage.
		code := CodeOperationFailed
		if errors.Is(err, sshconfig.ErrNotFound) {
			code = CodeSSHConfigNotFound
		}
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: code, msg: err.Error()})
	}
	cs, err := container.CreateChildShellLine(stmt.Payload, false, 0, 0, "")
	if err != nil {
		code := CodeOperationFailed
		if errors.Is(err, session.ErrClosing) {
			code = CodeConflict
		}
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: code,
			msg: fmt.Sprintf("open a command channel on %s: %v", host, err)})
	}
	// THE CHANNEL IS THE TARGET FROM HERE ON. The rest of the reply is produced by
	// the same code a channel run reaches, and it needs the handle, not the host: the
	// bytes, the exit state and the grep scan all belong to this channel.
	single := stmt
	single.Target = cs.ID
	slot := s.finishRun(ctx, single, win, 0)
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	// TWO FACTS THAT BELONG TO THIS ROAD AND CANNOT BE DERIVED FROM THE CHANNEL:
	// which host answered, and the handle of the channel the command owns. The second
	// is what a caller needs the moment the window expires — the process is still
	// running, and reading or closing it takes a name. Naming it is also why this road
	// no longer kills the process on timeout: it used to, because it handed back no way
	// to reach it, and that made the same question answer 504 here and 200 there.
	slot.Headers["Host"] = host
	slot.Headers["Channel"] = cs.ID
	return slot
}
