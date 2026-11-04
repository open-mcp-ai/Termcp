package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func recordAgentDenied(ctx context.Context, action, target, reason string) {
	if len(reason) > 300 {
		reason = reason[:300]
	}
	auth.RecordAudit(ctx, auth.AuditEvent{
		Action: action, TargetType: "mcp_tool", TargetID: target, Result: "deny", Reason: reason,
	})
}

func (s *Server) agentHostAllowed(p auth.AgentPolicy, name string) bool {
	if s.sshConfigs == nil {
		return name == "internal" && p.AllowsHost(sshconfig.InternalIdentity)
	}
	canonical, err := profileReferenceName(name)
	if err != nil {
		return false
	}
	id, err := s.sshConfigs.Identity(canonical)
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
	allowed := p.MayRequestOperation("session.list") || p.MayRequestOperation("shell.list")
	if err != nil || !allowed {
		return out
	}
	for _, v := range in {
		if ns.Reachable(v.Namespace, callerNamespace(ctx)) && p.AllowsHost(v.SSHConfigIdentity) {
			if !p.MayRequestOperation("shell.list") {
				v.Shells = nil
			}
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) agentToolCheck(ctx context.Context, tool string, request mcpgo.CallToolRequest) (context.Context, *mcpgo.CallToolResult, bool) {
	if extensionMCPToolAuthorized(ctx, tool, request) {
		return ctx, nil, false
	}
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
			recordAgentDenied(ctx, "mcp.authorize", tool, "Agent policy is unavailable")
			return ctx, toolError(CodeForbidden, "%s", "Agent policy is unavailable"), false
		}
		return ctx, nil, false
	}
	if err != nil {
		recordAgentDenied(ctx, "mcp.authorize", tool, err.Error())
		return ctx, toolError(CodeForbidden, "%s", err.Error()), false
	}
	return s.agentToolCheckV2(ctx, p, tool, request)
}

func (s *Server) agentStatementCheck(ctx context.Context, stmt directive.Statement, win termArgs) *envelope.Slot {
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
			recordAgentDenied(ctx, "mcp.authorize", "dsl", "Agent policy is unavailable")
			return agentDeniedSlot(stmt, fmt.Errorf("Agent policy is unavailable"))
		}
		return nil
	}
	if err != nil {
		recordAgentDenied(ctx, "mcp.authorize", "dsl", err.Error())
		return &envelope.Slot{Code: 403, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeForbidden}, Body: err.Error()}
	}
	return s.agentStatementCheckV2(ctx, p, stmt, win)
}
