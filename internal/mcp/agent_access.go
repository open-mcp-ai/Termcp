package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func agentFunction(p auth.AgentPolicy, group string) error {
	if group == "" || !p.Allows(group) {
		return fmt.Errorf("Agent permission denied: %s", group)
	}
	if (group == "extension" || group == "profile_write") && p.HostMode != "all" {
		return fmt.Errorf("this function requires all saved hosts")
	}
	return nil
}

func (s *Server) agentHostAllowed(p auth.AgentPolicy, name string) bool {
	if s.sshConfigs == nil {
		return name == "internal" && p.AllowsHost(sshconfig.InternalIdentity)
	}
	if looksLikeResourceLocator(name) {
		if parsed, err := parseResourceURL(name); err == nil {
			name = parsed.Entry
		}
	}
	id, err := s.sshConfigs.Identity(name)
	return err == nil && p.AllowsHost(id)
}

func (s *Server) agentSessionAllowed(ctx context.Context, p auth.AgentPolicy, sess *session.Session) bool {
	if sess == nil {
		return false
	}
	info := sess.Info()
	return ns.Reachable(info.Namespace, callerNamespace(ctx)) && p.AllowsHost(info.SSHConfigIdentity)
}

func (s *Server) filterAgentSessions(ctx context.Context, in []api.Session) []api.Session {
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		return in
	}
	out := make([]api.Session, 0, len(in))
	if err != nil || !p.Allows("session_read") {
		return out
	}
	for _, v := range in {
		if ns.Reachable(v.Namespace, callerNamespace(ctx)) && p.AllowsHost(v.SSHConfigIdentity) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) agentToolCheck(ctx context.Context, tool string, request mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		return nil
	}
	deny := func(e error) *mcpgo.CallToolResult { return toolError(CodeForbidden, "%s", e.Error()) }
	if err != nil {
		return deny(err)
	}

	args := request.GetArguments()
	requirement := capability.AgentRequirement(tool, args)
	if requirement.ParsedDirectives {
		return nil
	}
	if err = agentFunction(p, requirement.Function); err != nil {
		return deny(err)
	}
	if key := requirement.ProfileArgument; key != "" && !s.agentHostAllowed(p, getString(args, key, "")) {
		return deny(fmt.Errorf("host is outside this Agent's scope"))
	}
	if requirement.StartsCommand && !p.Allows("terminal_write") {
		return deny(fmt.Errorf("starting a command requires terminal_write"))
	}
	if requirement.LocalTransfer && (!p.Allows("file_read") || !p.Allows("file_write")) {
		return deny(fmt.Errorf("local transfers require both file_read and file_write"))
	}
	if requirement.NeedsLocal && !s.agentHostAllowed(p, "internal") {
		return deny(fmt.Errorf("local access requires the internal host"))
	}

	guard := toolGuard{server: s}
	for _, key := range []string{"session_id", "shell_id", "forward_id"} {
		if raw := getString(args, key, ""); raw != "" {
			ids, _ := splitBatchIDs(raw)
			for _, id := range ids {
				var sess *session.Session
				switch key {
				case "forward_id":
					sess = guard.sessionForForwardID(id)
				case "shell_id":
					sess = guard.sessionForShellID(id)
				default:
					sess = guard.sessionForSessionID(id)
				}
				if sess == nil || !s.agentSessionAllowed(ctx, p, sess) {
					return deny(fmt.Errorf("session is outside this Agent's host scope"))
				}
				// Deferred review cannot retain a stale Agent grant. This version refuses
				// queueing on restricted accounts; operators may perform reviewed actions
				// directly. Ordinary resource restrictions and review behavior remain intact.
				if sess != nil && sess.GatesKind(kindFor(tool, args)) {
					return deny(fmt.Errorf("this operation requires operator review; perform it in the WebUI"))
				}
			}
		}
	}
	return nil
}

