package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/task"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func agentDeniedSlot(stmt directive.Statement, err error) *envelope.Slot {
	return &envelope.Slot{Code: 403, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeForbidden}, Body: err.Error()}
}

func checkAgentPlan(p auth.AgentPolicy, plan authorization.Plan) error {
	decision, err := evaluateAgentPlan(p, plan)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fmt.Errorf("Agent permission denied: %s", decision.Reason)
	}
	return nil
}

func evaluateAgentPlan(p auth.AgentPolicy, plan authorization.Plan) (authorization.Decision, error) {
	for _, req := range plan.Requirements {
		op, known := authorization.Lookup(req.Operation)
		if !known {
			return authorization.Decision{}, fmt.Errorf("Agent permission denied: unknown operation %q", req.Operation)
		}
		if op.Global && p.HostMode != "all" {
			return authorization.Decision{}, fmt.Errorf("operation %s requires all hosts", op.ID)
		}
	}
	return authorization.CheckPlan(p.AuthorizationPolicy(), plan), nil
}

func operationPlan(id string, evidence authorization.Evidence) authorization.Plan {
	return authorization.Plan{Requirements: []authorization.Requirement{{Operation: id, Evidence: evidence}}}
}

type statementApprovalPlanningKey struct{}

func withStatementApprovalPlanning(ctx context.Context) context.Context {
	return context.WithValue(ctx, statementApprovalPlanningKey{}, true)
}

func statementApprovalPlanning(ctx context.Context) bool {
	value, _ := ctx.Value(statementApprovalPlanningKey{}).(bool)
	return value
}

func (s *Server) agentToolCheckV2(ctx context.Context, p auth.AgentPolicy, tool string, request mcpgo.CallToolRequest) (context.Context, *mcpgo.CallToolResult, bool) {
	if tool == "term" {
		return ctx, nil, false
	} // parsed statements are checked before dispatch
	deny := func(err error) (context.Context, *mcpgo.CallToolResult, bool) {
		recordAgentDenied(ctx, "mcp.tool.invoke", tool, err.Error())
		return ctx, toolError(CodeForbidden, "%s", err.Error()), false
	}
	args := request.GetArguments()
	plan, err := capability.AgentToolPlan(tool, args)
	if err != nil {
		return deny(err)
	}
	bindings, _ := agentBindingsFromContext(ctx)
	if bindings.profiles == nil {
		bindings.profiles = map[string]sshconfig.AuthorizationBinding{}
	}
	if bindings.extensions == nil {
		bindings.extensions = map[string]extensions.AuthorizationBinding{}
	}
	bindHost := func(name string) error {
		binding, exists := bindings.profiles[strings.TrimSpace(name)]
		if !exists {
			binding, err = s.profileAuthorizationBinding(name)
			if err != nil {
				return fmt.Errorf("cannot resolve the authorized host: %w", err)
			}
		}
		if !p.AllowsHost(binding.Identity) {
			return fmt.Errorf("host is outside this Agent's scope")
		}
		bindings.profiles[name] = binding
		bindings.profiles[strings.TrimSpace(name)] = binding
		bindings.profiles[binding.Name] = binding
		return nil
	}
	targets := capability.AgentToolTargets(tool, args)
	if key := targets.ProfileArgument; key != "" {
		if err := bindHost(getString(args, key, "")); err != nil {
			return deny(err)
		}
	}
	if key := targets.ExtensionArgument; key != "" {
		name := getString(args, key, "")
		target := getString(args, "target", "")
		binding, err := s.agentExtensionBinding(p, name, target)
		if err != nil {
			return deny(fmt.Errorf("extension is outside this Agent's host scope: %w", err))
		}
		bindings.extensions[extensionBindingKey(name, target)] = binding
	}
	if targets.NeedsLocal && !s.agentHostAllowed(p, "internal") {
		return deny(fmt.Errorf("local access requires the internal host"))
	}

	g := toolGuard{server: s}
	for _, key := range []string{"session_id", "shell_id", "forward_id"} {
		if raw := getString(args, key, ""); raw != "" {
			ids, _ := splitBatchIDs(raw)
			for _, id := range ids {
				var sess *session.Session
				switch key {
				case "shell_id":
					sess = g.lookupShellID(id)
				case "forward_id":
					sess = g.sessionForForwardID(id)
				default:
					sess = g.lookupSessionID(id)
				}
				// Borrowed host locators do not name an existing session. Bind
				// their saved host identity before applying the operation policy.
				if sess == nil && key != "forward_id" {
					if host, borrowed := s.parseBorrowedHost(id); borrowed {
						if err := bindHost(host); err != nil {
							return deny(err)
						}
						continue
					}
				}
				if !s.agentSessionAllowed(ctx, p, sess) {
					return deny(fmt.Errorf("session is outside this Agent's host scope"))
				}
			}
		}
	}
	decision, err := evaluateAgentPlan(p, plan)
	if err != nil {
		return deny(err)
	}
	if !decision.Allowed {
		return deny(fmt.Errorf("Agent permission denied: %s", decision.Reason))
	}
	ctx = withAgentResourceBindings(ctx, bindings)
	if decision.NeedsApproval && !approvedContext(ctx) && !previewMode(ctx) && !statementApprovalPlanning(ctx) {
		principalID, _ := auth.ActorFromContext(ctx)
		credentialID := strings.TrimPrefix(auth.CredentialIDFromContext(ctx), "token:")
		payload, err := json.Marshal(gatedPayload{
			Mode: "tool", Tool: tool, Args: args,
			PrincipalID: principalID.ID, CredentialID: credentialID,
		})
		if err != nil {
			return ctx, toolError(CodeOperationFailed, "cannot describe operation for approval: %s", err.Error()), false
		}
		operation := approvalOperation(p, plan)
		summary := summarizeOperation(tool, args) + " [" + operation + "]"
		sessionID := targetSessionID(request)
		approvalID, err := s.submitPolicyApproval(ctx, sessionID, payload, approval.Kind(operation), summary)
		if err != nil {
			return ctx, toolError(CodeOperationFailed, "%s", err.Error()), false
		}
		auth.RecordAudit(ctx, auth.AuditEvent{
			Action: "mcp.approval.request", TargetType: "mcp_tool", TargetID: tool,
			Result: "pending", Reason: operation,
		})
		return ctx, reviewPendingOperationResult(summary, s.policyApprovalCorrelation(sessionID, approvalID)), true
	}
	return ctx, nil, false
}

