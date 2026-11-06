package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// termcpToolCheck is what a downstream declared, once it has been verified.
//
// The point of verifying rather than trusting is that a wrong declaration must
// not be expensive. An extension that says "my `term` tool speaks TERM/1" and
// then does not is a typo, and a typo must not take the instance down or fail
// every call through it: it degrades to an ordinary peer, and the reason is
// printed where an operator looks for it.
type termcpToolCheck struct {
	// tool is the declared tool's name, empty when the extension declares none.
	tool string
	// reason is why a declaration was not accepted, empty when it was.
	reason string
}

// OK reports whether routing through this extension is allowed.
func (c termcpToolCheck) OK() bool { return c.tool != "" && c.reason == "" }

// checkTermcpTool verifies an extension's `termcp_tool` declaration against the
// peer it just mounted: the tool must exist, and its input schema must be a
// single required string property.
//
// THE SHAPE IS THE WHOLE CONTRACT. TERM/1 is a line of text; a tool that takes
// `{query, top_k}` cannot be handed one, and a tool that takes `{cmd, timeout}`
// would make the downstream implement this side's window semantics. Requiring
// exactly one string property means the recursion has one meaning, and a
// downstream that wants to pass a window can say so in its own tool's
// documentation — not by silently changing what `run` forwards.
//
// A failure here is never fatal. It is recorded and reported, and the extension
// stays usable as the peer it also is.
func (s *Server) checkTermcpTool(ctx context.Context, ext *extensions.Extension, h *jrpcHandle) termcpToolCheck {
	declared := strings.TrimSpace(ext.TermcpTool)
	if declared == "" {
		return termcpToolCheck{}
	}
	if h == nil {
		return termcpToolCheck{tool: declared, reason: "the peer is not mounted, so its tools cannot be read"}
	}
	raw, err := h.ch.Call(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if err != nil {
		return termcpToolCheck{tool: declared, reason: "tools/list failed: " + err.Error()}
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return termcpToolCheck{tool: declared, reason: "tools/list did not answer with a JSON-RPC frame"}
	}
	tool, ok := findTool(reply, declared)
	if !ok {
		return termcpToolCheck{tool: declared, reason: fmt.Sprintf(
			"it has no tool named %q; `get <peer> tools` lists what it does have", declared)}
	}
	if problem := singleStringArgumentProblem(tool); problem != "" {
		return termcpToolCheck{tool: declared, reason: problem}
	}
	return termcpToolCheck{tool: declared}
}

// findTool locates one tool in a tools/list result and returns its input schema.
func findTool(reply map[string]any, name string) (map[string]any, bool) {
	result, _ := reply["result"].(map[string]any)
	if result == nil {
		return nil, false
	}
	list, _ := result["tools"].([]any)
	for _, item := range list {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		if got, _ := tool["name"].(string); got == name {
			return tool, true
		}
	}
	return nil, false
}

// singleStringArgumentProblem reports why a tool cannot be a TERM/1 downstream,
// or "" when it can.
//
// The rule is deliberately about the SCHEMA rather than about what the tool does
// with the value: this side cannot know the latter, and pretending to would mean
// guessing. One required property, of type string, and no other required ones —
// a tool with an optional extra parameter still works, because the caller here
// only ever sends the one it declared.
func singleStringArgumentProblem(tool map[string]any) string {
	name, _ := tool["name"].(string)
	schema, _ := tool["inputSchema"].(map[string]any)
	if schema == nil {
		return fmt.Sprintf("%q has no input schema, so it cannot be shown to take one command string", name)
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return fmt.Sprintf("%q takes no arguments, so there is nothing to put the command line in", name)
	}
	required, _ := schema["required"].([]any)
	var stringProps []string
	for key, raw := range props {
		spec, _ := raw.(map[string]any)
		if spec == nil {
			continue
		}
		if typ, _ := spec["type"].(string); typ == "string" {
			stringProps = append(stringProps, key)
		}
	}
	if len(stringProps) != 1 {
		return fmt.Sprintf("%q must take exactly one string argument to speak TERM/1; it takes %d", name, len(stringProps))
	}
	if len(required) > 1 {
		return fmt.Sprintf("%q requires %d arguments; a TERM/1 downstream takes one command string", name, len(required))
	}
	for _, raw := range required {
		key, _ := raw.(string)
		if key != "" && key != stringProps[0] {
			return fmt.Sprintf("%q requires %q, which is not its string argument %q", name, key, stringProps[0])
		}
	}
	return ""
}

// extTermcpNote is the sentence an `open ext` reply adds when a declaration was
// refused, so the operator learns it from the same place they learn the handle.
func extTermcpNote(name string, c termcpToolCheck) string {
	if c.tool == "" {
		return ""
	}
	if c.reason != "" {
		return "\ntermcp_tool = " + c.tool + " is not usable: " + c.reason +
			"\nThe extension still works as an ordinary MCP peer."
	}
	return "\ntermcp_tool = " + c.tool + ": this peer speaks TERM/1, so it can be routed to."
}

// keep the envelope import honest if this file ever stops using it.
var _ = envelope.Slot{}
