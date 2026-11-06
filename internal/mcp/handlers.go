package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/jrpc"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func validateStartParams(args map[string]any) (*mcpgo.CallToolResult, error) {
	mode := strings.TrimSpace(getString(args, "mode", "pty"))
	if mode == "" {
		mode = "pty"
	}
	if mode != "pty" && mode != "pipe" {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("mode must be 'pty' or 'pipe', got %q", mode)), nil
	}
	rows := int(getFloat64(args, "rows", 24))
	if rows < 1 || rows > 1000 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("rows must be between 1 and 1000, got %d", rows)), nil
	}
	cols := int(getFloat64(args, "cols", 80))
	if cols < 1 || cols > 1000 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("cols must be between 1 and 1000, got %d", cols)), nil
	}
	if p := strings.TrimSpace(strings.ToLower(getString(args, "on_exit", ""))); p != "" && p != string(session.OnExitKeep) && p != string(session.OnExitClose) {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("on_exit must be 'keep' or 'close', got %q", p)), nil
	}
	return nil, nil
}

// filterRunning returns only sessions whose Status is SessionRunning.
func filterRunning(in []api.Session) []api.Session {
	out := make([]api.Session, 0, len(in))
	for _, s := range in {
		if s.Status == api.SessionRunning {
			out = append(out, s)
		}
	}
	return out
}