func approvalOperation(p auth.AgentPolicy, plan authorization.Plan) string {
	for _, req := range plan.Requirements {
		if grant, ok := p.Actions[req.Operation]; ok && grant.Effect == authorization.EffectAsk {
			return req.Operation
		}
	}
	return plan.Requirements[0].Operation
}

func (s *Server) agentExtensionAllowed(p auth.AgentPolicy, name, target string) bool {
	_, err := s.agentExtensionBinding(p, name, target)
	return err == nil
}

func (s *Server) agentExtensionBinding(p auth.AgentPolicy, name, target string) (extensions.AuthorizationBinding, error) {
	if s.extensions == nil {
		return extensions.AuthorizationBinding{}, errors.New("extensions are not configured")
	}
	binding, err := s.extensions.Binding(name, target)
	if err != nil {
		return extensions.AuthorizationBinding{}, err
	}
	if !p.AllowsHost(binding.TargetProfile.Identity) {
		return extensions.AuthorizationBinding{}, errors.New("target host is outside this Agent's scope")
	}
	return binding, nil
}

// agentStatementSession is a read-only counterpart of dispatch's target lookup.
func (s *Server) agentStatementSession(target string) *session.Session {
	g := toolGuard{server: s}
	if sess := g.lookupShellID(target); sess != nil {
		return sess
	}
	jrpcChannelsMu.Lock()
	h := jrpcChannels[target]
	jrpcChannelsMu.Unlock()
	if h != nil {
		return g.lookupSessionID(h.sessionID)
	}
	if s.sessMgr == nil {
		return nil
	}
	var found *session.Session
	for _, info := range s.sessMgr.ListAll() {
		if info.SSHConfig == target && info.Status == api.SessionRunning {
			if found != nil {
				return nil
			}
			found = s.sessMgr.Get(info.ID)
		}
	}
	return found
}

func agentApprovalSlot(stmt directive.Statement, summary string, corr reviewPendingFields) *envelope.Slot {
	result := reviewPendingOperationResult(summary, corr)
	return &envelope.Slot{
		Code:    202,
		Headers: map[string]string{"Path": stmt.Path},
		Body:    toolResultText(result),
	}
}

