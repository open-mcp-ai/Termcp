package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
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
	for _, req := range plan.Requirements {
		op, known := authorization.Lookup(req.Operation)
		if !known {
			return fmt.Errorf("Agent permission denied: unknown operation %q", req.Operation)
		}
		if op.Global && p.HostMode != "all" {
			return fmt.Errorf("operation %s requires all hosts", op.ID)
		}
	}
	if d := authorization.CheckPlan(p.AuthorizationPolicy(), plan); !d.Allowed {
		return fmt.Errorf("Agent permission denied: %s", d.Reason)
	}
	return nil
}

func operationPlan(id string, evidence authorization.Evidence) authorization.Plan {
	return authorization.Plan{Requirements: []authorization.Requirement{{Operation: id, Evidence: evidence}}}
}

func (s *Server) agentToolCheckV2(ctx context.Context, p auth.AgentPolicy, tool string, request mcpgo.CallToolRequest) (context.Context, *mcpgo.CallToolResult) {
	if tool == "term" {
		return ctx, nil
	} // parsed statements are checked before dispatch
	deny := func(err error) (context.Context, *mcpgo.CallToolResult) {
		recordAgentDenied(ctx, "mcp.tool.invoke", tool, err.Error())
		return ctx, toolError(CodeForbidden, "%s", err.Error())
	}
	args := request.GetArguments()
	plan, err := capability.AgentToolPlan(tool, args)
	if err != nil {
		return deny(err)
	}
	if err = checkAgentPlan(p, plan); err != nil {
		return deny(err)
	}
	bindings := agentResourceBindings{profiles: map[string]sshconfig.AuthorizationBinding{}, extensions: map[string]extensions.AuthorizationBinding{}}
	targets := capability.AgentToolTargets(tool, args)
	if key := targets.ProfileArgument; key != "" {
		name := getString(args, key, "")
		binding, err := s.profileAuthorizationBinding(name)
		if err != nil {
			return deny(fmt.Errorf("cannot resolve the authorized host: %w", err))
		}
		if !p.AllowsHost(binding.Identity) {
			return deny(fmt.Errorf("host is outside this Agent's scope"))
		}
		bindings.profiles[name] = binding
		bindings.profiles[strings.TrimSpace(name)] = binding
		bindings.profiles[binding.Name] = binding
	}
	if key := targets.ExtensionArgument; key != "" {
		name := getString(args, key, "")
		binding, err := s.agentExtensionBinding(p, name)
		if err != nil {
			return deny(fmt.Errorf("extension is outside this Agent's host scope: %w", err))
		}
		bindings.extensions[strings.TrimSpace(name)] = binding
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
				if !s.agentSessionAllowed(ctx, p, sess) {
					return deny(fmt.Errorf("session is outside this Agent's host scope"))
				}
				if sess.GatesKind(kindFor(tool, args)) {
					return deny(fmt.Errorf("this operation requires operator review; perform it in the WebUI"))
				}
			}
		}
	}
	return withAgentResourceBindings(ctx, bindings), nil
}

func (s *Server) agentExtensionAllowed(p auth.AgentPolicy, name string) bool {
	_, err := s.agentExtensionBinding(p, name)
	return err == nil
}