// listableSessions is what a session list shows: the running sessions that belong
// to the caller. A session in an infrastructure namespace was created by a
// subsystem — the extension layer keeps one per host — and is reached through that
// subsystem's own handle (extension(action=list) hands out its session_id and
// shell_id) rather than discovered in a list. Its id still resolves everywhere an
// id is accepted, so a handle already in hand keeps working.
//
// callerNS is the caller's own namespace, empty for the operator. A caller that
// claimed one sees exactly its own sessions — an extension session belongs to the
// extension layer, so a tenant's list does not grow one just because the tenant
// is the one asking. Listing and by-id reach both go through ns.OwnedBy, so the
// two cannot disagree about what a caller may touch.
func listableSessions(in []api.Session, callerNS string) []api.Session {
	out := make([]api.Session, 0, len(in))
	for _, s := range in {
		if s.Status != api.SessionRunning {
			continue
		}
		if !ns.Listable(s.Namespace, callerNS) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// requireSession resolves a session_id argument into the session it names. It is
// the one place a session is looked up for a tool argument, so every tool accepts
// the same spellings and acts on the same canonical object.
//
// Accepted: a raw session id, a termcp:// session locator, a termcp:// shell
// locator (its session part), or a raw shell id (both the root and child shells
// sessionByName resolves a display name to exactly one session, or explains the
// ambiguity (issue #247).
//
// Three answers, and all three are useful: one match is the session; no match is nil
// with no error, so the caller's own not-found message still names what it was given;
// several matches is a refusal listing the candidates, because every candidate is
// reachable by id and choosing for the caller is how a `close` lands on the wrong one.
func (s *Server) sessionByName(name string) (*session.Session, *mcpgo.CallToolResult) {
	matches := s.sessMgr.SessionsNamed(name)
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	}
	ids := make([]string, 0, len(matches))
	running := 0
	for _, m := range matches {
		info := m.Info()
		ids = append(ids, info.ID)
		if info.Status == api.SessionRunning {
			running++
		}
	}
	sort.Strings(ids)
	return nil, toolError(CodeInvalidArgument, "%s", fmt.Sprintf(
		"%d sessions are named %q, so the name does not say which one: %s. "+
			"Address one by id, or give the next one a distinct --name (`connect <host> --name web`).",
		len(matches), name, strings.Join(ids, ", ")))
}

// are stored with their parent, so a shell id names its container).
//
// Callers must use the RETURNED session's ID. Echoing the argument back would
// leak a locator into fields that are later used as ids — a forward stores its
// SessionID for the DEAD cascade, and a locator there never matches the real id.
func (s *Server) requireSession(sessionID string) (*session.Session, *mcpgo.CallToolResult) {
	if sess := s.sessMgr.Get(sessionID); sess != nil {
		return sess, nil
	}
	if sess := s.sessMgr.GetByShellID(sessionID); sess != nil {
		return sess, nil
	}
	// The short handle `sid:N` names a session too (plus one of its channels),
	// which is what makes `info`, `close` and the namespace check agree with
	// `read` about a handle copied out of `ps`.
	if h := locator.SplitHandle(sessionID); h.Index > 0 {
		if sess := s.sessMgr.Get(h.SessionID); sess != nil {
			return sess, nil
		}
	}
	// A NAME ADDRESSES A SESSION (issue #247), and an ambiguous name is refused
	// rather than guessed: Name defaults to the profile name, so "internal" names
	// every loopback session nobody labelled. Picking one would let `close internal`
	// kill a session the caller was not looking at, and the reply would say success.
	if sess, bad := s.sessionByName(sessionID); bad != nil || sess != nil {
		return sess, bad
	}
	if looksLikeResourceLocator(sessionID) {
		p, err := parseResourceURL(sessionID)
		if err != nil {
			return nil, toolError(CodeInvalidArgument, "%s", err.Error())
		}
		switch p.Kind {
		case resourceURLEntry:
			return nil, toolError(CodeInvalidArgument, "%s", fmt.Sprintf("resource URL %q names an entry, not a session", sessionID))
		case resourceURLSession, resourceURLShell:
			if sess := s.sessMgr.Get(p.SessionID); sess != nil {
				return sess, nil
			}
			return nil, toolError(CodeSessionNotFound, "%s", s.sessionNotFoundMessage(p.SessionID))
		}
	}
	return nil, toolError(CodeSessionNotFound, "%s", s.sessionNotFoundMessage(sessionID))
}

// sessionNotFoundMessage is the 404 for a session the registry does not hold.
//
// "Session 'x' not found" is true and useless: it is the same sentence for a typo, for an
// id this process never loaded, and for an id that lives in another instance's data
// directory. The caller cannot tell which, and the three call for different next moves —
// so when a record for that id IS on disk, the reply says so (issue #176).
//
// The check costs one manifest read, and only on the failure path.
func (s *Server) sessionNotFoundMessage(sessionID string) string {
	msg := fmt.Sprintf("Session '%s' not found", sessionID)
	if s.sessMgr == nil {
		return msg
	}
	// AN INSTANCE THAT WAS CLOSED ON PURPOSE IS NOT A TYPO (issue #183). An
	// on-demand extension is closed when nothing has used it for its idle window,
	// and the caller holding the old handle would otherwise read the same
	// not-found a mistyped id gets — the two need different next moves, so the
	// daemon that took it away says so.
	if s.extensions != nil {
		if name, reason, at, ok := s.extensions.ExplainClosed(sessionID, ""); ok {
			return fmt.Sprintf(
				"Session '%s' was closed at %s: the extension %q %s. "+
					"This is not a typo and not a failure — `open ext %s` starts a new instance and returns its handles.",
				sessionID, at.Format("15:04:05"), name, reason, name)
		}
	}
	sess := s.sessMgr.PersistedSession(sessionID)
	if sess == nil {
		return msg
	}
	// The record is on disk but not in this process. Two roads: a second termcp is using
	// this data directory (its `ls` lists this session, this one's does not), or this
	// process did not restore it. Either way the id is not a typo, which is the fact the
	// bare 404 was hiding.
	return fmt.Sprintf(
		"%s in this process, but its record is on disk (status %s, last written %s). "+
			"The id is not a typo: either another termcp instance is using this data directory, "+
			"or this one did not restore it. `ls --all` lists what this process knows.",
		msg, sess.Status, ageOf(sess.UpdatedAt))
}

// splitBatchIDs splits the comma-separated form of a session_id argument, which
// the lifecycle commands (session_terminate / session_delete) accept. A value
// with no comma is a single id and isBatch stays false, so the caller keeps the
// single-session code path and its exact error shapes. Whitespace around
// entries is trimmed; empty entries (stray commas) are dropped.
func splitBatchIDs(v string) (ids []string, isBatch bool) {
	if !strings.Contains(v, ",") {
		return []string{v}, false
	}
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			ids = append(ids, part)
		}
	}
	return ids, true
}

// batchSessionResult runs op over every id in a batch, reporting one outcome per
// entry: {"session_id":...,"ok":true} or {"session_id":...,"ok":false,
// "code":...,"error":...}. Entries resolve independently, so one missing id (or
// a locator that no longer resolves) never aborts the rest — the caller sees
// exactly which entries succeeded. The result echoes the argument text; op
// receives the resolved raw id (a locator entry resolves to its session).
func (s *Server) batchSessionResult(ids []string, op func(id string) error) *mcpgo.CallToolResult {
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		sess, bad := s.requireSession(id)
		if bad != nil {
			results = append(results, map[string]any{
				"session_id": id,
				"ok":         false,
				"code":       CodeSessionNotFound,
				"error":      fmt.Sprintf("Session '%s' not found", id),
			})
			continue
		}
		if err := op(sess.ID); err != nil {
			results = append(results, map[string]any{
				"session_id": id,
				"ok":         false,
				"code":       CodeOperationFailed,
				"error":      err.Error(),
			})
			continue
		}
		results = append(results, map[string]any{"session_id": id, "ok": true})
	}
	return jsonResult(map[string]any{"results": results})
}

