package mcp

import (
	"context"
	"slices"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// gate is the admission decision for one call, taken from the capability table.
type gate struct {
	capability.Capability
	known bool
}

// lookupGate resolves the capability a tool call names.
func lookupGate(tool string) gate {
	c, ok := capability.ForTool(tool)
	return gate{Capability: c, known: ok}
}

// checkAdmission is the one place an MCP call is admitted or refused.
//
// It asks the three questions every surface used to re-derive for itself:
//
//  1. may this caller reach the session at all (namespace isolation)?
//  2. may this session run this operation (the template's capability list, and
//     the paths it granted)?
//  3. does it need review before it runs (the capability's gate)?
//
// The third question used to be answered inside the handlers — `if
// shell.RequiresApproval()` in internal/mcp's shell handlers, a gatedTools table
// in approval_gate.go — which is the bug this file exists to remove: a new
// reviewable operation was covered only if someone remembered to guard its
// handler. Here it is covered by its capability row.
//
// The result is (refusal, held): a refusal is the reply to send, and held means
// the operation was queued for a human and the reply reports that.
func (s *Server) checkAdmission(ctx context.Context, toolName string, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, bool) {
	g := lookupGate(toolName)
	if bad := s.admission(ctx, toolName, g, request); bad != nil {
		return bad, false
	}
	// A scoped token's whitelists (scoped-tokens.md PR 4): tools by capability
	// id, sessions by resolved session id or profile name. The static
	// credentials carry no scopes and never reach these checks.
	sessionID := targetSessionID(request)
	if bad := scopeAdmission(ctx, g, sessionID, s, request); bad != nil {
		return bad, false
	}
	// Review holds the operation for a human. Whether THIS call is reviewable is
	// the capability's own answer plus the call's action (see kindFor): a
	// `forward --action list` and a `forward --action local` are one capability
	// and only one of them is a decision. Reads are absent by construction, which
	// is deliberate — putting a listing behind a decision trains a reviewer to
	// click Accept without reading, which is how a gate stops working.
	if kindFor(toolName, request.GetArguments()) == "" {
		return nil, false
	}
	// A staged operation is reviewed by its own write path, not by queueing this
	// call: terminal input is one command line split across two calls, and the
	// line is the unit a human decides. See capability.Capability.Staged.
	if g.Staged {
		return nil, false
	}
	return s.gateOperation(ctx, request, toolName, sessionID)
}

// scopeAdmission enforces a scoped token's restrictions. It answers a refusal
// when the token's whitelists exclude this call, and force-enables the
// session's review mode when the token demands approval.
func scopeAdmission(ctx context.Context, g gate, sessionID string, s *Server, request mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p == nil || p.ScopeFree() && !p.ApprovalRequired {
		return nil
	}
	if len(p.Tools) > 0 {
		if !g.known || !slices.Contains(p.Tools, g.Capability.ID) {
			return toolError(CodeForbidden, "this token's scope does not include %s", g.Capability.ID)
		}
	}
	if len(p.Sessions) > 0 && sessionID != "" {
		sess, bad := s.requireSession(sessionID)
		if bad == nil && sess != nil {
			if !slices.Contains(p.Sessions, sess.ID) && !slices.Contains(p.Sessions, sess.SSHConfig) {
				return toolError(CodeForbidden, "this token's scope does not include session %s", sess.ID)
			}
		}
	}
	if p.ApprovalRequired && sessionID != "" {
		// The token says every write waits for a human, whatever the session's
		// own mode says: force review on (idempotent), after which the staged
		// write path and gateOperation both engage on their own.
		if sess, bad := s.requireSession(sessionID); bad == nil && sess != nil && !sess.ApprovalEnabled() {
			if err := sess.EnableApprovalScoped(1, 0, nil); err != nil {
				return toolError(CodeOperationFailed, "cannot engage review mode required by this token: %s", err.Error())
			}
		}
	}
	return nil
}

// admission answers the reachability and whitelist questions and returns a
// refusal when either says no.
func (s *Server) admission(ctx context.Context, toolName string, g gate, request mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	guard := toolGuard{server: s}
	return guard.check(ctx, toolName, g, request)
}

// targetSessionID is the session a call names, from whichever argument carries
// it. It is the SAME spelling independence the guard implements: a raw session
// id, a shell id, or a locator, in either argument.
func targetSessionID(request mcpgo.CallToolRequest) string {
	args := request.GetArguments()
	if args == nil {
		return ""
	}
	if id := getString(args, "shell_id", ""); id != "" {
		if ids, isBatch := splitBatchIDs(id); isBatch {
			return ids[0]
		}
		return id
	}
	return getString(args, "session_id", "")
}

// refuseSession is the whitelist refusal for a capability.
//
// It is kept here rather than in internal/session because it needs the
// capability's id: the sentence a caller reads names the operation it was denied
// in the caller's own vocabulary (the tool name), while the decision was made on
// the capability.
func refuseSession(sess *session.Session, capID, toolName string) *mcpgo.CallToolResult {
	if sess == nil {
		return nil
	}
	if sess.Restriction().Allows(capID) {
		return nil
	}
	return toolError(CodeForbidden, "%s", session.ForbiddenTool(toolName))
}

// nsRefusal is the namespace refusal, naming what the caller is rather than what
// the resource is, so an id cannot be used to enumerate other namespaces.
func nsRefusal(callerNS string) *mcpgo.CallToolResult {
	return toolError(CodeForbidden, "%s", ns.ForbiddenForeign(callerNS))
}