func (s *Server) agentStatementCheck(ctx context.Context, stmt directive.Statement) *envelope.Slot {
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		return nil
	}
	deny := func(e error) *envelope.Slot {
		return &envelope.Slot{Code: 403, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeForbidden}, Body: e.Error()}
	}
	if err != nil {
		return deny(err)
	}
	if stmt.Op == "help" {
		return nil
	}
	group := ""
	switch {
	case stmt.Kind == "host":
		group = "profile_read"
		if stmt.Op == "create" || stmt.Op == "edit" || stmt.Op == "rm" {
			group = "profile_write"
		}
	case stmt.Kind == "ext":
		group = "extension"
	case stmt.Kind == "alias":
		group = "alias"
	case stmt.Kind == "task":
		group = "session_read"
		if stmt.Op == "close" {
			group = "file_write"
		}
	case isPeerKind(stmt.Kind) || isPeerHandle(stmt.Target) || stmt.Op == "call" || boolFlag(stmt, "frame"):
		group = "peer"
	case stmt.Kind == "forward":
		group = "forward"
	case stmt.Kind == "file":
		group = "file_read"
		if stmt.Op == "write" || stmt.Op == "create" || stmt.Op == "edit" || stmt.Op == "rm" {
			group = "file_write"
		}
	default:
		switch stmt.Op {
		case "connect", "open":
			group = "session_manage"
			if s.sessionForStatement(stmt) != nil {
				group = "terminal_write"
			}
		case "run", "input", "key", "resize":
			group = "terminal_write"
		case "read", "span", "reader":
			group = "terminal_read"
		case "ls", "get":
			group = "session_read"
		case "close", "rm":
			group = "session_manage"
		case "notify":
			group = "notify"
		}
	}
	if err = agentFunction(p, group); err != nil {
		return deny(err)
	}
	if stmt.Pipe != nil {
		if !p.Allows("file_write") {
			return deny(fmt.Errorf("copy requires file_write"))
		}
		if bad := s.agentStatementCheck(ctx, *stmt.Pipe); bad != nil {
			return bad
		}
	}
	if stmt.Kind == "host" && stmt.Key != "" && stmt.Op == "get" && !s.agentHostAllowed(p, stmt.Key) {
		return deny(fmt.Errorf("host is outside this Agent's scope"))
	}
	if stmt.Kind == "task" && stmt.Op != "ls" {
		id := stmt.Key
		if id == "" {
			id = stmt.Target
		}
		if v, e := s.tasks.Get(id); e == nil {
			if !s.agentTaskAllowed(ctx, p, v.SessionID) {
				return deny(fmt.Errorf("task is outside this Agent's host scope"))
			}
		}
		return nil
	}
	if local, _ := stmt.Flag("local-path"); local != "" && !s.agentHostAllowed(p, "internal") {
		return deny(fmt.Errorf("local access requires the internal host"))
	}
	if stmt.Target != "" && stmt.Kind != "host" && stmt.Kind != "alias" && stmt.Kind != "ext" {
		ids, _ := splitBatchIDs(stmt.Target)
		for _, id := range ids {
			single := stmt
			single.Target = id
			sess := s.sessionForStatement(single)
			if sess == nil && isPeerHandle(id) {
				jrpcChannelsMu.Lock()
				h := jrpcChannels[id]
				jrpcChannelsMu.Unlock()
				if h != nil {
					sess = (toolGuard{server: s}).sessionForSessionID(h.sessionID)
				}
			}
			if sess == nil {
				if linked, _ := s.resolveTunnelSession(id); linked != nil {
					sess = linked
				}
			}
			if sess != nil {
				if !s.agentSessionAllowed(ctx, p, sess) {
					return deny(fmt.Errorf("session is outside this Agent's host scope"))
				}
			} else if stmt.Op == "connect" || stmt.Op == "open" {
				if !s.agentHostAllowed(p, id) {
					return deny(fmt.Errorf("host is outside this Agent's scope"))
				}
			} else {
				return deny(fmt.Errorf("target is outside this Agent's accessible resources"))
			}
		}
	}
	if argv, _ := stmt.Flag("argv"); argv != "" && !p.Allows("terminal_write") {
		return deny(fmt.Errorf("starting a command requires terminal_write"))
	}
	return nil
}

func (s *Server) agentTaskAllowed(ctx context.Context, p auth.AgentPolicy, target string) bool {
	sess := (toolGuard{server: s}).sessionForSessionID(target)
	if sess == nil {
		sess, _ = s.resolveTunnelSession(target)
	}
	return s.agentSessionAllowed(ctx, p, sess)
}