func (s *Server) statementPlanStatus(ctx context.Context, p auth.AgentPolicy, plan authorization.Plan, stmt directive.Statement, win termArgs) (*envelope.Slot, error) {
	decision, err := evaluateAgentPlan(p, plan)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, fmt.Errorf("Agent permission denied: %s", decision.Reason)
	}
	if !decision.NeedsApproval || approvedContext(ctx) || previewMode(ctx) {
		return nil, nil
	}
	principal, _ := auth.ActorFromContext(ctx)
	credentialID := strings.TrimPrefix(auth.CredentialIDFromContext(ctx), "token:")
	statement := stmt
	window := win
	payload, err := json.Marshal(gatedPayload{
		Mode: "dsl", Statement: &statement, Window: &window,
		PrincipalID: principal.ID, CredentialID: credentialID,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot describe DSL operation for approval: %w", err)
	}
	operation := approvalOperation(p, plan)
	summary := stmt.Path + " [" + operation + "]"
	sessionID := ""
	if sess := s.agentStatementSession(stmt.Target); sess != nil {
		sessionID = sess.ID
	}
	approvalID, err := s.submitPolicyApproval(ctx, sessionID, payload, approval.Kind(operation), summary)
	if err != nil {
		return nil, fmt.Errorf("cannot queue DSL operation for approval: %w", err)
	}
	auth.RecordAudit(ctx, auth.AuditEvent{
		Action: "mcp.approval.request", TargetType: "dsl",
		TargetID: strings.TrimSpace(stmt.Kind + " " + stmt.Op), Result: "pending", Reason: operation,
	})
	return agentApprovalSlot(stmt, summary, s.policyApprovalCorrelation(sessionID, approvalID)), nil
}

func (s *Server) agentStatementCheckV2(ctx context.Context, p auth.AgentPolicy, stmt directive.Statement, win termArgs) *envelope.Slot {
	if stmt.Op == "help" {
		return nil
	}
	planningCtx := withStatementApprovalPlanning(ctx)
	deny := func(err error) *envelope.Slot {
		recordAgentDenied(ctx, "mcp.dsl.authorize", strings.TrimSpace(stmt.Kind+" "+stmt.Op), err.Error())
		return agentDeniedSlot(stmt, err)
	}
	// These branches dispatch through checkAdmission after their effective arguments
	// have been built. Preclassifying them by verb would reintroduce the old drift.
	delegated := stmt.Kind == "host" || stmt.Kind == "ext" || stmt.Kind == "file" || stmt.Kind == "forward"
	if stmt.Kind == "task" {
		delegated = false
	}
	if stmt.Pipe != nil {
		source := "shell.output"
		evidence := authorization.Evidence{Forms: []string{"pipe"}}
		if stmt.Kind == "file" {
			source = "file.read"
			// copySource only parses addressing. It does not open a file or start
			// a task. Execution applies the same range, possibly narrowed further
			// by the call's bytes_limit, so this is a conservative byte bound.
			src, bad := s.copySource(stmt, termArgs{})
			if bad != nil {
				return bad
			}
			evidence.Bytes, evidence.HasBytes = src.length, src.length > 0
		}
		plan := operationPlan(source, evidence)
		plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "file.write", Evidence: evidence})
		for _, end := range []directive.Statement{stmt, *stmt.Pipe} {
			sess := s.agentStatementSession(end.Target)
			if !s.agentSessionAllowed(ctx, p, sess) {
				return deny(fmt.Errorf("copy endpoint is outside this Agent's host scope"))
			}
			// This path streams directly rather than calling the file tools. Apply the
			// same resource/path/review checks to both ends before starting a task.
			tool := "file_write"
			args := argsOf("session_id", end.Target, "remote_path", end.Key)
			if end.Op == "read" {
				tool = "file_read"
				if end.Kind != "file" {
					tool = "shell_output"
					args = argsOf("shell_id", end.Target)
				}
			}
			if bad := s.admission(ctx, tool, lookupGate(tool), toolRequest(tool, args)); bad != nil {
				return deny(fmt.Errorf("%s", toolResultText(bad)))
			}
		}
		pending, err := s.statementPlanStatus(ctx, p, plan, stmt, win)
		if err != nil {
			return deny(err)
		}
		return pending
	}
	if delegated {
		return nil
	}
	id := ""
	e := authorization.Evidence{}
	var selectedPlan authorization.Plan
	if isPeerKind(stmt.Kind) || isPeerHandle(stmt.Target) || stmt.Op == "call" || boolFlag(stmt, "frame") {
		plan, err := s.agentPeerPlan(stmt)
		if err != nil {
			return deny(err)
		}
		selectedPlan = plan
	} else {
		switch stmt.Kind {
		case "alias":
			return deny(fmt.Errorf("alias management is not an Agent capability"))
		case "task":
			// Tasks inherit the host scope of both endpoints. They have no
			// separate row in the dev capability table.
			if stmt.Op != "ls" {
				key := stmt.Key
				if key == "" {
					key = stmt.Target
				}
				v, err := s.tasks.Get(key)
				if err == nil && !s.agentTaskViewAllowed(ctx, p, v) {
					return deny(fmt.Errorf("task is outside this Agent's host scope"))
				}
			}
			return nil
		default:
			switch stmt.Op {
			case "connect", "open", "input", "key", "resize", "notify", "reader", "close", "rm":
				return nil // tool wrapper checks the actual branch
			case "run":
				plan := operationPlan("shell.input", e)
				plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.key", Evidence: authorization.Evidence{Keys: []string{"enter"}, Repeat: 1}}, authorization.Requirement{Operation: "shell.output"})
				for _, tool := range []string{"shell_input", "shell_key", "shell_output"} {
					args := argsOf("shell_id", stmt.Target, "key", "enter")
					if _, bad, _ := s.agentToolCheckV2(planningCtx, p, tool, toolRequest(tool, args)); bad != nil {
						return deny(fmt.Errorf("%s", toolResultText(bad)))
					}
					if bad := s.admission(ctx, tool, lookupGate(tool), toolRequest(tool, args)); bad != nil {
						return deny(fmt.Errorf("%s", toolResultText(bad)))
					}
				}
				pending, err := s.statementPlanStatus(ctx, p, plan, stmt, win)
				if err != nil {
					return deny(err)
				}
				return pending
			case "read":
				id = "shell.output"
			case "span":
				id = "session.message"
			case "get":
				id = "session.info"
			case "ls":
				id = "session.list"
				if stmt.Target != "" {
					id = "shell.list"
				}
			}
		}
		selectedPlan = operationPlan(id, e)
	}
	if stmt.Target != "" && stmt.Kind != "alias" {
		ids, _ := splitBatchIDs(stmt.Target)
		for _, target := range ids {
			sess := s.agentStatementSession(target)
			if !s.agentSessionAllowed(ctx, p, sess) {
				return deny(fmt.Errorf("target is outside this Agent's host scope"))
			}
			if isPeerKind(stmt.Kind) || isPeerHandle(target) {
				kind := stmt.Kind
				if !isPeerKind(kind) {
					kind = "mcp"
				}
				if !sess.Restriction().Allows(kind + ".open") {
					return deny(fmt.Errorf("this session's template does not grant %s.open", kind))
				}
			}
		}
	}
	pending, err := s.statementPlanStatus(ctx, p, selectedPlan, stmt, win)
	if err != nil {
		return deny(err)
	}
	return pending
}

