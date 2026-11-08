package mcp

import (
	"context"
	"net/http"

	"github.com/open-mcp-ai/termcp/term/envelope"
	"github.com/open-mcp-ai/termcp/term/server"
)

// The REST entrance to the command space.
//
// `POST /api/term` is the same protocol the tool serves, over HTTP: the body is
// the tool's argument object ({"cmd": "ps", "bytes_limit": 0}), the reply is the
// same envelope, and the execution is literally the same call. Nothing is
// re-implemented for this surface, because there is nothing to re-implement —
// the command space IS the protocol, and which transport carries it does not
// change what a directive does.
//
// It lives in this package rather than internal/webui for the same reason: the
// namespace claim (withCallerNamespace) and the admission path (checkAdmission,
// reached through every directive's callTool) are here, and a second copy on the
// other side of an import cycle is a second place to get admission wrong. The
// cycle is real (internal/webui must not import internal/mcp — see
// Handler.MCPConnections), and the handler that needs mcp's internals is the one
// that belongs in mcp.
//
// Why an HTTP entrance at all: a caller that speaks MCP gets this for free, but a
// script, a skill and a CLI need a shape they can already send. one POST with one
// JSON object is that shape — the alternative was eight endpoints whose output
// cursor, stream filter and window semantics are all one directive's job here.
//
// The status code answers for the REQUEST; a failed command is a 2xx with a
// failing slot, exactly as `term` reports it. That split is what lets a batch
// reply carry a failure without the caller losing the slots that succeeded.

// TermHandler serves POST /api/term.
//
// The body is the tool's argument object. A malformed body is a 400 with the
// same envelope shape as any other reply, so a caller reads one format whether it
// mistyped JSON or a directive.
func (s *Server) TermHandler() http.Handler {
	// THE ENTRANCE ITSELF IS term/server's (#273). What stays here is the part that
	// is this project's rather than the protocol's: the two context decorations,
	// which carry the caller's isolation namespace and the fact that this write
	// arrived over REST rather than through an agent's tool call. An embedder has
	// neither model, which is why they are a hook there and a policy here.
	return server.New(server.Options{
		// Params, not Term: this server reads the window parameters out of the same
		// object, so it owns the whole map. The empty-calls-guide rule still applies
		// — s.Term applies it — but it applies because this server implements it,
		// which is what the doc comment on Params warns about.
		Params: server.HandlerFunc(func(ctx context.Context, args map[string]any) envelope.Reply {
			return s.Term(ctx, "", args)
		}),
		RequestContext: func(ctx context.Context, r *http.Request) context.Context {
			// Skipping the namespace would hand a caller that claimed one the shared
			// view — the one mistake that widens an isolation axis instead of refusing
			// it.
			return withCallerSource(withRequestContext(ctx, r), "rest")
		},
	})
}

// replyStatus, writeEnvelope and escapeHeader moved to term/server with the
// entrance they render for. They were never about this server: a status code
// answers for a request, and an envelope is a wire format.
