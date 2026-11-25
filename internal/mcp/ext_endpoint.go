package mcp

// MCP over HTTP, PER EXTENSION (issues #234, #328).
//
// The instance's own tool surface is served at /stream. That surface has one tool
// (`term`) whose vocabulary is this project's directive language, so a client that does
// not know the language cannot reach anything an extension provides. /ext/{name}/stream
// closes that gap: the client points its MCP config at the URL and talks to the
// EXTENSION, whose tools, instructions and serverInfo come back unchanged.
//
// THIS FILE IS THE ADAPTER, NOT A TRANSPORT. It used to be both, and the second half was
// a hand-rolled copy of streamable HTTP that had drifted in three ways that matter: it
// answered 202 to a request (the spec forbids that — a client then waits on a stream the
// copy refused to serve), it answered 405 to GET (which is exactly where that client
// waits), and it invented no Mcp-Session-Id (so a later GET could not name itself). Now
// the only thing written here is the part that is specific to an extension — resolve the
// channel, serialise one line onto it, wait for the answer — and mcp.StreamableHTTPServer
// does sessions, streams, version headers, body limits and status codes.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

const (
	// extMCPStartWait is how long a request waits for an on-demand extension to come
	// up before answering. It is bounded because an HTTP client is holding a
	// connection: a target that takes minutes to answer must not turn into a request
	// that never answers.
	extMCPStartWait = 20 * time.Second
	// extMCPCallWindow is the reply window for one forwarded frame. It is what keeps a
	// dead peer from pinning a goroutine for ever.
	extMCPCallWindow = 60.0 // seconds, as termArgs.TimeoutSeconds
)

// ExtMCPHandler serves every extension's MCP server: one route, the name in the path.
func (s *Server) ExtMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.PathValue("name"))
		if name == "" {
			http.Error(w, "no extension named in the path: the route is /ext/{name}/stream", http.StatusNotFound)
			return
		}
		if s.extensions == nil {
			http.Error(w, "extensions are not available on this instance", http.StatusServiceUnavailable)
			return
		}
		// THE EXTENSION IS RESOLVED BEFORE THE TRANSPORT SEES THE REQUEST, so a bad name or
		// a template with nothing to talk to is a 404/409 that says which, rather than a
		// protocol-shaped answer from a transport that cannot know about extensions.
		if _, code, why := s.extChannel(name); code != 0 {
			http.Error(w, why, code)
			return
		}
		s.extTransport(name).ServeHTTP(w, r)
	})
}

// extTransport returns the one transport serving an extension, creating it on first use.
//
// ONE PER EXTENSION, NOT ONE PER REQUEST: the transport owns the sessions (and their GET
// streams), and a per-request transport would give every client a session nobody else
// could name.
func (s *Server) extTransport(name string) *mcp.StreamableHTTPServer {
	if v, ok := s.extTransports.Load(name); ok {
		return v.(*mcp.StreamableHTTPServer)
	}
	tr := mcp.NewStreamableHTTPServer(&extDispatcher{s: s, name: name})
	actual, _ := s.extTransports.LoadOrStore(name, tr)
	return actual.(*mcp.StreamableHTTPServer)
}

// extDispatcher is one extension's channel, seen through the transport's three calls.
type extDispatcher struct {
	s    *Server
	name string
}

// RegisterSession and UnregisterSession do nothing on purpose: an extension's channel is
// the server, not the client. Every session talks to the same process, and the session's
// only job here is to name the client's GET stream.
func (d *extDispatcher) RegisterSession(context.Context, mcp.Session)   {}
func (d *extDispatcher) UnregisterSession(context.Context, mcp.Session) {}

// Handle turns one inbound frame into one line on the channel, and the peer's answer
// back into a frame.
func (d *extDispatcher) Handle(ctx context.Context, raw []byte, sess mcp.Session) []byte {
	shell, code, why := d.s.extChannel(d.name)
	if shell == "" {
		return extErrorFrame(nil, extCodeForStatus(code), why)
	}
	frame, why := extFrameBody(string(raw))
	if why != "" {
		return extErrorFrame(nil, extCodeParse, why)
	}

	// ONE LINE AT A TIME. The text and the Enter that ends it are two writes, and two
	// requests interleaving there produce a line no reader can parse (#212). The lock is
	// released before the wait: holding it across the reply would serialise every client
	// behind the slowest peer.
	// A SLOT IN A CHANNEL TOKEN, not a mutex: acquiring it listens on ctx, and releasing
	// it is a send — so no return path can leave it held (AGENTS.md).
	tok := d.s.lineToken(shell)
	select {
	case <-tok:
	case <-ctx.Done():
		return extErrorFrame(nil, extCodePeer, "timed out waiting for a prior command line on this channel to finish submitting")
	}
	inErr := d.s.extWriteLine(ctx, shell, frame)
	tok <- struct{}{}
	if inErr != nil {
		return extErrorFrame(nil, extCodePeer, "the extension's channel refused the frame: "+inErr.Error())
	}

	id := frameID(frame)
	if id == "" {
		// A notification has no answer. Returning nil is the transport's signal for "no
		// body", which it turns into 202 — correct here, because nothing was requested.
		return nil
	}
	stmt := directive.Statement{Op: "mcp", Kind: "mcp", Target: shell}
	win := termArgs{TimeoutSet: true, TimeoutSeconds: extMCPCallWindow}
	slot := d.s.mcpAwaitReply(ctx, stmt, shell, id, win, nil)
	if body, ok := extSlotBody(slot); ok {
		return []byte(body)
	}
	return extErrorFrame(json.RawMessage(id), extCodePeer,
		extSlotReason(slot, id))
}

