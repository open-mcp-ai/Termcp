package mcp

import (
	"context"
	"log/slog"
	"runtime/debug"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// withPanicRecovery turns a panicking tool handler into a failed tool result.
//
// A panic in a tool handler is a bug in termcp, and without this it is not a
// failed tool call - it is the end of the process. The tool handler runs on a
// goroutine that mcp-go owns (the streamable-HTTP and SSE servers dispatch
// requests from their own goroutine), so a panic there is not contained by any
// HTTP handler's recover and no deferred cleanup above it runs: every session,
// every browser tab and the daemon's own HTTP listener die with it. That blast
// radius is the reason this exists; it is not a substitute for fixing the bug.
//
// It is a middleware rather than mcpserver.WithRecovery() because the two disagree
// about what a client should see. WithRecovery returns a plain Go error, which
// mcp-go converts into a JSON-RPC protocol error (code -32603) carrying the panic
// value as its message. That bypasses the error_code contract every other failure
// in this server follows (see toolError), so a client that branches on error_code
// - which is what the field is for - has nothing to branch on, and the panic value
// itself is usually a runtime message about internals.
//
// So the two audiences get different things, which is the whole point:
//
//	the client   a normal failed tool result, {"error_code":"internal_error",
//	             "error":...}, indistinguishable in shape from any other failure.
//	             The message says the failure is termcp's own and not caused by the
//	             request, because the alternative - telling an agent "invalid
//	             argument" for a bug on our side - sends it off fixing its input.
//	             The panic value is deliberately NOT included: it names internal
//	             files and line numbers.
//	the operator the panic value and a full stack trace, at Error level, in the
//	             server log, which is the only place it can be acted on. Without
//	             this line a recovered panic would be invisible: swallowing it
//	             silently would trade a loud crash for a quiet wrong answer.
//
// The tool name is included in both, since a panic report without it cannot be
// located.
func withPanicRecovery() mcpserver.ServerOption {
	return mcpserver.WithToolHandlerMiddleware(func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, request mcpgo.CallToolRequest) (result *mcpgo.CallToolResult, err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				name := request.Params.Name
				// debug.Stack is taken here, in the deferred call, so the stack
				// still holds the panicking frames.
				slog.ErrorContext(ctx, "panic recovered in tool handler",
					"tool", name,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				// err stays nil: this is a tool-level failure, reported the same
				// way as every other one, not a protocol-level error.
				result = toolError(CodeInternalError,
					"termcp hit an internal error handling %s; the request was not completed. "+
						"This is a bug in termcp, not a problem with the arguments - "+
						"retrying the same call is unlikely to help. Stack trace is in the server log.", name)
				err = nil
			}()
			return next(ctx, request)
		}
	})
}
