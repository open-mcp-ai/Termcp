package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/mcp"
)

type toolHandler = func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error)

func withLogging(name string, h toolHandler, s *Server) toolHandler {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// The admission decision runs before anything else and before the handler
		// is even entered: a call that a session's capability list — or another
		// caller's namespace — refuses must not reach the tool at all, and one that
		// review mode holds must be queued instead of run. Answering all of it here
		// rather than in each handler is what makes a reviewable operation covered
		// by its capability row instead of by someone remembering to guard the
		// handler: the file and forward handlers no longer call the gate, and the
		// shell handlers no longer ask RequiresApproval.
		//
		// A nil server means there is nothing to admit against (a logging-only test
		// harness), so the wrapper passes the call through.
		if s != nil {
			if bad, held := s.checkAdmission(ctx, name, request); bad != nil || held {
				if bad != nil {
					slog.DebugContext(ctx, "tool refused by session restriction",
						append([]any{"tool", name}, restrictionAttrs(request)...)...)
				}
				return bad, nil
			}
		}

		start := time.Now()
		baseAttrs := []any{"tool", name}
		baseAttrs = appendSafeArgsFrom(ctx, baseAttrs, request)

		slog.InfoContext(ctx, "tool call start", baseAttrs...)
		result, err := h(ctx, request)
		durMs := time.Since(start).Milliseconds()

		exitAttrs := append([]any{}, baseAttrs...)
		exitAttrs = append(exitAttrs, "duration_ms", durMs)

		if err != nil {
			exitAttrs = append(exitAttrs, "err", err)
			slog.ErrorContext(ctx, "tool call end", exitAttrs...)
		} else {
			if result != nil && result.IsError {
				// Tool-level failure (returned as an IsError result, err == nil):
				// log at Warn so it is visible at the default log level instead of
				// vanishing into debug-only output.
				exitAttrs = append(exitAttrs, "is_error", true)
				code, msg := extractErrorInfo(result)
				if code != "" {
					exitAttrs = append(exitAttrs, "error_code", code)
				}
				if msg != "" {
					exitAttrs = append(exitAttrs, "error", truncate(msg, 300))
				}
				slog.WarnContext(ctx, "tool call end", exitAttrs...)
				return result, err
			}
			if result != nil {
				// A CALL THAT DECLARED SECRETS DOES NOT LOG ITS REPLY. The reply echoes the
				// line that ran (`Path:` is the substituted command line), so for a call whose
				// values were declared secret the reply text is one of the carriers — and it is
				// the last one left once the args and the inner calls are shapes. The trade is
				// stated rather than hidden: you asked for this value not to be written down, so
				// the reply's text is not written down either. Nothing here inspects the reply.
				if declaredSecrets(request) {
					exitAttrs = append(exitAttrs, "reply", fmt.Sprintf("%d B (not logged: the call declared secrets)", len(toolResultText(result))))
				} else if preview := extractOutputPreview(result); preview != "" {
					exitAttrs = append(exitAttrs, "output_preview", truncate(preview, 200))
				}
			}
			// INFO, not Debug. A successful call is the unit a support report is
			// written in ("this call returned X"), and at Debug level the ones that
			// matter are absent by default — a report of a broken reply arrived with a
			// log whose last line was an unrelated 400, because everything that had
			// worked was invisible. One line per call is the cost; being able to match
			// a complaint to a command is what it buys.
			slog.InfoContext(ctx, "tool call end", exitAttrs...)
		}
		return result, err
	}
}

// declaredSecrets reports whether this call carried a non-empty secrets table.
//
// It reads the CALLER's arguments, so it is true only where the caller said the values
// are secret. Nothing infers it from the shape or content of a value.
func declaredSecrets(request mcp.CallToolRequest) bool {
	obj, ok := request.GetArguments()["secrets"].(map[string]any)
	return ok && len(obj) > 0
}

// restrictionAttrs names the target a refusal applied to, so the log line can
// answer "which session refused what" without replaying the whole call.
func restrictionAttrs(request mcp.CallToolRequest) []any {
	args := request.GetArguments()
	attrs := []any{}
	if id := getString(args, "session_id", ""); id != "" {
		attrs = append(attrs, "session_id", id)
	}
	if id := getString(args, "shell_id", ""); id != "" {
		attrs = append(attrs, "shell_id", id)
	}
	return attrs
}

func appendSafeArgs(attrs []any, request mcp.CallToolRequest) []any {
	return appendSafeArgsFrom(context.Background(), attrs, request)
}

// innerCallKey marks the tool calls the directive layer makes on the caller's behalf.
type innerCallKey struct{}