// extWriteLine puts one frame on the channel as one line.
func (s *Server) extWriteLine(ctx context.Context, shellID, frame string) error {
	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", shellID, "text", frame)); fail != nil {
		return fmt.Errorf("%s", fail.msg)
	}
	if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", shellID, "key", "enter")); fail != nil {
		return fmt.Errorf("%s", fail.msg)
	}
	return nil
}

// extChannel resolves the extension to the channel this endpoint talks to, running it if
// it is on demand. code is 0 when the channel is usable; otherwise it is the HTTP status
// to answer with and why says which shape is missing.
func (s *Server) extChannel(name string) (shellID string, code int, why string) {
	st, err := s.extensions.Acquire(name, extMCPStartWait)
	if err != nil {
		return "", http.StatusNotFound, "extension " + name + ": " + err.Error()
	}
	// WHICH CHANNEL ANSWERS /ext/{name}/stream (issue #331). A template may declare
	// several now; the one whose traffic this endpoint can carry is the http one.
	shell := extMCPChannelShell(st)
	if shell != "" {
		return shell, 0, ""
	}
	if len(st.Channels) > 0 {
		names := make([]string, 0, len(st.Channels))
		for _, ch := range st.Channels {
			names = append(names, ch.Name+" ("+ch.Kind+")")
		}
		return "", http.StatusConflict, "extension " + name + " has no http channel, so this endpoint has nothing to talk to: " +
			"its channels are " + strings.Join(names, ", ") + ". Add a channel with kind = \"http\" to give it one."
	}
	return "", http.StatusConflict, "extension " + name + " has no channel, so it has no MCP server to talk to: " +
		"its template declares no command. Add --command (or --url) to give it one."
}

// extMCPChannelShell picks the channel the /ext endpoint speaks to.
//
// AN HTTP CHANNEL WINS WHEN THERE IS ONE. Everything about this endpoint is the HTTP
// shape — one JSON-RPC frame per request, no session state of ours — and a template that
// declares an endpoint has already said which channel is meant. A template with one
// channel answers with that one, which is exactly what this endpoint did before channels
// existed.
func extMCPChannelShell(st extensions.Status) string {
	if len(st.Channels) == 0 {
		return st.ShellID
	}
	for _, ch := range st.Channels {
		if ch.Kind == extensions.ChannelHTTP && ch.ShellID != "" {
			return ch.ShellID
		}
	}
	return ""
}

// extFrameBody normalises a request body into one frame, refusing what cannot be
// forwarded.
//
// A BATCH IS REFUSED BY NAME. JSON-RPC batches were removed in 2025-06-18, so a client
// that sends one is speaking a version this endpoint does not; forwarding it would put a
// line on the peer's channel that the peer may parse as one message with a bogus shape.
func extFrameBody(raw string) (string, string) {
	frame := strings.TrimSpace(raw)
	if frame == "" {
		return "", "empty request body: POST one JSON-RPC message"
	}
	if strings.HasPrefix(frame, "[") {
		return "", "a JSON-RPC batch is not part of MCP 2025-06-18: post one message per request"
	}
	if !json.Valid([]byte(frame)) {
		return "", "the body is not JSON; this endpoint forwards one JSON-RPC message per request"
	}
	return frame, ""
}

// extSlotBody is the peer's frame, or false when the slot is not one.
//
// THE BODY MUST BE THE PEER'S BYTES. Every sentence this project appends for a human (a
// Hint header, a note after the text) would be a parse error for an MCP client, so a body
// that is not a JSON-RPC message is reported as a gateway failure instead of forwarded.
func extSlotBody(slot envelope.Slot) (string, bool) {
	if slot.Code >= 400 || slot.Code == http.StatusNoContent {
		return "", false
	}
	body := strings.TrimSpace(slot.Body)
	var probe struct {
		JSONRPC string `json:"jsonrpc"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil || probe.JSONRPC == "" {
		return "", false
	}
	return body, true
}

// extSlotReason turns a non-answer into the sentence the client reads.
func extSlotReason(slot envelope.Slot, id string) string {
	body := strings.TrimSpace(slot.Body)
	switch {
	case slot.Code == http.StatusNoContent:
		return fmt.Sprintf("the extension did not answer id %s within %s. It is alive — the frame was delivered — "+
			"but this window is all the endpoint will hold a request open for. A tool that needs longer belongs on the "+
			"directive surface (`call <peer>`), whose window the caller sets", id, time.Duration(extMCPCallWindow*float64(time.Second)))
	case body == "":
		return "the extension answered with an empty body, which is not a JSON-RPC message"
	default:
		return "the extension answered with something that is not a JSON-RPC message: " + truncateForReply(body)
	}
}

// JSON-RPC error codes this adapter produces. They are the protocol's own numbers, not
// HTTP ones: once the transport has a session, the answer is a frame.
const (
	extCodeParse = -32700 // parse error: the body was not JSON
	extCodePeer  = -32000 // server error: the peer did not answer, or answered with junk
)

// extErrorFrame renders one JSON-RPC error answer.
func extErrorFrame(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return out
}

// extCodeForStatus maps an HTTP status back to a JSON-RPC code, for the errors Handle
// reports after the transport has committed to a frame.
func extCodeForStatus(code int) int {
	switch code {
	case http.StatusNotFound, http.StatusConflict:
		return extCodePeer
	default:
		return extCodePeer
	}
}
