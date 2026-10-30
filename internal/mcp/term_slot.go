package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// jsonMarshalIndent renders a document body for a generated reply.
func jsonMarshalIndent(m map[string]any) (string, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// withFlags returns a copy of stmt whose flag map carries the given values.
//
// THE ADAPTER IS THE POINT OF THIS LAYER. The handlers below were written against
// the old spelling (`file <session> --action read --remote-path x`) and their
// internals are not what changed: an SFTP read is an SFTP read. Rewriting them to
// read a different field would have been six hundred lines of shuffling with a
// fresh bug in each. So the surface is translated here, once, and the machinery
// keeps working.
//
// The copy is shallow except for the flag map: a statement is shared between the
// statements of one cmd, and mutating the caller's map would make `a; b` where a is
// translated leak its flags into b.
// firstFlagOtherThan names a flag the caller set besides the one given.
//
// It exists so a call that asks for two things can be refused BY NAME. The
// alternative - doing one of them and reporting success - is the failure mode this
// codebase has been bitten by more than once: the caller believes it did both, and
// the half that was dropped is the one it cannot see.
func firstFlagOtherThan(stmt directive.Statement, except string) string {
	names := make([]string, 0, len(stmt.Flags))
	for name := range stmt.Flags {
		if name != except {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	// Map order is random and the message is read by a person: sort it.
	sort.Strings(names)
	return names[0]
}

func withFlags(stmt directive.Statement, set map[string]string) directive.Statement {
	flags := make(map[string]*string, len(stmt.Flags)+len(set))
	for k, v := range stmt.Flags {
		flags[k] = v
	}
	for k, v := range set {
		if v == "" {
			continue
		}
		val := v
		flags[k] = &val
	}
	out := stmt
	out.Flags = flags
	return out
}

// noContent returns a 204 slot carrying the statement's Path header.
func noContent(stmt directive.Statement) envelope.Slot {
	return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
}

// noContentSaying answers "this succeeded and delivered nothing, and here is why"
// as a 204 whose reason rides in a HEADER, never in the body.
//
// HTTP SAYS A 204 HAS NO CONTENT, AND THIS FORMAT IS HTTP ON PURPOSE. The package
// comment on internal/envelope states the reason: a model already has prior on HTTP. That
// prior is the whole value of the format, so a 204 carrying a sentence does not merely
// bend a rule — it teaches the caller that the one part of the reply it already
// understands is a lie about itself, and the next 204 is then unreadable. Measured:
// `read <channel> --frame` over bytes that are not a frame answered
//
//	TERM/1 204 No Content
//	Content-Range: bytes …
//
//	no complete frame yet — the bytes in front are not a frame: "\x1b[?9001h…"
//
// with the body contradicting the status line above it.
//
// THE CODE STAYS 204 AT EVERY CALL SITE, and the reason is the same at each: the
// statement ran, and nothing was delivered. The alternatives are worse, not louder —
// a 206 claims a partial delivery that does not exist (and an empty 206 reads as
// "folded", a different fact), a 200 with `(nothing)` in the body invents a content
// vocabulary for silence, and 202 claims work in progress nobody started.
//
// THE EXPLANATION IS NOT DROPPED, because here silence is worse than an error: "no
// complete frame yet" alone is the sentence a caller retries for ever, and a grep's
// "0 lines scanned" is invisible. Hint is the header this package already uses for
// "why this reply looks empty and what to do next" (see read's Waited/Hint pair in
// term_read.go), it renders in the ordered header block both projections carry, and
// it is one line by construction — a header cannot hold a newline, so every caller
// passes a value it has already flattened.
//
// The map is the caller's and is modified in place: every call site builds it for this
// reply alone.
func noContentSaying(headers map[string]string, why string) envelope.Slot {
	if headers == nil {
		headers = map[string]string{}
	}
	// A HEADER VALUE IS ONE LINE, ENFORCED HERE RATHER THAN TRUSTED. The why text is
	// assembled from caller data (a grep pattern may hold a newline), and a raw newline
	// would split the header block: the rest of the sentence would render below the
	// blank line, where it reads as a body — the very thing this helper exists to avoid.
	headers["Hint"] = strings.ReplaceAll(strings.ReplaceAll(why, "\r\n", " "), "\n", " ")
	return envelope.Slot{Code: 204, Headers: headers}
}

// closedSlot answers a close with the object it closed, in a header.
//
// A CLOSE IS THE LAST THING A CALLER DOES WITH AN OBJECT, AND IT WAS THE ONLY REPLY
// THAT SAID NOTHING. `close <session>` answered 204 with an empty body, so the caller
// could not tell "closed the session I named" from "closed something, somewhere" — the
// reported reaction was to run `ls` and see what was missing, a call spent confirming
// instead of advancing, on the step where a wrong answer is most expensive.
//
// The code stays 204: there is no content to return, and the REST entrance drops a body
// on 204, so a sentence there would reach MCP only. A header is carried by both.
//
// The NAME rides along when the object has one, because that is how the caller thinks
// of it: they wrote `--name explore` and the reply should say "explore", not only the
// uuid they copied out of a receipt three calls ago.
func closedSlot(stmt directive.Statement, address, name string) envelope.Slot {
	closed := address
	if strings.TrimSpace(name) != "" {
		closed += " (" + name + ")"
	}
	return envelope.Slot{Code: 204, Headers: map[string]string{
		"Path":   stmt.Path,
		"Closed": closed,
	}}
}

// badRequest returns a 400 slot for an invalid argument or missing parameter.
func badRequest(stmt directive.Statement, body string) envelope.Slot {
	return envelope.Slot{
		Code:    400,
		Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeInvalidArgument},
		Body:    body,
	}
}

// badRequestNoPath is badRequest for the replies that predate a statement — the
// argument screen at the entrance, and a directive error raised before a Path
// exists. There is no Path header to attach, and inventing one would echo a
// position the caller never wrote.
func badRequestNoPath(body string) envelope.Slot {
	return envelope.Slot{
		Code:    400,
		Headers: map[string]string{"Error-Code": CodeInvalidArgument},
		Body:    body,
	}
}

// ptrSlot is the address of a slot, for the helpers whose caller must return a
// *envelope.Slot rather than a value.
func ptrSlot(s envelope.Slot) *envelope.Slot { return &s }

// notFoundSlot returns a 404 slot with the given error code.
func notFoundSlot(stmt directive.Statement, code, body string) envelope.Slot {
	return envelope.Slot{
		Code:    404,
		Headers: map[string]string{"Path": stmt.Path, "Error-Code": code},
		Body:    body,
	}
}

// tailSlot keeps the last N lines of a reply, and says that it did.
//
// NOT THE TRANSCRIPT'S OWN TAIL SELECTOR. That one reads from the end of the whole
// buffer and IGNORES an offset when both are given (window.go switches on TailLines
// before Offset), so `run --tail 20` would hand back the previous command's last lines
// as part of this command's output — the exact confusion issue #194 is about. The
// window is read from this command's input first, and the tail is taken from it.
//
// The trim is a FACT, not a silent loss: the reply names how many lines it kept and how
// many the window held, and Content-Range still describes the stream span the window
// came from (so `read --cursor` continues from where this reply's bytes ended).
func tailSlot(slot envelope.Slot, stmt directive.Statement) envelope.Slot {
	v, ok := stmt.Flag("tail")
	if !ok || v == "" || slot.Code >= 400 || slot.Body == "" {
		return slot
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return slot
	}
	lines := strings.Split(strings.TrimRight(slot.Body, "\n"), "\n")
	if len(lines) <= n {
		return slot
	}
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	slot.Headers["Trimmed"] = fmt.Sprintf("the last %d of %d lines of this command's output; read the rest with `read`", n, len(lines))
	slot.Body = strings.Join(lines[len(lines)-n:], "\n")
	return slot
}

// pipeClosedHint turns a bare write failure into something a caller can act on.
//
// A pipe channel's process can exit while the session and the channel still report
// `running` — the status is updated when the exit is reaped, and the write can lose
// that race. What reaches the caller then is the OS error on a closed stdin, `EOF`
// or `process has exited, cannot send input`, which says nothing about the two facts
// that matter: this channel was a one-shot program, and its output is still there.
//
// The hint is only added for a pipe channel: on a pty the same error means something
// else (the shell died), and pointing at `read` would be wrong.
func pipeClosedHint(handle string, f *toolFailure, s *Server) *toolFailure {
	if f == nil {
		return f
	}
	msg := strings.TrimSpace(f.msg)
	closed := msg == "EOF" || strings.Contains(msg, "cannot send input") ||
		strings.Contains(msg, "file already closed") || strings.Contains(msg, "broken pipe")
	if !closed {
		return f
	}
	// The channel may already be refused by requireShell (its process exited and the
	// status caught up), which is the OTHER way this error arrives. Both roads lead to
	// the same advice, so a bad resolution is checked for the mode instead of
	// returning early: a pty whose shell died must NOT be told to `read` its exit
	// code, because a pty has no exit code to read.
	cs, bad := s.requireShell(handle)
	if cs == nil {
		return f
	}
	if bad != nil {
		if cs.Info().Mode != api.ModePipe {
			return f
		}
	} else if cs.Info().Mode != api.ModePipe {
		return f
	}
	// 409, NOT 500. The channel's process is over; nothing about the server broke and
	// the caller's own line was fine. A 5xx here invites a retry of a write that can
	// never land, and hides a finished channel behind "the server failed".
	return &toolFailure{
		code: CodeSessionNotRunning,
		msg: msg + "\nThis is a PIPE channel: its process has finished, so there is no stdin left to write to " +
			"and `run`/`input` cannot drive it. Its output is still readable — `read " + handle + "` returns it, " +
			"including the exit code. For an interactive session use `open <session>` instead (a pty).",
	}
}

// slotFromFailure renders a handler failure for a directive.
func (s *Server) slotFromFailure(ctx context.Context, stmt directive.Statement, f *toolFailure) envelope.Slot {
	slot := failureSlot(f)
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	slot.Headers["Path"] = stmt.Path
	return slot
}

// addUsage adds the directive's usage line to one slot when learn_mode asked for
// it.
//
// The de-duplication lives on the REPLY, not on the server: "the same directive
// once" is a fact about one answer, so two calls in flight must not be able to see
// each other's bookkeeping. That is why this takes the reply as an argument instead
// of reading a field off the Server — a shared field made every Term call race with
// every other, which is a bug this signature removes rather than locks around.
func (s *Server) addUsage(t *termReply, stmt directive.Statement, headers map[string]string) {
	if t == nil {
		return
	}
	t.withUsage(stmt, headers)
}

type reviewPendingView struct {
	Message    string
	ApprovalID string
	SessionID  string
	ExpiresAt  int64
}

func (v *reviewPendingView) slot(path string) envelope.Slot {
	headers := map[string]string{"Path": path}
	if v.ApprovalID != "" {
		headers["Approval-ID"] = v.ApprovalID
	}
	if v.SessionID != "" {
		headers["Session-ID"] = v.SessionID
	}
	if v.ExpiresAt > 0 {
		headers["Expires-At"] = strconv.FormatInt(v.ExpiresAt, 10)
	}
	return envelope.Slot{Code: 202, Headers: headers, Body: v.Message}
}

// reviewPendingFrom extracts the review-pending message and correlation fields
// from a handler reply, or nil when the call ran.
func reviewPendingFrom(res *mcpgo.CallToolResult) *reviewPendingView {
	doc, ok := toolResultMap(res)
	if !ok {
		return nil
	}
	if pending, _ := doc["review_pending"].(bool); !pending {
		return nil
	}
	view := &reviewPendingView{
		Message:    getString(doc, "message", "Submitted for review."),
		ApprovalID: getString(doc, "approval_id", ""),
		SessionID:  getString(doc, "session_id", ""),
	}
	if raw, ok := doc["expires_at"].(float64); ok && raw > 0 {
		view.ExpiresAt = int64(raw)
	}
	return view
}

// toolResultMap parses a handler reply's JSON body.
func toolResultMap(res *mcpgo.CallToolResult) (map[string]any, bool) {
	if res == nil || len(res.Content) == 0 {
		return nil, false
	}
	text := toolResultText(res)
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, false
	}
	return m, true
}

// failureFromResult turns a handler's error result into a toolFailure.
// failureFromResult translates a failed tool result into envelope terms.
//
// It goes through extractErrorInfo rather than decoding Content directly, because
// the error payload is a JSON envelope whose `error` field is what a caller
// reads: decoding the raw content would return the envelope as if it were text,
// and — worse — the dedicated error_code would be lost, turning every refusal
// into a generic operation_failed (a 500) instead of the 403 it is.
func failureFromResult(res *mcpgo.CallToolResult) *toolFailure {
	if res == nil {
		return &toolFailure{code: CodeOperationFailed, msg: "the operation failed without a reason"}
	}
	code, msg := extractErrorInfo(res)
	if code == "" {
		code = CodeOperationFailed
	}
	if msg == "" {
		msg = toolResultText(res)
	}
	return &toolFailure{code: code, msg: msg}
}