// checkAgentPeerEffect is also called at the protocol send site, after parsing.
// That makes the values actually sent authoritative, including shorthand/raw
// dispatch differences. It runs before handshakes and before writing stdio bytes.
func (s *Server) checkAgentPeerEffect(ctx context.Context, stmt directive.Statement, plan authorization.Plan) *envelope.Slot {
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		return nil
	}
	if err != nil {
		return agentDeniedSlot(stmt, err)
	}
	decision, err := evaluateAgentPlan(p, plan)
	if err != nil {
		return agentDeniedSlot(stmt, err)
	}
	if !decision.Allowed {
		return agentDeniedSlot(stmt, fmt.Errorf("Agent permission denied: %s", decision.Reason))
	}
	if decision.NeedsApproval && !approvedContext(ctx) && !previewMode(ctx) {
		return agentDeniedSlot(stmt, errors.New("this protocol operation must be submitted for approval at the command entrance"))
	}
	sess := s.agentStatementSession(stmt.Target)
	if !s.agentSessionAllowed(ctx, p, sess) {
		return agentDeniedSlot(stmt, fmt.Errorf("target is outside this Agent's host scope"))
	}
	for _, req := range plan.Requirements {
		op, _ := authorization.Lookup(req.Operation)
		// Stdio peers inherit the shell's existing resource capabilities. The
		// network-door capability was never required to type on an existing pipe.
		if !isPeerKind(stmt.Kind) && !isPeerHandle(stmt.Target) && (strings.HasPrefix(op.Resource, "mcp.") || strings.HasPrefix(op.Resource, "cdp.") || strings.HasPrefix(op.Resource, "jrpc.")) {
			continue
		}
		if op.Resource != "" && !sess.Restriction().Allows(op.Resource) {
			return agentDeniedSlot(stmt, fmt.Errorf("this session's template does not grant %s", op.Resource))
		}
	}
	return nil
}