// withInnerCall marks ctx as the inside of a `term` call, whose payloads are already
// recorded — in full — by the call that started it.
func withInnerCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, innerCallKey{}, true)
}

// isInnerCall reports whether this is a call a directive made, not one a caller made.
func isInnerCall(ctx context.Context) bool {
	v, _ := ctx.Value(innerCallKey{}).(bool)
	return v
}

// appendSafeArgsFrom is appendSafeArgs with the context that says whose call this is.
//
// AN INNER CALL LOGS SHAPES, NOT VALUES. `run` reaches the terminal by calling the
// shell_input handler, so the expanded command line — the one place a declared secret
// actually appears — was written to the log by the INNER call, under `text`, after the
// outer call had carefully recorded only the secret's name. Suppressing it here is not
// a second redactor: the value is a copy of something the outer line already holds in
// full, so nothing is lost from the record, and there is no pattern to miss.
func appendSafeArgsFrom(ctx context.Context, attrs []any, request mcp.CallToolRequest) []any {
	args := request.GetArguments()
	if args == nil {
		return attrs
	}
	if isInnerCall(ctx) {
		return appendInnerShapes(attrs, args)
	}
	return appendCallerArgs(attrs, args)
}

// appendInnerShapes records what an inner call carried without recording it.
//
// The ids and the numeric parameters are kept: they are what a support report matches
// on, and none of them is caller text.
func appendInnerShapes(attrs []any, args map[string]any) []any {
	for _, name := range []string{"text", "command", "cmd"} {
		if v, ok := args[name].(string); ok && v != "" {
			attrs = append(attrs, name, fmt.Sprintf("%d B (not logged)", len(v)))
		}
	}
	if v, ok := args["args"].([]any); ok && len(v) > 0 {
		total := 0
		for _, item := range v {
			if s, isString := item.(string); isString {
				total += len(s)
			}
		}
		attrs = append(attrs, "args", fmt.Sprintf("%d items, %d B (not logged)", len(v), total))
	}
	if v, ok := args["vars"].(map[string]any); ok && len(v) > 0 {
		attrs = append(attrs, "vars", fmt.Sprintf("%d names (not logged)", len(v)))
	}
	return appendNonTextArgs(attrs, args)
}

// appendCallerArgs records a caller's own call: what it asked for, verbatim.
func appendCallerArgs(attrs []any, args map[string]any) []any {
	// CMD FIRST, because it is the only attribute that says what was ASKED.
	//
	// The log carried the resolved ids and the window parameters but never the
	// command line, so a support report of "this call returned [object Object]" could
	// not be matched to anything: the log showed a session and a handle and no way to
	// tell which directive produced them. Truncated like every other value — a
	// payload can be a whole heredoc, and the log is not a transcript.
	if v, ok := args["cmd"].(string); ok && v != "" {
		attrs = append(attrs, "cmd", truncate(v, 200))
	}
	if v, ok := args["args"].([]any); ok && len(v) > 0 {
		if len(v) <= 10 {
			b, _ := json.Marshal(v)
			attrs = append(attrs, "args", string(b))
		} else {
			preview := make([]any, 10)
			copy(preview, v[:10])
			b, _ := json.Marshal(preview)
			attrs = append(attrs, "args", fmt.Sprintf("%s... (%d items total)", string(b), len(v)))
		}
	}
	// VARS IS LOGGED VERBATIM, and it has to be: the log is the record of what was
	// ASKED, and a value that reached the shell is part of that. Leaving it out (which is
	// what this function did) made the record lie twice — the call looked like it passed
	// no named values at all, and the one channel that carried data the command line
	// cannot hold left no trace anywhere.
	attrs = appendNamed(attrs, args, "vars")
	// SECRETS CONTRIBUTE ONLY THEIR NAMES. Same substitution, different record: a caller
	// that does not want a value written down puts it in this table instead of vars, and
	// the log then proves the value was supplied without reproducing it. Nothing here
	// inspects the value — matching `-p` while missing `--password` would report a safety
	// this code does not have. The names are sorted so two calls that differed only in
	// key order produce the same line.
	attrs = appendSecretNames(attrs, args)
	if v, ok := args["command"].(string); ok && v != "" {
		attrs = append(attrs, "command", v)
	}
	if v, ok := args["text"].(string); ok && v != "" {
		attrs = append(attrs, "text", truncate(v, 200))
	}
	return appendNonTextArgs(attrs, args)
}