// requireRunningSession resolves a session and rejects closed (DEAD) sessions:
// a DEAD session is a read-only record — its output stays readable via
// shell_output, and nothing new is created on it. The check is the session
// status, not transport liveness: a pipe session whose command exited cleanly is
// DEAD while its SSH client is still open until Delete, and it must be refused
// exactly like a terminated, disconnected, or restored one.
func (s *Server) requireRunningSession(sessionID string) (*session.Session, *mcpgo.CallToolResult) {
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return nil, bad
	}
	if info := sess.Info(); info.Status != api.SessionRunning {
		return nil, toolError(CodeSessionNotRunning, "%s", fmt.Sprintf("Session '%s' is closed (status %s); this operation needs a running session — read its output with `read`", sessionID, info.Status))
	}
	return sess, nil
}

// sshClientForSession resolves a running session and returns its live SSH client.
// Closed (DEAD) sessions — whether just terminated or restored after a restart —
// return session_not_running so SFTP/forward internals never see a dead transport.
func (s *Server) sshClientForSession(sessionID string) (*session.Session, *ssh.Client, *mcpgo.CallToolResult) {
	sess, bad := s.requireRunningSession(sessionID)
	if bad != nil {
		return nil, nil, bad
	}
	cli := sess.SSHClient()
	if cli == nil {
		return nil, nil, toolError(CodeSessionNotRunning, "%s", fmt.Sprintf("Session '%s' is not running or has no active SSH connection", sessionID))
	}
	return sess, cli, nil
}

// dialerForSession returns a jrpc.DialFunc that reaches the network THROUGH the
// session's SSH connection.
//
// THIS IS WHAT A GATEWAY URL MEANS. "localhost:9222" passed to `cdp open` names a
// port on the machine the session is connected to, not on the machine termcp runs on.
// Every gateway (mcp, cdp, jrpc) therefore needs a session before it can have a URL,
// and the dialer is the only place that distinction is made.
//
// An SSH connection that has already gone away answers here rather than at the far
// end of a five-second TCP timeout: the caller is told the session is not running,
// which is the true reason and the one it can act on.
func (s *Server) dialerForSession(name string) (jrpc.DialFunc, *mcpgo.CallToolResult) {
	sess, bad := s.resolveTunnelSession(name)
	if bad != nil {
		return nil, bad
	}
	cli := sess.SSHClient()
	if cli == nil {
		return nil, toolError(CodeSessionNotRunning, "%s", fmt.Sprintf(
			"session %s has no live SSH connection, so there is no tunnel", sess.ID))
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Wrapped because an SSH channel has no deadlines: see jrpc's
		// deadlineFreeConn for why that is dropped rather than propagated.
		conn, err := cli.Dial(network, addr)
		if err != nil {
			return nil, err
		}
		return jrpc.NoDeadlineConn(conn), nil
	}, nil
}

