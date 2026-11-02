package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
)

// PreviewPolicy plans a request against a proposed policy. It deliberately does
// not call runDirective, admission effects, Acquire, manager Touch, or task
// creation.
func (s *Server) PreviewPolicy(ctx context.Context, p auth.AgentPolicy, input auth.PolicyPreviewInput) auth.PolicyPreview {
	ctx = auth.WithoutAudit(ctx)
	out := auth.PolicyPreview{Requirements: []auth.PolicyPreviewRequirement{}}
	var plan authorization.Plan
	if input.Tool != "" {
		request := toolRequest(input.Tool, input.Arguments)
		resolved, err := capability.AgentToolPlan(input.Tool, input.Arguments)
		if err != nil {
			out.Reason = err.Error()
			return out
		}
		plan = resolved
		if bad := s.previewToolResource(ctx, p, input.Tool, request); bad != "" {
			out.Resource = bad
		}
		out.Recipes = recipesForPlan(plan)
		return previewDecision(p, plan, out)
	}

	win, derr := parseTermArgs(input.TermArguments)
	if derr != nil {
		out.Reason = derr.Msg
		return out
	}
	file, err := directive.ParseWithSubst(input.Cmd, directive.Subst{Named: win.Vars, Pos: win.Args})
	if err != nil {
		if stmt, resolved, cycle := aliases.expandErrorCycle(err); resolved {
			plan, err = s.previewStatementPlan(stmt)
		} else if len(cycle) > 0 {
			out.Reason = "alias expansion forms a cycle"
		} else {
			out.Reason = err.Error()
		}
		if err != nil {
			out.Reason = err.Error()
			return out
		}
	} else if file.Empty {
		out.Reason = "an empty command has no operations to authorize"
		return out
	} else {
		for _, stmt := range file.Statements {
			part, err := s.previewStatementPlan(stmt)
			if err != nil {
				out.Reason = err.Error()
				return out
			}
			plan.Requirements = append(plan.Requirements, part.Requirements...)
			if bad := s.previewStatementResource(ctx, p, stmt); bad != "" {
				out.Resource = bad
			}
		}
	}
	out.Recipes = recipesForPlan(plan)
	return previewDecision(p, plan, out)
}

func previewDecision(p auth.AgentPolicy, plan authorization.Plan, out auth.PolicyPreview) auth.PolicyPreview {
	if len(plan.Requirements) == 0 {
		out.Reason = "the request resolves to no operation"
		return out
	}
	policy := p.AuthorizationPolicy()
	for _, req := range plan.Requirements {
		op, known := authorization.Lookup(req.Operation)
		item := auth.PolicyPreviewRequirement{Operation: req.Operation, Name: op.Name}
		if !known {
			item.Reason = "unknown operation"
		} else if op.Global && p.HostMode != "all" {
			item.Reason = "operation changes shared configuration and requires all hosts"
		} else if grant, ok := policy.Actions[req.Operation]; !ok {
			item.Reason = "operation is not granted"
		} else if grant.Effect != authorization.EffectAllow {
			item.Reason = "operation is denied"
		} else if grant.Constraints != nil {
			if ok, reason := grant.Constraints.Satisfied(req.Evidence); !ok {
				item.Reason = "constraint does not match: " + reason
			}
		}
		item.Allowed = item.Reason == ""
		out.Requirements = append(out.Requirements, item)
		if !item.Allowed && out.Reason == "" {
			out.Reason = fmt.Sprintf("%s: %s", item.Operation, item.Reason)
		}
	}
	out.Allowed = out.Reason == ""
	return out
}

func recipesForPlan(plan authorization.Plan) []string {
	have := map[string]bool{}
	for _, req := range plan.Requirements {
		have[req.Operation] = true
	}
	out := []string{}
	for _, recipe := range authorization.Recipes() {
		ok := true
		for _, id := range recipe.Requirements {
			ok = ok && have[id]
		}
		if ok {
			out = append(out, recipe.ID)
		}
	}
	return out
}

