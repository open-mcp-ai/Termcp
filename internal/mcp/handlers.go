package mcp

import (
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/crypto/ssh"

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

// requireSession resolves a session_id argument into the session it names. It is
// the one place a session is looked up for a tool argument, so every tool accepts
// the same spellings and acts on the same canonical object.
//
// Accepted: a raw session id, a termcp:// session locator, a termcp:// shell
// locator (its session part), or a raw shell id (both the root and child shells
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
			return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' not found", p.SessionID))
		}
	}
	return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' not found", sessionID))
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
		return nil, toolError(CodeSessionNotRunning, "%s", fmt.Sprintf("Session '%s' is closed (status %s); this operation needs a running session — read its output via shell_output", sessionID, info.Status))
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

// sftpClient resolves a session and creates an SFTP client over it.
// Caller must defer Close() on the returned client.
func (s *Server) sftpClient(sessionID string) (*sftp.Client, *mcpgo.CallToolResult) {
	_, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return nil, bad
	}
	cli, err := sftp.NewClient(sshCli)
	if err != nil {
		return nil, toolError(CodeOperationFailed, "%s", fmt.Sprintf("SFTP: %v", err))
	}
	return cli, nil
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
				return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' is closed (status %s); its output is read-only via shell_output", sess.ID, status))
			}
		}
		return cs, nil
	}
	if sess := s.sessMgr.Get(shellID); sess != nil {
		if sess.Info().Status != api.SessionRunning {
			return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' is closed (status %s); its output is read-only via shell_output", shellID, sess.Info().Status))
		}
		if cs := sess.PrimaryShell(); cs != nil {
			return cs, nil
		}
		return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Session '%s' has no shell", shellID))
	}
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
	return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shellID))
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
