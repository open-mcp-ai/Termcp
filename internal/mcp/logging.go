package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

type toolHandler = func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)

func withLogging(name string, h toolHandler, s *Server) toolHandler {
	return func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
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
						append([]any{"tool", name}, append(restrictionAttrs(request), principalAttrs(ctx)...)...)...)
				}
				return bad, nil
			}
		}

		start := time.Now()
		baseAttrs := []any{"tool", name}
		baseAttrs = append(baseAttrs, principalAttrs(ctx)...)
		baseAttrs = appendSafeArgs(baseAttrs, request)

		slog.DebugContext(ctx, "tool call start", baseAttrs...)
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
				if preview := extractOutputPreview(result); preview != "" {
					exitAttrs = append(exitAttrs, "output_preview", truncate(preview, 200))
				}
			}
			slog.DebugContext(ctx, "tool call end", exitAttrs...)
		}
		return result, err
	}
}

// principalAttrs names the credential behind the call — the scoped token's id
// (never the token itself) and its kind — so the audit log can answer "which
// harness did that" per scoped-tokens.md §4.1.
func principalAttrs(ctx context.Context) []any {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p == nil {
		return nil
	}
	return []any{"principal_id", p.ID, "principal_kind", string(p.Kind)}
}

// restrictionAttrs names the target a refusal applied to, so the log line can
// answer "which session refused what" without replaying the whole call.
func restrictionAttrs(request mcpgo.CallToolRequest) []any {
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

func appendSafeArgs(attrs []any, request mcpgo.CallToolRequest) []any {
	args := request.GetArguments()
	if args == nil {
		return attrs
	}
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
	if v, ok := args["command"].(string); ok && v != "" {
		attrs = append(attrs, "command", v)
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
	if v, ok := args["text"].(string); ok && v != "" {
		attrs = append(attrs, "text", truncate(v, 200))
	}
	return attrs
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractOutputPreview(result *mcpgo.CallToolResult) string {
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return ""
}

// extractErrorInfo decodes the dedicated error_code field from a failed tool
// result. Plain-text errors (returned by mcp-go internals or third-party
// handlers) yield an empty code and the raw text as the message.
func extractErrorInfo(result *mcpgo.CallToolResult) (code, message string) {
	preview := extractOutputPreview(result)
	if preview == "" {
		return "", ""
	}
	var payload struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(preview), &payload); err == nil && payload.ErrorCode != "" {
		return payload.ErrorCode, payload.Error
	}
	return "", preview
}
