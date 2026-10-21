package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/open-mcp-ai/termcp/internal/envelope"
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := map[string]any{}
		if r.Body != nil {
			dec := json.NewDecoder(r.Body)
			// The window parameters are numbers; the default decoding into any
			// gives float64, which is what parseTermArgs reads. An empty body is
			// legal and means the same thing as an empty call: the guide.
			if err := dec.Decode(&args); err != nil && err.Error() != "EOF" {
				writeEnvelope(w, http.StatusBadRequest, envelope.Reply{Slots: []envelope.Slot{
					badRequestNoPath("request body must be a JSON object of term arguments: " + err.Error()),
				}})
				return
			}
		}
		// The caller's own namespace and entrance ride on the context: the first is
		// read off the same header the MCP transports read, the second tells the
		// review queue and the transcript that this write came from REST, not from
		// an agent's tool call. Skipping the namespace would hand a caller that
		// claimed one the shared view — the one mistake that widens an isolation
		// axis instead of refusing it.
		ctx := withCallerSource(withRequestContext(r.Context(), r), "rest")
		reply := s.Term(ctx, "", args)
		writeEnvelope(w, replyStatus(reply), reply)
	})
}

// replyStatus is the HTTP status of a reply: the REQUEST succeeded if any slot did.
//
// A single failing slot is a 200 with a 4xx inside it, which is what "the status
// code answers for the request" means — but a reply where every slot failed has
// nothing that succeeded, so it is reported as its first slot's code. That keeps
// `curl -f` and a naive HTTP client useful without making a batch's partial
// success look like a failure.
//
// It is named replyStatus because statusFor already answers a different question
// (a tool error CODE's HTTP status, for a handler failure inside a directive).
func replyStatus(reply envelope.Reply) int {
	if len(reply.Slots) == 0 {
		return http.StatusNoContent
	}
	for _, slot := range reply.Slots {
		if slot.Code < 400 {
			return http.StatusOK
		}
	}
	return reply.Slots[0].Code
}

// writeEnvelope serves one reply.
//
// The body is the tool's structured projection, SLOT FOR SLOT: one slot arrives
// as an object, several arrive wrapped in {"slots": [...]}. A bare array for the
// batch case would be the friendlier REST shape and is exactly what this entrance
// must not invent — a caller that learns one shape here and another from the tool
// has two descriptions of one protocol, and only one of them is true. The wrapper
// is also the honest one: a reply is a set of slots, not a list.
//
// The rendered form rides along as X-Termcp-Text, which keeps the shape a model
// reads available to a human debugging a curl call instead of making them decode
// JSON to read a status line. It is a convenience, not the contract: the body is,
// and a header cannot carry newlines, so the rendering is escaped.
func writeEnvelope(w http.ResponseWriter, status int, reply envelope.Reply) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if text := reply.Text(); text != "" {
		w.Header().Set("X-Termcp-Text", escapeHeader(text))
	}
	w.WriteHeader(status)
	structured := reply.Structured()
	var body any
	switch len(structured) {
	case 0:
		body = map[string]any{"slots": []any{}}
	case 1:
		body = structured[0]
	default:
		body = map[string]any{"slots": structured}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// escapeHeader renders a multi-line rendering as a single header value.
//
// Newlines become literal "\n" so the value survives the header grammar, which is
// the same reason the envelope renders them at all: the text is for a reader, and a
// reader that cannot see the line structure is looking at an unreadable run of
// header stamps.
//
// CARRIAGE RETURNS ARE ESCAPED, NOT DROPPED. The previous version fell through to
// `default` for '\r' and appended nothing, so the same field rendered two different
// strings depending on the projection: a Path holding a CR kept it in the JSON body
// and silently lost it in X-Termcp-Text. A caller comparing the two — or reading
// only the header — then had a path that does not exist, with nothing to indicate a
// byte had been removed. Escaping keeps the two projections byte-identical, which is
// the whole reason they are generated from one value.
func escapeHeader(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
