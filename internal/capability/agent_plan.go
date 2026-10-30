package capability

import (
	"fmt"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	textencoding "github.com/open-mcp-ai/termcp/internal/encoding"
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
		if getString(args, "command", "") != "" || len(getStringSlice(args, "args")) > 0 {
			plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.exec"})
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
		id = "shell.marks"
	case "file_perm", "file_link", "file_fs":
		action := getString(args, "action", "")
		valid := map[string]string{"chmod": "file_perm", "chown": "file_perm", "chtimes": "file_perm", "readlink": "file_link", "symlink": "file_link", "link": "file_link", "truncate": "file_fs", "realpath": "file_fs", "statvfs": "file_fs"}
		if valid[action] != tool {
			return authorization.Plan{}, fmt.Errorf("unclassified file action %q", action)
		}
		id = "file." + action
	case "file_read", "file_write", "file_replace":
		id = "file." + strings.TrimPrefix(tool, "file_")
		form := "inline"
		local := getString(args, "local_path", "") != ""
		if local {
			form = "local"
		}
		e.Forms = []string{form}
		if tool == "file_read" {
			// A positive length is a provable upper bound. An unbounded stream isn't.
			if n := int64(getFloat64(args, "length", 0)); n > 0 {
				e.Bytes = n
				e.HasBytes = true
			}
		} else if !local {
			data := getString(args, "data", "")
			if tool == "file_replace" {
				data = getString(args, "new_text", "")
				e.Bytes = int64(len(data))
				e.HasBytes = true
			} else {
				mode := getString(args, "mode", "")
				var raw []byte
				var err error
				switch mode {
				case "hex":
					raw, err = textencoding.HexDecode(data)
				case "text", "":
					if hex, ok := strings.CutPrefix(data, "hex:"); ok {
						raw, err = textencoding.HexDecode(hex)
					} else {
						raw = textencoding.DecodeText(data)
					}
				default:
					return authorization.Plan{}, fmt.Errorf("unclassified write mode %q", mode)
				}
				if err != nil {
					return authorization.Plan{}, err
				}
				e.Bytes = int64(len(raw))
				e.HasBytes = true
			}
		}
		plan := operationPlan(id, e)
		// Only the read handler's file form writes to the local filesystem.
		if local && (tool != "file_read" || getString(args, "mode", "text") == "file") {
			other := "file.read"
			if tool == "file_read" {
				other = "file.write"
			}
			plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: other, Evidence: authorization.Evidence{Forms: []string{"local"}}})
		}
		return plan, nil
	case "forward":
		action := getString(args, "action", "")
		switch action {
		case "list", "close":
			id = "forward." + action
		case "local", "remote", "dynamic":
			id = "forward.open." + action
		default:
			return authorization.Plan{}, fmt.Errorf("unclassified forward action")
		}
	case "ssh_config":
		id = "profile." + getString(args, "action", "")
	case "extension":
		action := getString(args, "action", "")
		if action == "restart" {
			action = "retry"
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
	legacy := AgentRequirement(tool, args)
	out := AgentTargets{ProfileArgument: legacy.ProfileArgument, NeedsLocal: legacy.NeedsLocal}
	if tool == "extension" {
		switch getString(args, "action", "") {
		case "get", "use", "release", "retry", "restart":
			out.ExtensionArgument = "name"
		}
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