// appendNonTextArgs records the ids and numbers, which are the same on both roads: a
// session id, a channel id, a key name, a window size. None of them is caller text, so
// an inner call keeps them and a support report still matches on them.
func appendNonTextArgs(attrs []any, args map[string]any) []any {
	if v, ok := args["session_id"].(string); ok && v != "" {
		attrs = append(attrs, "session_id", v)
	}
	if v, ok := args["shell_id"].(string); ok && v != "" {
		attrs = append(attrs, "shell_id", v)
	}
	if v, ok := args["key"].(string); ok && v != "" {
		attrs = append(attrs, "key", v)
	}
	if v, ok := args["reader_id"].(float64); ok {
		attrs = append(attrs, "reader_id", int64(v))
	}
	if v, ok := args["mode"].(string); ok && v != "" {
		attrs = append(attrs, "mode", v)
	}
	if v, ok := args["rows"].(float64); ok {
		attrs = append(attrs, "rows", int64(v))
	}
	if v, ok := args["cols"].(float64); ok {
		attrs = append(attrs, "cols", int64(v))
	}
	if v, ok := args["timeout"].(float64); ok {
		attrs = append(attrs, "timeout", v)
	}
	if v, ok := args["repeat"].(float64); ok {
		attrs = append(attrs, "repeat", int64(v))
	}
	if v, ok := args["force"].(bool); ok {
		attrs = append(attrs, "force", v)
	}
	if v, ok := args["grace_period"].(float64); ok {
		attrs = append(attrs, "grace_period", int64(v))
	}
	return attrs
}

// appendNamed logs a named-value dictionary verbatim, in a stable key order.
//
// It is written once for vars because a second copy for any other table would be a
// second place for the truncation limit and the key order to differ.
func appendNamed(attrs []any, args map[string]any, name string) []any {
	obj, ok := args[name].(map[string]any)
	if !ok || len(obj) == 0 {
		return attrs
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, _ := obj[k].(string)
		parts = append(parts, k+"="+truncate(v, 200))
	}
	return append(attrs, name, strings.Join(parts, " "))
}

// appendSecretNames records WHICH secrets were passed, never what they were.
//
// This is the one place the two tables diverge, so it is the one place that has to be
// right: a value logged here cannot be un-logged. Only the count and the names travel,
// which is enough to answer "did this call carry a password" without writing it down.
func appendSecretNames(attrs []any, args map[string]any) []any {
	obj, ok := args["secrets"].(map[string]any)
	if !ok || len(obj) == 0 {
		return attrs
	}
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names)
	return append(attrs, "secrets", fmt.Sprintf("%s (values not logged)", strings.Join(names, " ")))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractOutputPreview(result *mcp.CallToolResult) string {
	for _, c := range result.Content {
		if c.Text != "" {
			return c.Text
		}
	}
	return ""
}

// extractErrorInfo decodes the dedicated error_code field from a failed tool
// result. Plain-text errors (returned by the protocol layer or third-party
// handlers) yield an empty code and the raw text as the message.
//
// THE FAILING SLOT, NOT THE FIRST LINE. A reply is several TERM/1 sections, one
// per statement, and `isError` is true when ANY of them failed. Reading the preview
// from the top therefore logged the first statement's status as the error — a call
// whose second statement failed was recorded as "is_error=true error=TERM/1 200 OK",
// which points a reader at the statement that worked. Every failed call would then
// have to be re-diagnosed from scratch.
func extractErrorInfo(result *mcp.CallToolResult) (code, message string) {
	preview := extractOutputPreview(result)
	if preview == "" {
		return "", ""
	}
	failed := failingSection(preview)
	if failed == "" {
		return "", preview
	}
	var payload struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(failed), &payload); err == nil && payload.ErrorCode != "" {
		return payload.ErrorCode, payload.Error
	}
	return "", failed
}

// failingSection returns the first TERM/1 section whose status is 4xx or 5xx, or ""
// when every section succeeded.
//
// Sections are split on the "TERM/1 " marker rather than on blank lines: a body can
// contain blank lines (it is terminal output), and the marker is the one thing the
// envelope guarantees.
func failingSection(preview string) string {
	const marker = "TERM/1 "
	sections := strings.Split(preview, marker)
	if len(sections) == 1 {
		// Not an envelope: a handler's own JSON error, which is a single result and
		// therefore already the failure this function is looking for. Returning ""
		// here made every such error fall back to operation_failed and lose its code
		// — `close nosuchsession` answered 500 with session_not_found in the body.
		return preview
	}
	for _, sec := range sections[1:] { // sections[0] precedes the first marker
		// The status follows the marker: "200 OK", "404 Not Found", …
		if len(sec) < 3 {
			continue
		}
		if sec[0] < '4' {
			continue
		}
		return marker + sec
	}
	return ""
}
