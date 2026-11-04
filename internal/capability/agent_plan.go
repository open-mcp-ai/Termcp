package capability

import (
	"fmt"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/fileop"
	"strings"
)

// AgentToolPlan describes the actual handler branch, using the handler's defaults.
// No request-supplied operation id or bypass flag is used. DSL adapters that call
// a tool use this same classifier after translating their flags.
func AgentToolPlan(tool string, args map[string]any) (authorization.Plan, error) {
	id, known := IDForTool(tool)
	e := authorization.Evidence{}
	switch tool {
	case "session_start", "shell_open":
		plan := operationPlan(id, e)
		if tool == "session_start" {
			plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.open"})
		}
		return plan, nil
	case "shell_key":
		key := strings.ToLower(strings.TrimSpace(getString(args, "key", "")))
		if key == "escape" {
			key = "esc"
		}
		repeat := int(getFloat64(args, "repeat", 1))
		if repeat < 1 {
			repeat = 1
		}
		e.Keys = []string{key}
		e.Repeat = repeat
	case "message":
		if getString(args, "action", "") != "list" {
			return authorization.Plan{}, fmt.Errorf("unclassified message action")
		}
		id = "session.message"
	case "file_perm", "file_link", "file_fs":
		action := getString(args, "action", "")
		valid := map[string]string{"chmod": "file_perm", "chown": "file_perm", "chtimes": "file_perm", "readlink": "file_link", "symlink": "file_link", "link": "file_link", "truncate": "file_fs", "realpath": "file_fs", "statvfs": "file_fs"}
		if valid[action] != tool {
			return authorization.Plan{}, fmt.Errorf("unclassified file action %q", action)
		}
		id = "file." + strings.TrimPrefix(tool, "file_")
	case "file_read", "file_write", "file_replace":
		if tool == "file_replace" {
			id = "file.write"
		} else {
			id = "file." + strings.TrimPrefix(tool, "file_")
		}
		e.Forms = []string{"inline"}
		local := false
		switch tool {
		case "file_read":
			p := fileop.ReadFromArgs(args)
			local = p.LocalTransfer()
			e.Bytes, e.HasBytes = p.ByteLimit()
		case "file_write":
			p := fileop.WriteFromArgs(args)
			local = p.LocalPath != ""
			var err error
			e.Bytes, e.HasBytes, err = p.ByteLimit()
			if err != nil {
				return authorization.Plan{}, err
			}
		case "file_replace":
			e.Bytes = int64(len(getString(args, "new_text", "")))
			e.HasBytes = true
		}
		if tool == "file_replace" {
			local = false
		}
		if local {
			e.Forms = []string{"local"}
		}
		plan := operationPlan(id, e)
		if local {
			other := "file.read"
			if tool == "file_read" {
				other = "file.write"
			}
			// The same bounded transfer reads one endpoint and writes the other.
			plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: other, Evidence: e})
		}
		return plan, nil
	case "forward":
		action := getString(args, "action", "")
		switch action {
		case "list", "close":
			id = "forward." + action
		case "local", "remote", "dynamic":
			id = "forward.open"
		default:
			return authorization.Plan{}, fmt.Errorf("unclassified forward action")
		}
	case "ssh_config":
		action := getString(args, "action", "")
		if action == "" || action == "get" {
			action = "list"
		}
		id = "profile." + action
	case "extension":
		action := getString(args, "action", "")
		if action == "restart" {
			action = "retry"
		}
		if action == "use" || action == "release" {
			action = "get"
		}
		id = "ext." + action
	default:
		if !known {
			return authorization.Plan{}, fmt.Errorf("unclassified tool %q", tool)
		}
	}
	if _, ok := authorization.Lookup(id); !ok {
		return authorization.Plan{}, fmt.Errorf("unclassified operation %q", id)
	}
	return operationPlan(id, e), nil
}

// AgentTargets describes the resource evidence required by a fine-grained call.
// The adapter resolves these references read-only against live server state.
type AgentTargets struct {
	ProfileArgument   string
	ExtensionArgument string
	NeedsLocal        bool
}

func AgentToolTargets(tool string, args map[string]any) AgentTargets {
	out := AgentTargets{}
	switch tool {
	case "session_start":
		out.ProfileArgument = "ssh_config"
	case "shell_detect":
		out.NeedsLocal = true
	case "ssh_config":
		switch getString(args, "action", "list") {
		case "get", "edit", "delete":
			out.ProfileArgument = "name"
		case "copy":
			out.ProfileArgument = "source_name"
		}
	case "extension":
		switch getString(args, "action", "") {
		case "get", "use", "release", "retry", "restart", "delete":
			out.ExtensionArgument = "name"
		case "update":
			out.ExtensionArgument = "name"
			if getString(args, "target", "") != "" {
				out.ProfileArgument = "target"
			}
		case "create":
			if getString(args, "target", "") != "" {
				out.ProfileArgument = "target"
			}
		}
	}
	switch tool {
	case "file_read":
		out.NeedsLocal = fileop.ReadFromArgs(args).LocalTransfer()
	case "file_write":
		out.NeedsLocal = fileop.WriteFromArgs(args).LocalPath != ""
	case "file_replace":
		out.NeedsLocal = false // the replacement handler never uses local_path
	}
	return out
}
func operationPlan(id string, evidence authorization.Evidence) authorization.Plan {
	return authorization.Plan{Requirements: []authorization.Requirement{{Operation: id, Evidence: evidence}}}
}
func getString(args map[string]any, key, fallback string) string {
	v, ok := args[key].(string)
	if !ok {
		return fallback
	}
	return v
}
func getFloat64(args map[string]any, key string, fallback float64) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return fallback
}
func getStringSlice(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
