package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/approval"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Review mode over the MCP surface.
//
// The gate covers every MCP operation that changes the remote host: terminal
// input, file transfers, and port forwards. Review used to cover only the
// terminal, which left the two operations that can be just as irreversible —
// overwriting a file, exposing a port — running without a decision. A gate with
// a hole in it is worse than no gate, because it reads as protection.
//
// What is deliberately NOT gated is the human's own surface: the WebSocket
// terminal and the Web UI's file browser (which is the REST file API). The rule
// is "the AI's programmatic surface is reviewed; the operator's interactive one
// is not". Gating the operator's keyboard locked them out of their own session
// the moment they turned review on — they could not even interrupt a command
// they were watching, which is not what reviewing the AI means.
//
// A token holder can bypass this by calling the REST API directly, but they can
// approve their own request just as directly, so the gate was never protecting
// against them. It protects against an agent doing something nobody looked at.

// approvedOpKey marks a context whose operation has already been approved.
//
// It is how the executor replays an operation through the same handler that
// submitted it, without that handler queueing it again. An unexported key type
// means no other package can forge it.
type approvedOpKey struct{}

// approvedContext reports whether this call is a replay of an approved request.
func approvedContext(ctx context.Context) bool {
	v, _ := ctx.Value(approvedOpKey{}).(bool)
	return v
}

// withApproved marks a context as an approved replay.
func withApproved(ctx context.Context) context.Context {
	return context.WithValue(ctx, approvedOpKey{}, true)
}

// gatedTool is one MCP operation that review mode can hold.
//
// tool is the MCP tool name and kind is what the queue records, so the browser
// can label the request and the executor can find its handler. Keeping both in
// one table means a new gated operation is one row, not three edits that can
// drift apart.
type gatedTool struct {
	tool string
	kind approval.Kind
}

// gatedTools lists every MCP tool whose effect on the host needs a decision.
//
// Reads are absent on purpose: listing a directory or reading a file changes
// nothing, and putting them behind a decision would train a reviewer to click
// Accept without reading, which is how a gate stops working.
var gatedTools = []gatedTool{
	{"file_write", approval.KindFileWrite},
	{"file_delete", approval.KindFileDelete},
	{"file_rename", approval.KindFileRename},
	{"file_mkdir", approval.KindFileMkdir},
	{"file_perm", approval.KindFilePerm},
	{"file_link", approval.KindFileLink},
	{"file_fs", approval.KindFileTruncate},
	{"forward", approval.KindForwardOpen},
}

// kindForTool maps a tool name to the kind recorded on the request.
func kindForTool(tool string) approval.Kind {
	for _, g := range gatedTools {
		if g.tool == tool {
			return g.kind
		}
	}
	return approval.Kind(tool)
}

// gateOperation holds an MCP operation for review when the session gates.
//
// It reports whether the caller should stop: true means the operation was
// queued (or failed to queue) and the returned result is the reply to send.
//
// The payload stored on the request is the tool name plus the call's own
// arguments, so the executor replays the operation by invoking the same handler.
// That is what keeps one implementation of every file and forward operation:
// nothing here knows how to write a file or open a port.
func (s *Server) gateOperation(ctx context.Context, request mcpgo.CallToolRequest, tool, sessionID string) (*mcpgo.CallToolResult, bool) {
	if approvedContext(ctx) {
		return nil, false
	}
	if sessionID == "" {
		return nil, false // the handler will report the missing session
	}
	sess := s.sessMgr.Get(sessionID)
	if sess == nil || !sess.ApprovalEnabled() {
		return nil, false
	}

	args := request.GetArguments()
	summary := summarizeOperation(tool, args)
	payload, err := json.Marshal(gatedPayload{Tool: tool, Args: args})
	if err != nil {
		return toolError(CodeOperationFailed, "cannot describe the operation for review: %s", err.Error()), true
	}
	kind := kindForTool(tool)
	// A forward's kind depends on the action: opening a tunnel and closing one
	// are different decisions, and the worklist should say which is which.
	if tool == "forward" {
		switch getString(args, "action", "") {
		case "close":
			kind = approval.KindForwardClose
		default:
			kind = approval.KindForwardOpen
		}
	}
	if _, err := sess.SubmitOperation(approval.Submission{
		Kind:    kind,
		Source:  "mcp",
		Summary: summary,
		Payload: payload,
	}); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), true
	}
	return reviewPendingOperationResult(summary), true
}

// gatedPayload is what a held operation stores so it can be replayed.
type gatedPayload struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// reviewPendingOperationResult is the reply to an operation that review mode
// intercepted. Like the command-line case it carries no id: the agent cannot
// decide its own request, so an id would only invite a retry loop.
func reviewPendingOperationResult(summary string) *mcpgo.CallToolResult {
	return jsonResult(map[string]any{
		"ok":             true,
		"approved":       false,
		"review_pending": true,
		"message": fmt.Sprintf(
			"Submitted for review: %s. A human decides it in the Web UI; nothing happens until then.",
			summary),
	})
}