func (s *Server) previewToolResource(ctx context.Context, p auth.AgentPolicy, tool string, request mcpgo.CallToolRequest) string {
	if tool == "" || tool == termToolName {
		return ""
	}
	if _, bad := s.agentToolCheckV2(ctx, p, tool, request); bad != nil {
		return toolResultText(bad)
	}
	return ""
}

func (s *Server) previewStatementResource(ctx context.Context, p auth.AgentPolicy, stmt directive.Statement) string {
	if bad := s.agentStatementCheckV2(ctx, p, stmt); bad != nil {
		return bad.Body
	}
	return ""
}

func (s *Server) previewStatementPlan(stmt directive.Statement) (authorization.Plan, error) {
	if stmt.Op == "run" {
		return runOperationPlan(), nil
	}
	if isPeerKind(stmt.Kind) || isPeerHandle(stmt.Target) || stmt.Op == "call" || boolFlag(stmt, "frame") {
		return s.agentPeerPlan(stmt)
	}
	id := ""
	switch stmt.Kind {
	case "alias":
		id = map[string]string{"ls": "alias.list", "create": "alias.write", "edit": "alias.write", "rm": "alias.delete"}[stmt.Op]
	case "task":
		id = map[string]string{"ls": "task.list", "get": "task.info", "close": "task.cancel"}[stmt.Op]
	case "host":
		id = map[string]string{"ls": "profile.list", "get": "profile.get", "create": "profile.create", "edit": "profile.edit", "copy": "profile.copy", "rm": "profile.delete"}[stmt.Op]
	case "ext":
		id = map[string]string{"ls": "ext.list", "get": "ext.get", "open": "ext.use", "close": "ext.release", "create": "ext.create", "edit": "ext.update", "rm": "ext.delete"}[stmt.Op]
	case "forward":
		id = map[string]string{"ls": "forward.list", "open": "forward.open.local", "close": "forward.close"}[stmt.Op]
	case "file":
		if stmt.Op == "rm" {
			id = "file.delete"
		} else if stmt.Op == "write" {
			id = "file.write"
		} else if stmt.Op == "read" || stmt.Op == "get" {
			if stmt.Op == "get" {
				if value, ok := stmt.Flag("op"); ok {
					id = map[string]string{
						"stat": "file.stat", "realpath": "file.realpath", "readlink": "file.readlink",
						"getwd": "file.getwd", "statvfs": "file.statvfs", "urls": "file.urls",
						"truncate": "file.truncate", "symlink": "file.symlink", "link": "file.link",
					}[value]
				}
			}
			if id == "" {
				id = "file.read"
			}
		}
	default:
		switch stmt.Op {
		case "connect":
			plan := operationPlan("session.start", authorization.Evidence{})
			plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.open"})
			if strings.TrimSpace(stmt.Payload) != "" {
				plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.exec"})
			}
			return plan, nil
		case "open":
			plan := operationPlan("shell.open", authorization.Evidence{})
			if strings.TrimSpace(stmt.Payload) != "" {
				plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.exec"})
			}
			return plan, nil
		case "input":
			id = "shell.input"
		case "key":
			id = "shell.key"
		case "resize":
			id = "shell.resize"
		case "reader":
			if boolFlag(stmt, "drop") {
				id = "shell.reader.unregister"
			} else {
				id = "shell.reader.register"
			}
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
		case "notify":
			id = "user.notify"
		}
	}
	if id == "" {
		return authorization.Plan{}, fmt.Errorf("preview does not classify %s %s", stmt.Kind, stmt.Op)
	}
	return operationPlan(id, authorization.Evidence{}), nil
}

func runOperationPlan() authorization.Plan {
	plan := operationPlan("shell.input", authorization.Evidence{})
	plan.Requirements = append(plan.Requirements,
		authorization.Requirement{Operation: "shell.key", Evidence: authorization.Evidence{Keys: []string{"enter"}, Repeat: 1}},
		authorization.Requirement{Operation: "shell.output"},
	)
	return plan
}