func (s *Server) agentExtensionBinding(p auth.AgentPolicy, name string) (extensions.AuthorizationBinding, error) {
	if s.extensions == nil {
		return extensions.AuthorizationBinding{}, errors.New("extensions are not configured")
	}
	binding, err := s.extensions.Binding(name)
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

func (s *Server) agentStatementCheckV2(ctx context.Context, p auth.AgentPolicy, stmt directive.Statement) *envelope.Slot {
	if stmt.Op == "help" {
		return nil
	}
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
		if err := checkAgentPlan(p, plan); err != nil {
			return deny(err)
		}
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
			if sess.GatesKind(kindFor(tool, args)) {
				return deny(fmt.Errorf("copy requires operator review; perform it in the WebUI"))
			}
		}
		return nil
	}
	if delegated {
		return nil
	}
	id := ""
	e := authorization.Evidence{}
	if isPeerKind(stmt.Kind) || isPeerHandle(stmt.Target) || stmt.Op == "call" || boolFlag(stmt, "frame") {
		plan, err := s.agentPeerPlan(stmt)
		if err != nil {
			return deny(err)
		}
		if err = checkAgentPlan(p, plan); err != nil {
			return deny(err)
		}
	} else {
		switch stmt.Kind {
		case "alias":
			switch stmt.Op {
			case "ls":
				id = "alias.list"
			case "create", "edit":
				id = "alias.write"
			case "rm":
				id = "alias.delete"
			}
		case "task":
			switch stmt.Op {
			case "ls":
				id = "task.list"
			case "get":
				id = "task.info"
			case "close":
				id = "task.cancel"
			}
		default:
			switch stmt.Op {
			case "connect", "open", "input", "key", "resize", "notify", "reader", "close", "rm":
				return nil // tool wrapper checks the actual branch
			case "run":
				plan := operationPlan("shell.input", e)
				plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.key", Evidence: authorization.Evidence{Keys: []string{"enter"}, Repeat: 1}}, authorization.Requirement{Operation: "shell.output"})
				if err := checkAgentPlan(p, plan); err != nil {
					return deny(err)
				}
				for _, tool := range []string{"shell_input", "shell_key", "shell_output"} {
					args := argsOf("shell_id", stmt.Target, "key", "enter")
					if _, bad := s.agentToolCheckV2(ctx, p, tool, toolRequest(tool, args)); bad != nil {
						return deny(fmt.Errorf("%s", toolResultText(bad)))
					}
					if bad := s.admission(ctx, tool, lookupGate(tool), toolRequest(tool, args)); bad != nil {
						return deny(fmt.Errorf("%s", toolResultText(bad)))
					}
				}
				return nil
			case "read":
				id = "shell.output"
			case "span":
				id = "shell.marks"
			case "get":
				id = "session.info"
			case "ls":
				id = "session.list"
				if stmt.Target != "" {
					id = "shell.list"
				}
			}
		}
		if err := checkAgentPlan(p, operationPlan(id, e)); err != nil {
			return deny(err)
		}
	}
	if stmt.Kind == "task" {
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
	return nil
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
	if err = checkAgentPlan(p, plan); err != nil {
		return agentDeniedSlot(stmt, err)
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
	id := ""
	switch stmt.Op {
	case "open":
		id = kind + ".open"
		if _, ok := stmt.Flag("target"); ok {
			id = "cdp.attach"
		}
	case "close":
		id = kind + ".close"
		if kind == "cdp" && strings.Contains(stmt.Key, "@") {
			id = "cdp.detach"
		}
	case "ls":
		id = kind + ".list"
	case "get":
		id = kind + ".info"
		if stmt.Key == "protocol" || len(stmt.Args) > 0 && stmt.Args[0] == "protocol" {
			id = "cdp.protocol.read"
		}
		if kind == "mcp" && (stmt.Key == "tools" || len(stmt.Args) > 0 && stmt.Args[0] == "tools") {
			id = "mcp.tools.list"
		}
	case "read":
		id = kind + ".read"
	case "call":
		payload := unquoteWord(strings.TrimSpace(stmt.Payload))
		if kind == "mcp" && !strings.HasPrefix(payload, "{") && !strings.HasPrefix(payload, "[") {
			name, _, bad := splitToolCall(stmt, stmt.Payload)
			if bad != nil {
				return authorization.Plan{}, fmt.Errorf("%s", bad.Body)
			}
			return operationPlan("mcp.tool.invoke", authorization.Evidence{ToolName: name}), nil
		}
		var value any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return authorization.Plan{}, fmt.Errorf("invalid protocol frame")
		}
		return frameOperationPlan(kind, value)
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
			return operationPlan("mcp.tool.invoke", authorization.Evidence{ToolName: name}), nil
		case "tools/list":
			return operationPlan("mcp.tools.list", authorization.Evidence{}), nil
		default:
			return operationPlan("mcp.raw", authorization.Evidence{}), nil
		}
	case "cdp":
		id := "cdp.method.invoke"
		switch method {
		case "":
			id = "cdp.raw"
		case "Target.attachToTarget":
			id = "cdp.attach"
		case "Target.detachFromTarget":
			id = "cdp.detach"
		}
		return operationPlan(id, authorization.Evidence{Method: method}), nil
	case "jrpc":
		id := "jrpc.method.invoke"
		if method == "" {
			id = "jrpc.raw"
		}
		return operationPlan(id, authorization.Evidence{Method: method}), nil
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
	if transient {
		plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "cdp.attach"}, authorization.Requirement{Operation: "cdp.detach"})
	}
	return s.checkAgentPeerEffect(ctx, stmt, plan)
}
