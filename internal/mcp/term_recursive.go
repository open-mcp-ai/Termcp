package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// Recursive routing (#274).
//
// An extension whose template declares `termcp_tool` is a downstream that speaks
// TERM/1, so `run <ext> <words…>` sends the words to it as one command line
// instead of typing them at a shell. The point is one round trip for a chain:
// A calls B calls C, and each layer parses only its own line.
//
// What this deliberately is NOT:
//
//   - not a guess. Only a declared-and-verified downstream is routed to; see
//     checkTermcpTool. An undeclared peer is called with `call`, as before.
//   - not a second protocol. The downstream answers a TERM/1 envelope because
//     that is what its tool returns; if it answers something else, the caller
//     gets that text and a note, rather than a half-parsed reply.
//   - not unlimited. Hops are counted and capped, and the cap is a declared
//     fact carried in a header rather than a timeout: a loop must fail at a
//     place that can name itself.

// maxTermcpHops bounds how deep a chain may go. Eight is enough for the shape
// this exists for (a gateway in front of a few specialised servers) and small
// enough that a loop reports itself in well under a second.
const maxTermcpHops = 8

// hopsHeader carries the depth on the wire. A downstream that speaks TERM/1
// reads it, adds one, and refuses past the cap — so a loop terminates at a
// layer that can say which names it went through, instead of at whichever
// timeout fired first.
const hopsHeader = "X-Termcp-Hops"

// termcpRoute decides whether a `run` statement is a recursive call and, if so,
// performs it. The bool reports whether this function handled the statement.
//
// The target must name an extension instance whose declaration verified; every
// other target — a session, a channel, a borrowed host — falls through to the
// ordinary path untouched, which is what keeps this from changing existing
// behaviour.
func (s *Server) termcpRoute(ctx context.Context, stmt directive.Statement, win termArgs) ([]envelope.Slot, bool) {
	if s.extensions == nil {
		return nil, false
	}
	name := strings.TrimSpace(stmt.Target)
	if name == "" || strings.Contains(name, ":") {
		return nil, false
	}
	ext, err := s.extensions.Store().Load(name)
	if err != nil || ext == nil || strings.TrimSpace(ext.URL) == "" || ext.TermcpTool == "" {
		return nil, false
	}
	// The session the peer is mounted under. `run <ext>` has no session of its
	// own to name, so this uses the extension's own instance — the same one
	// `open ext` mounts.
	sessionID := s.extSessionID(ctx, name)
	if sessionID == "" {
		return nil, false
	}
	check := s.extTermcpOf(sessionID, name)
	if !check.OK() {
		// Declared but unusable: say so here rather than routing into a peer that
		// will not understand the line. The declaration's refusal already explains
		// itself in `open ext`; this repeats the one sentence at the point of use.
		slot := badRequest(stmt, "`"+name+"` declared termcp_tool = "+check.tool+
			" but it is not usable: "+check.reason+
			"\nNothing was sent. Use `call` to reach its tools directly.")
		return []envelope.Slot{slot}, true
	}
	return []envelope.Slot{s.termcpForward(ctx, stmt, win, name, check.tool)}, true
}

