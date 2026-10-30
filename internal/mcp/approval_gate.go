package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// Review mode over the MCP surface.
//
// The gate covers every MCP operation that changes the remote host: terminal
// input, file transfers, and port forwards. Review used to cover only the
// terminal, which left the two operations that can be just as irreversible —
// overwriting a file, exposing a port — running without a decision. A gate with
// a hole in it is worse than no gate, because it reads as protection.
//
// It covers the HTTP entrance to the same command space for the same reason:
// POST /api/term resolves to the same directives and reaches these handlers, so
// a caller that arrived over HTTP is gated by this code without being told it
// exists. The only thing the entrance changes is what the record SAYS — the
// queue's Source and the transcript's status — which is why both are read from
// the context (callerSource) rather than passed in.
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

// callerSourceKey carries which entrance submitted an operation, for the two
// places that record it: the review queue's Source field and the transcript.
type callerSourceKey struct{}

// callerSource is the entrance this call arrived through: "mcp" for the tool
// surface, "rest" for POST /api/term. It is a context value rather than a
// parameter because the operation is submitted four calls deep inside a
// handler that must not know which transport carried it.
//
// The default is "mcp" because every existing caller is one: only the HTTP
// entrance sets anything, so a path that forgets to inherit this keeps the
// behaviour it has always had instead of silently claiming to be REST.
func callerSource(ctx context.Context) string {
	if v, ok := ctx.Value(callerSourceKey{}).(string); ok && v != "" {
		return v
	}
	return "mcp"
}

// withCallerSource marks a context with the entrance that opened it.
func withCallerSource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, callerSourceKey{}, src)
}

// callerInputSource is the transcript status those bytes get: an agent's
// keystrokes are AI input, a caller's own HTTP write is API input.
//
// The mapping matches the one the approval executor already applies when it
// replays a decision (internal/webui/approval.go), so a command line carries the
// same attribution whether it ran immediately or waited for a human — and the
// transcript never says a human typed something an agent sent.
func callerInputSource(ctx context.Context) session.InputSource {
	if callerSource(ctx) == "rest" {
		return session.InputFromAPI
	}
	return session.InputFromAI
}

// gatedPayload is what a held operation stores so it can be replayed.
type gatedPayload struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
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
	// Resolve the argument the same way every handler does. The gate must not
	// inspect the raw spelling: a locator (termcp://#<sid>) would not be found here,
	// the gate would decline, and the handler would then resolve it itself and
	// perform the operation — an approval-gated write executing with nobody asked.
	// The lookup is by the resolved session, so a locator gates exactly like the id
	// it names. A failure here is not reported: the handler owns the error message.
	sess, bad := s.requireSession(sessionID)
	if bad != nil || sess == nil {
		return nil, false
	}
	// Which operations this session holds. Asked before the arguments are
	// marshalled because the kind depends on them for `forward` (open vs close),
	// and a session scoped to one of the two must let the other through.
	if !sess.GatesKind(kindFor(tool, request.GetArguments())) {
		return nil, false
	}
	args := request.GetArguments()
	summary := summarizeOperation(tool, args)
	payload, err := json.Marshal(gatedPayload{Tool: tool, Args: args})
	if err != nil {
		return toolError(CodeOperationFailed, "cannot describe the operation for review: %s", err.Error()), true
	}
	requestID, err := sess.SubmitOperation(approval.Submission{
		Kind:    kindFor(tool, args),
		Source:  callerSource(ctx),
		Summary: summary,
		Payload: payload,
	})
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), true
	}
	return reviewPendingOperationResult(summary, approvalCorrelation(sess.ApprovalQueue(), requestID, sess.ID)), true
}

// reviewWaitTail is the operational half of every review-mode reply: what the
// agent must not do while a request waits. It is a shared constant because a held
// command line and a held operation answer the same question, and two spellings of
// one prohibition invite the agent to treat the weaker one as the rule.
const reviewWaitTail = "A human decides it in the Web UI; nothing happens until then. " +
	"Use approval_id only to correlate this request: do not poll, resubmit, reword or retry - WAIT."

// reviewPendingReply is the single constructor of a review-mode reply, shared by
// the two points where review intercepts a call: a held command line
// (handlers_shell.go) and a held file or forward operation (below).
//
// A queued request carries its stable approval_id so an MCP host can correlate
// the result with the approval worklist. The id is metadata, not a decision
// capability: the agent still cannot decide the request and must not poll it.
//
// The JSON shape is built once, here, so the two replies cannot drift apart: a
// model branches on approved/review_pending, and a shape that differed between
// them would read as two different states.
func reviewPendingReply(msg string, request *approval.Request) *mcpgo.CallToolResult {
	fields := map[string]any{
		"ok":             true,
		"approved":       false,
		"review_pending": true,
		"message":        msg,
	}
	if request != nil {
		fields["approval_id"] = request.ID
		if request.SessionID != "" {
			fields["session_id"] = request.SessionID
		}
		if request.ExpiresAt > 0 {
			fields["expires_at"] = request.ExpiresAt
		}
	}
	return jsonResult(fields)
}

// reviewPendingOperationResult is the reply to an operation that review mode
// intercepted. The summary is echoed back so the agent can tell which of its calls
// is now waiting: unlike a command line, none of these operations left a trace
// anywhere the agent can read later.
func reviewPendingOperationResult(summary string, request *approval.Request) *mcpgo.CallToolResult {
	return reviewPendingReply(fmt.Sprintf("Submitted for review: %s. %s", summary, reviewWaitTail), request)
}

// approvalCorrelation reads the request metadata returned by the queue. If the
// immediate lookup fails, the stable ID and session are still returned so the
// caller can correlate the request without inviting a resubmission.
func approvalCorrelation(queue *approval.Queue, requestID, sessionID string) *approval.Request {
	if requestID == "" {
		return nil
	}
	if queue != nil {
		if request, err := queue.Get(requestID); err == nil {
			return &request
		}
	}
	return &approval.Request{ID: requestID, SessionID: sessionID}
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
	handler := s.handlerFor(p.Tool)
	if handler == nil {
		return fmt.Errorf("no handler for approved operation %q", p.Tool)
	}
	if p.Args == nil {
		p.Args = map[string]any{}
	}
	// Re-resolve the session id from the request when the operation acts on one:
	// it is authoritative, and a payload that named a different session must not
	// be able to redirect the operation that a human approved. An operation that
	// acts on the instance instead (installing an extension, creating a profile)
	// has no session to pin, and injecting an empty one would only overwrite what
	// the payload carried.
	if req.SessionID != "" {
		p.Args["session_id"] = req.SessionID
	}

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