// resolveTunnelSession accepts EITHER a session id or a host name.
//
// `mcp open` needs a session, and the two names a caller has at that moment are
// different things: an id (64e9e022-428) that `ps` just printed, and a host
// (internal, prod) that it typed from memory. Requiring the id would make every
// gateway call a two-step lookup for no reason — the id identifies a LINK, but what
// the caller actually means is "the network of prod", and there is usually exactly
// one running link per host.
//
// So a host name resolves to that host's running session when there is exactly one.
// Two running links to the same host is genuinely ambiguous — they can differ in jump
// host, proxy or user — and answering with either would silently pick a network the
// caller did not choose. That case names both ids and lets the caller say which.
func (s *Server) resolveTunnelSession(name string) (*session.Session, *mcpgo.CallToolResult) {
	if sess, bad := s.requireSession(name); bad == nil {
		return sess, nil
	}
	// Not an id: try it as a host name.
	var matches []*session.Session
	for _, info := range s.sessMgr.ListAll() {
		if info.SSHConfig != name || info.Status != api.SessionRunning {
			continue
		}
		if live := s.sessMgr.Get(info.ID); live != nil {
			matches = append(matches, live)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, toolError(CodeSessionNotRunning, "%s", fmt.Sprintf(
			"no running session for %q. A gateway tunnels through a session, so open one first: `pty %s`. `ps` lists what exists.",
			name, name))
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return nil, toolError(CodeInvalidArgument, "%s", fmt.Sprintf(
			"%q has %d running sessions, so the tunnel target is ambiguous: %s. Pass one of those ids instead.",
			name, len(matches), strings.Join(ids, ", ")))
	}
}

// sshClientOnly is sshClientForSession without the session record, for callers that
// need the transport and not the metadata.
func (s *Server) sshClientOnly(sessionID string) (*ssh.Client, *mcpgo.CallToolResult) {
	_, cli, bad := s.sshClientForSession(sessionID)
	return cli, bad
}

// sftpClient preserves the session-only helper used by tests and callers that
// already own the session lifecycle. Production directives use sftpClientContext
// so a borrowed transport lease can be released alongside the SFTP channel.
func (s *Server) sftpClient(target string) (*sftp.Client, *mcpgo.CallToolResult) {
	cli, release, bad := s.sftpClientContext(context.Background(), target)
	if bad != nil {
		return nil, bad
	}
	if _, borrowed := s.parseBorrowedHost(target); borrowed {
		_ = cli.Close()
		release()
		return nil, toolError(CodeInvalidArgument, "%s", "borrowed SFTP requires a request-scoped transport lease")
	}
	return cli, nil
}

// sftpClientContext resolves a session (or borrowed host, #237) and returns an SFTP
// client over it. The caller closes the SFTP client and invokes release.
//
// A SESSION'S ENGINE IS REUSED; A BORROWED HOST'S IS NOT (issue #244). The reuse is what
// removes three round trips — channel open, subsystem request, INIT/VERSION — from every
// file operation. A borrowed host keeps the old per-call engine because its transport
// lease is request-scoped: caching one would hand a later call a transport that has
// already been released, which is a correctness bug traded for a latency win.
//
// The shared Client's Close is a no-op (see sftp.Shared), so every existing
// `defer sftpCli.Close()` in the handlers stays correct without knowing which kind it
// got. That is deliberate: the alternative was editing twelve handlers and trusting each
// one to remember which branch it was on.
func (s *Server) sftpClientContext(ctx context.Context, target string) (*sftp.Client, func(), *mcpgo.CallToolResult) {
	var sshCli *ssh.Client
	var release func()
	if host, borrowed := s.parseBorrowedHost(target); borrowed {
		var err error
		// The explicit marker is kept across this boundary: `host` is already known
		// to be a borrowed target, and handing the resolver a bare word would make it
		// re-decide (an unknown profile then read as a missing session).
		sshCli, release, err = s.acquireSSHTransport(ctx, "use:"+host)
		if err != nil {
			code := CodeConnectionFailed
			if errors.Is(err, sshconfig.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				code = CodeSSHConfigNotFound
			}
			return nil, nil, toolError(code, "%s", err.Error())
		}
	} else {
		sess, client, bad := s.sshClientForSession(target)
		if bad != nil {
			return nil, nil, bad
		}
		sshCli = client
		release = func() {}
		// The canonical session id, not the caller's spelling: `read <id> file` and
		// `read <name> file` name the same session and must share one engine, and the
		// DEAD hook that evicts it is called with this id.
		view, err := s.sftpEngines.Get(sess.ID, sshCli)
		if err != nil {
			release()
			return nil, nil, toolError(CodeOperationFailed, "%s", fmt.Sprintf("SFTP: %v", err))
		}
		return view, release, nil
	}

	cli, err := sftp.NewClient(sshCli)
	if err != nil {
		release()
		return nil, nil, toolError(CodeOperationFailed, "%s", fmt.Sprintf("SFTP: %v", err))
	}
	return cli, release, nil
}

// requireShell looks up a shell by shell_id for terminal I/O (never session_id).
// The id may also be a termcp:// resource URL (session or shell form), a raw
// session id (→ primary shell), or a raw shell id. Closed (DEAD) sessions are
// rejected: their process is gone, so the only remaining operation is reading
// output via shell_output.
//
// A raw shell id reaches the shell object even after its session went DEAD,
// because the retained shell is still in the live map (it is kept so its tail
// output stays readable). The status check is therefore repeated here for that
// path: without it a write would travel all the way to the (already closed)
// transport and come back as a generic operation failure, which reads like a
// transient error instead of "this session is closed". Reading is not affected —
// shell_output resolves through resolveOutputSource, which is allowed to serve a
// DEAD session from its persisted log.
func (s *Server) requireShell(shellID string) (*session.ChildShell, *mcpgo.CallToolResult) {
	if cs := s.sessMgr.GetChildShell(shellID); cs != nil {
		if sess := s.sessMgr.Get(cs.ParentSessionID()); sess != nil {
			if status := sess.Info().Status; status != api.SessionRunning {
				return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' is closed (status %s); its output is still readable with `read`", sess.ID, status))
			}
		}
		return cs, nil
	}
	if sess := s.sessMgr.Get(shellID); sess != nil {
		if sess.Info().Status != api.SessionRunning {
			return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' is closed (status %s); its output is still readable with `read`", shellID, sess.Info().Status))
		}
		// A SESSION IS NOT A CHANNEL, and resolving it to the primary one is what
		// made `close <session> --force` a silent no-op (#132) and `run <session>`
		// an unstated guess. The caller must say which channel: `sid:1`. The error
		// names the handle to use, because "which channel" is answerable in one
		// line and leave the caller guessing otherwise.
		return nil, toolError(CodeInvalidArgument, "%s",
			fmt.Sprintf("Session '%s' is a container, not a channel. Name the channel explicitly, e.g. '%s:%d', which is what `ps` prints (see also `ps --shells`).",
				shellID, shellID, sess.PrimaryShellIndex()))
	}
	// The locator branch runs BEFORE the short-handle branch, and must keep doing
	// so: both spellings end in `:<N>`, and only this branch distinguishes a
	// malformed locator (invalid_argument) from an unknown id. Its error messages
	// are the ones the resource-URL contract pins.
	if looksLikeResourceLocator(shellID) {
		p, err := parseResourceURL(shellID)
		if err != nil {
			return nil, toolError(CodeInvalidArgument, "%s", err.Error())
		}
		if p.Kind == resourceURLEntry {
			return nil, toolError(CodeInvalidArgument, "%s", fmt.Sprintf("resource URL %q names an entry, not a session or shell", shellID))
		}
		sess, err := s.sessionFromParsed(p)
		if err != nil {
			// Surface the closed-session hint from sessionFromParsed instead of a
			// generic "shell not found".
			return nil, toolError(CodeSessionNotFound, "%s", err.Error())
		}
		cs, err := s.shellFromIndex(sess, p.Index)
		if err != nil {
			return nil, toolError(CodeShellNotFound, "%s", err.Error())
		}
		if cs == nil {
			return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Session '%s' has no shell", sess.ID))
		}
		return cs, nil
	}
	// The short handle `sid:N` that ps/info/pty print, accepted at the one door
	// every terminal directive goes through — so "copy the handle from ps into the
	// next command" holds for all of them. Locators were consumed above, so the
	// `:<N>` reaching this point belongs to a short handle.
	if h := locator.SplitHandle(shellID); h.Index > 0 {
		// `web:1` addresses channel 1 of the session NAMED web (issue #247), so the
		// name resolves before the id does — the id branch below is unchanged, and a
		// name that matches several sessions is refused by sessionByName rather than
		// guessed.
		if byName, bad := s.sessionByName(h.SessionID); bad != nil {
			return nil, bad
		} else if byName != nil {
			if byName.Info().Status != api.SessionRunning {
				return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' is closed (status %s); its output is still readable with `read`", byName.ID, byName.Info().Status))
			}
			cs, err := s.shellFromIndex(byName, h.Index)
			if err != nil {
				return nil, toolError(CodeShellNotFound, "%s", err.Error())
			}
			if cs == nil {
				return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Session '%s' has no shell", byName.ID))
			}
			return cs, nil
		}
		if sess := s.sessMgr.Get(h.SessionID); sess != nil {
			if sess.Info().Status != api.SessionRunning {
				return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' is closed (status %s); its output is still readable with `read`", sess.ID, sess.Info().Status))
			}
			cs, err := s.shellFromIndex(sess, h.Index)
			if err != nil {
				return nil, toolError(CodeShellNotFound, "%s", err.Error())
			}
			if cs == nil {
				return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Session '%s' has no shell", sess.ID))
			}
			return cs, nil
		}
	}
	return nil, toolError(CodeShellNotFound, "%s", s.shellNotFoundMessage(shellID))
}

// shellNotFoundMessage is the 404 for a channel the registry does not hold.
//
// THE HANDLE IS WHAT A CALLER ACTUALLY KEEPS (dogfood on #183). The extension
// status carries closed_at/closed_reason for the id it took away, and the session
// 404 already explains itself — but the channel 404 did not, so an agent holding
// the shell id it was given read `Shell 'x' not found` and had no way to tell a
// reaped instance from a typo. docs/api.md promised the id would explain itself;
// this is the half that makes that true.
func (s *Server) shellNotFoundMessage(shellID string) string {
	msg := fmt.Sprintf("Shell '%s' not found", shellID)
	if s.extensions == nil {
		return msg
	}
	if name, reason, at, ok := s.extensions.ExplainClosed("", shellID); ok {
		return fmt.Sprintf(
			"Shell '%s' was closed at %s: the extension %q %s. "+
				"This is not a typo and not a failure — `open ext %s` starts a new instance and returns its handles.",
			shellID, at.Format("15:04:05"), name, reason, name)
	}
	return msg
}

// resolveSSHFromArgs returns the ssh_config name, loaded entry, and remote dial settings (nil Remote = built-in loopback).
// The ssh_config value may be a profile name or a termcp:// entry resource URL
// (e.g. "termcp://mac"); both resolve to the same profile.
func (s *Server) resolveSSHFromArgs(args map[string]any) (string, *sshconfig.Entry, *session.RemoteSSH, error) {
	if s.sshConfigs == nil {
		return "", nil, nil, fmt.Errorf("ssh config store not configured")
	}
	name := strings.TrimSpace(getString(args, "ssh_config", ""))
	if name == "" {
		return "", nil, nil, errMissingSSHConfig
	}
	if looksLikeResourceLocator(name) {
		p, perr := parseResourceURL(name)
		if perr != nil {
			return "", nil, nil, fmt.Errorf("%w: %v", errInvalidResourceLocator, perr)
		}
		if p.Kind != resourceURLEntry {
			return "", nil, nil, fmt.Errorf("%w: ssh_config %q is a session/shell locator; pass an entry name or termcp://<entry>", errInvalidResourceLocator, name)
		}
		name = p.Entry
	}
	ent, err := s.sshConfigs.Load(name)
	if err != nil {
		return "", nil, nil, err
	}
	if ent.Kind == sshconfig.KindInternal {
		if s.NoInternal {
			return "", nil, nil, fmt.Errorf("internal profile is disabled")
		}
		return name, ent, nil, nil
	}
	r, err := sshconfig.RemoteFromEntry(ent, s.sshConfigs.ConfigDir(name))
	if err != nil {
		return "", nil, nil, err
	}
	return name, ent, r, nil
}

// batchShellResult runs op over every shell id in a batch, one outcome per entry.
//
// It is the shell-side twin of batchSessionResult, and it exists for the same
// reason: a batch must not be all-or-nothing, because "type into all ten shells"
// is precisely a case where one dead shell should not stop the other nine.
//
// Each entry carries whatever op returned plus shell_id and ok, so a caller can
// tell which shells took the bytes and which refused. `results` is always
// present, even for one entry, because a batch that silently degraded to a single
// result would be indistinguishable from a single call.
func (s *Server) batchShellResult(ids []string, op func(id string) (map[string]any, *mcpgo.CallToolResult)) *mcpgo.CallToolResult {
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		entry := map[string]any{"shell_id": id}
		out, bad := op(id)
		if bad != nil {
			entry["ok"] = false
			// unwrapToolError yields the human-readable message from the tool's
			// error envelope; a reply that is not one passes through unchanged.
			if txt := errorReplyText(bad); txt != "" {
				entry["error"] = txt
			}
		} else {
			entry["ok"] = true
			for k, v := range out {
				entry[k] = v
			}
		}
		results = append(results, entry)
	}
	return jsonResult(map[string]any{"results": results})
}

// errorReplyText is the message inside an error reply, or "" when there is none.
//
// Deliberately not toolResultText: that one substitutes a fallback sentence for an
// empty reply, which is right when reporting a failed operation and wrong here — a
// batch entry with no message would claim an error it cannot describe.
func errorReplyText(res *mcpgo.CallToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(mcpgo.TextContent); ok && t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return unwrapToolError(strings.Join(parts, "; "))
}