// agentPeerPlan covers both shorthand tool names and raw JSON-RPC, including
// every member of a stdio batch. Recognized methods never borrow raw permission.
func (s *Server) agentPeerPlan(stmt directive.Statement) (authorization.Plan, error) {
	kind := stmt.Kind
	if !isPeerKind(kind) {
		kind = "mcp"
		jrpcChannelsMu.Lock()
		h := jrpcChannels[stmt.Target]
		jrpcChannelsMu.Unlock()
		if h != nil {
			kind = h.kind
		}
	}
	e := authorization.Evidence{}
	id := kind + ".open"
	switch stmt.Op {
	case "open", "close", "ls", "get", "read":
		// Reaching and using a peer is one capability in the dev model.
	case "call":
		payload := unquoteWord(strings.TrimSpace(stmt.Payload))
		if kind == "mcp" && !strings.HasPrefix(payload, "{") && !strings.HasPrefix(payload, "[") {
			name, _, bad := splitToolCall(stmt, stmt.Payload)
			if bad != nil {
				return authorization.Plan{}, fmt.Errorf("%s", bad.Body)
			}
			return operationPlan(id, authorization.Evidence{ToolName: name}), nil
		}
		var value any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return authorization.Plan{}, fmt.Errorf("invalid protocol frame")
		}
		return frameOperationPlan(kind, value)
	default:
		return authorization.Plan{}, fmt.Errorf("unclassified peer action %q", stmt.Op)
	}
	return operationPlan(id, e), nil
}

func frameOperationPlan(kind string, value any) (authorization.Plan, error) {
	plan := authorization.Plan{}
	if batch, ok := value.([]any); ok {
		if len(batch) == 0 {
			return plan, fmt.Errorf("empty frame batch")
		}
		for _, item := range batch {
			p, err := frameOperationPlan(kind, item)
			if err != nil {
				return plan, err
			}
			plan.Requirements = append(plan.Requirements, p.Requirements...)
		}
		return plan, nil
	}
	frame, ok := value.(map[string]any)
	if !ok {
		return plan, fmt.Errorf("frame must be an object")
	}
	method, _ := frame["method"].(string)
	switch kind {
	case "mcp":
		switch method {
		case "tools/call":
			params, _ := frame["params"].(map[string]any)
			name, _ := params["name"].(string)
			if name == "" {
				return plan, fmt.Errorf("tools/call requires a tool name")
			}
			return operationPlan("mcp.open", authorization.Evidence{ToolName: name}), nil
		default:
			return operationPlan("mcp.open", authorization.Evidence{}), nil
		}
	case "cdp":
		return operationPlan("cdp.open", authorization.Evidence{Method: method}), nil
	case "jrpc":
		return operationPlan("jrpc.open", authorization.Evidence{Method: method}), nil
	}
	return plan, fmt.Errorf("unknown protocol kind")
}

func toolRequest(tool string, args map[string]any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Name = tool
	req.Params.Arguments = args
	return req
}

func (s *Server) taskResource(target, role string) task.ResourceRef {
	ref := task.ResourceRef{Role: role}
	if sess := s.agentStatementSession(target); sess != nil {
		info := sess.Info()
		ref.HostID, ref.Namespace = info.SSHConfigIdentity, info.Namespace
	}
	return ref
}

func (s *Server) localTaskResource(ctx context.Context, role string) task.ResourceRef {
	return task.ResourceRef{HostID: sshconfig.InternalIdentity, Namespace: callerNamespace(ctx), Role: role}
}

func (s *Server) agentTaskViewAllowed(ctx context.Context, p auth.AgentPolicy, v task.View) bool {
	if len(v.Resources) != 2 {
		return false
	}
	read, write := false, false
	for _, ref := range v.Resources {
		if !p.AllowsHost(ref.HostID) || !ns.Reachable(ref.Namespace, callerNamespace(ctx)) {
			return false
		}
		switch ref.Role {
		case "read":
			read = true
		case "write":
			write = true
		default:
			return false
		}
	}
	return read && write
}

func (s *Server) checkAgentCDPFrame(ctx context.Context, stmt directive.Statement, frame map[string]any, transient bool) *envelope.Slot {
	plan, err := frameOperationPlan("cdp", frame)
	if err != nil {
		return agentDeniedSlot(stmt, err)
	}
	_ = transient // CDP attach/detach are branches of cdp.open.
	return s.checkAgentPeerEffect(ctx, stmt, plan)
}