// summarizeOperation renders one line a reviewer can decide on.
//
// This is the whole value of the feature: a reviewer who cannot tell what they
// are approving is not reviewing anything. It reads the same arguments the
// handler will use, so the summary cannot describe something different from
// what will run.
func summarizeOperation(tool string, args map[string]any) string {
	s := func(k string) string { return strings.TrimSpace(getString(args, k, "")) }
	n := func(k string, def float64) float64 { return getFloat64(args, k, def) }

	switch tool {
	case "file_write":
		path := s("remote_path")
		if lp := s("local_path"); lp != "" {
			return fmt.Sprintf("write to %s from local file %s", path, lp)
		}
		return fmt.Sprintf("write %s to %s", describeBytes(len(s("data")), s("mode")), path)

	case "file_delete":
		return "delete " + s("remote_path")

	case "file_rename":
		return fmt.Sprintf("move %s to %s", s("from_path"), s("to_path"))

	case "file_mkdir":
		return "create directory " + s("remote_path")

	case "file_perm":
		path := s("remote_path")
		switch s("action") {
		case "chmod":
			return fmt.Sprintf("chmod %04o on %s", int(n("mode", 0)), path)
		case "chown":
			return fmt.Sprintf("chown uid=%d gid=%d on %s", int(n("uid", 0)), int(n("gid", 0)), path)
		case "chtimes":
			return "set times on " + path
		default:
			return "change permissions on " + path
		}

	case "file_link":
		switch s("action") {
		case "readlink":
			return "read link " + s("remote_path") // read-only; kept for completeness
		case "symlink":
			return fmt.Sprintf("symlink %s -> %s", s("link_path"), s("target"))
		default:
			return fmt.Sprintf("hard link %s -> %s", s("new_path"), s("existing_path"))
		}

	case "file_fs":
		return fmt.Sprintf("truncate %s to %d bytes", s("remote_path"), int64(n("size", 0)))

	case "forward":
		switch s("action") {
		case "close":
			return "close port forward " + s("forward_id")
		case "remote":
			return fmt.Sprintf("open remote forward %s:%d -> %s:%d",
				s("local_host"), int(n("local_port", 0)), s("remote_host"), int(n("remote_port", 0)))
		case "dynamic":
			return fmt.Sprintf("open SOCKS5 proxy on port %d", int(n("local_port", 0)))
		default:
			return fmt.Sprintf("open local forward :%d -> %s:%d",
				int(n("local_port", 0)), s("remote_host"), int(n("remote_port", 0)))
		}
	}
	return tool
}

// describeBytes says how much a write carries, in the unit a human reads.
// The byte count is what the reviewer actually needs to judge scale; the mode
// is stated because hex and text mean different things for the same length.
func describeBytes(dataLen int, mode string) string {
	unit := "bytes"
	switch {
	case dataLen >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(dataLen)/(1<<20))
	case dataLen >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(dataLen)/(1<<10))
	}
	if mode != "" && mode != "text" {
		unit = mode + " bytes"
	}
	return fmt.Sprintf("%d %s", dataLen, unit)
}

// ExecuteApprovedOperation replays a non-terminal approved request.
//
// It is the counterpart of gateOperation: the payload names the tool and holds
// the original arguments, so the operation runs through exactly the handler that
// submitted it. The context marks the call as approved, which is what stops that
// handler from queueing it a second time.
//
// It is wired into the Web UI's decision endpoint by main, because the decision
// is made there (by the human) while the operations live here (with the SFTP and
// forward machinery).
func (s *Server) ExecuteApprovedOperation(req approval.Request) error {
	var p gatedPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return fmt.Errorf("cannot read the approved operation: %w", err)
	}
	handler := s.gatedHandler(p.Tool)
	if handler == nil {
		return fmt.Errorf("no handler for approved operation %q", p.Tool)
	}
	if p.Args == nil {
		p.Args = map[string]any{}
	}
	// Re-resolve the session id from the request: it is authoritative, and a
	// payload that named a different session must not be able to redirect the
	// operation that a human approved.
	p.Args["session_id"] = req.SessionID

	call := mcpgo.CallToolRequest{}
	call.Params.Name = p.Tool
	call.Params.Arguments = p.Args

	res, err := handler(withApproved(context.Background()), call)
	if err != nil {
		return err
	}
	if res != nil && res.IsError {
		return fmt.Errorf("%s", toolResultText(res))
	}
	return nil
}

// gatedHandler returns the handler for a gated tool name.
func (s *Server) gatedHandler(tool string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	switch tool {
	case "file_write":
		return s.handleFileWrite
	case "file_delete":
		return s.handleFileDelete
	case "file_rename":
		return s.handleFileRename
	case "file_mkdir":
		return s.handleFileMakeDir
	case "file_perm":
		return s.handleFilePerm
	case "file_link":
		return s.handleFileLinkOp
	case "file_fs":
		return s.handleFileFsOp
	case "forward":
		return s.handleForwardOps
	default:
		return nil
	}
}

// toolResultText extracts the text of an error result for the caller's message.
//
// A failed tool result carries a JSON envelope — {"error_code":…,"error":…} — so
// agents can branch on the failure kind without parsing prose. That envelope is
// for a program. This text is shown to the HUMAN who just approved the operation,
// in the Web UI's error toast, where the raw JSON reads as noise around the one
// sentence that matters:
//
//	approved but execution failed: {"error_code":"operation_failed","error":"remove /tmp/x: file does not exist"}
//
// So the message is unwrapped when it is that envelope, and passed through
// untouched when it is not (an unknown shape must still be shown, not swallowed).
func toolResultText(res *mcpgo.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(mcpgo.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("; ")
			}
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 {
		return "the approved operation failed"
	}
	return unwrapToolError(b.String())
}

// unwrapToolError returns the human-readable part of a tool error envelope, or
// the input unchanged if it is not one.
func unwrapToolError(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return text
	}
	var env struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil || env.Error == "" {
		return text
	}
	return env.Error
}