// termcpForward sends one command line to a downstream and adapts its answer.
func (s *Server) termcpForward(ctx context.Context, stmt directive.Statement, win termArgs, name, tool string) envelope.Slot {
	line := strings.TrimSpace(stmt.Payload)
	if line == "" {
		return badRequest(stmt, "`run "+name+"` needs a command line to forward: run "+name+" <command…>")
	}
	hops := hopsOf(ctx)
	if hops >= maxTermcpHops {
		return envelope.Slot{
			Code: 400,
			Headers: map[string]string{
				"Path":       stmt.Path,
				"Error-Code": "termcp_hop_limit",
			},
			Body: fmt.Sprintf("stopped after %d hops at %q: this chain is too deep or loops.\n"+
				"The names it went through are in the %s header of each layer's reply; "+
				"break the loop or call the downstream directly with `call`.", hops, name, hopsHeader),
		}
	}

	h, slot := s.termcpPeer(ctx, name)
	if h == nil {
		slot.Headers["Path"] = stmt.Path
		return slot
	}

	frame := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": map[string]any{"cmd": line},
		},
	}
	callCtx, stop := context.WithTimeout(withHops(ctx, hops+1), peerCallWindow(win))
	defer stop()
	raw, err := h.ch.Call(callCtx, frame)
	if err != nil {
		return envelope.Slot{
			Code: 504,
			Headers: map[string]string{
				"Path":       stmt.Path,
				"Error-Code": CodeConnectionFailed,
			},
			Body: "the downstream did not answer: " + err.Error(),
		}
	}

	text := downstreamText(raw)
	out := parseTermcpEnvelope(text)
	// ALWAYS DECLARE THE DEPTH, on success and on refusal alike: the caller's
	// next decision (retry, stop, call directly) depends on knowing how far this
	// already went.
	if out.Headers == nil {
		out.Headers = map[string]string{}
	}
	out.Headers["Path"] = stmt.Path
	out.Headers[hopsHeader] = strconv.Itoa(hops + 1)
	if out.Code == 0 {
		// The downstream answered with something that is not a TERM/1 envelope.
		// Forward the text unchanged and say what happened, rather than pretending
		// to have parsed it: a gateway in front of a non-conforming server must not
		// turn that server's words into its own.
		out = envelope.Slot{
			Code: 200,
			Headers: map[string]string{
				"Path":       stmt.Path,
				hopsHeader:   strconv.Itoa(hops + 1),
				"Downstream": name,
			},
			Body: text,
		}
		if strings.TrimSpace(text) == "" {
			out.Body = "the downstream answered with nothing"
		}
	}
	return out
}

// termcpPeer mounts (or finds) the peer for an extension instance.
func (s *Server) termcpPeer(ctx context.Context, name string) (*jrpcHandle, envelope.Slot) {
	ext, err := s.extensions.Store().Load(name)
	if err != nil || ext == nil {
		return nil, envelope.Slot{Code: 404, Headers: map[string]string{"Error-Code": CodeRuleNotFound},
			Body: "no extension named " + name}
	}
	sessionID := s.extSessionID(ctx, name)
	h, _, slot, ok := s.mountExtPeer(ctx, ext, sessionID, name)
	if !ok || slot.Code >= 400 {
		return nil, slot
	}
	return h, envelope.Slot{}
}

// extSessionID is the instance a mounted peer belongs to: the session the
// reconciler keeps for this extension, which is what `open ext` mounts under.
//
// It is read from the extension document rather than from the template, because
// a template describes what to start and the document describes what is running.
func (s *Server) extSessionID(ctx context.Context, name string) string {
	if s.extensions == nil {
		return ""
	}
	res, fail := s.callTool(ctx, "extension", map[string]any{"action": "get", "name": name})
	if fail != nil {
		return ""
	}
	doc, _ := toolResultMap(res)
	id, _ := doc["session_id"].(string)
	return strings.TrimSpace(id)
}

// toolResultText pulls the text out of a tools/call result. A downstream that
// speaks TERM/1 answers with one text content item holding the envelope.
func downstreamText(raw string) string {
	var reply struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return raw
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return reply.Error.Message
	}
	var b strings.Builder
	for _, c := range reply.Result.Content {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// parseTermcpEnvelope reads a downstream's TERM/1 reply back into a Slot, so a
// gateway can forward it with its own Path and hop count rather than as opaque
// text. Code 0 means "this was not an envelope".
//
// The parse is deliberately shallow: a status line, headers, a blank line, a
// body. Anything else is not TERM/1, and pretending to understand it would be
// the exact failure this protocol exists to avoid.
func parseTermcpEnvelope(text string) envelope.Slot {
	text = strings.TrimLeft(text, "\r\n")
	line, rest, ok := strings.Cut(text, "\n")
	if !ok {
		return envelope.Slot{}
	}
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "TERM/1 ") {
		return envelope.Slot{}
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return envelope.Slot{}
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 100 || code > 599 {
		return envelope.Slot{}
	}
	headers := map[string]string{}
	body := rest
	for {
		h, tail, found := strings.Cut(body, "\n")
		if !found {
			// No blank line: the whole rest was headers, so there is no body.
			return envelope.Slot{Code: code, Headers: headers}
		}
		h = strings.TrimRight(h, "\r")
		if h == "" {
			body = tail
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok {
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		body = tail
	}
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// Hop counting rides on the context, so no handler signature changes: the count
// is a fact about the call, not an argument every layer must thread through.
type hopsKey struct{}

func withHops(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, hopsKey{}, n)
}

func hopsOf(ctx context.Context) int {
	if v, ok := ctx.Value(hopsKey{}).(int); ok {
		return v
	}
	return 0
}
