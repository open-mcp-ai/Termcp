package mcp

import (
	"context"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"

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
	// A SHORT-CIRCUIT TARGET HAS NO SESSION, SO REVIEW HAS NOWHERE TO PUT IT.
	//
	// This sits at the ONE admission point rather than in each handler, because the
	// hole it closes is not about one operation: `write use:prod file /x` reached the
	// SFTP handler ungated while `connect prod` + `write` was held, so a profile's
	// default_approval depended on which spelling the caller picked. Every tool that
	// names a target passes through here, including the ones `term` calls for itself.
	if bad := s.refuseBorrowedReview(targetSessionID(request), toolName); bad != nil {
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
	sessionID := targetSessionID(request)
	return s.gateOperation(ctx, request, toolName, sessionID)
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
