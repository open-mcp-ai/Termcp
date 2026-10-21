package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/jrpc"
)

// Three directives, one channel.
//
// `jrpc`, `mcp` and `cdp` are the same frame mover with three doors: the channel
// (internal/jrpc) knows only "write a frame, read until your id comes back, keep
// the rest", and each door adds the one thing its protocol needs on top of that.
//
//	jrpc  nothing. Any JSON-RPC peer: an LSP, a private RPC, a device.
//	mcp   the initialize handshake, run once, before the first real frame.
//	cdp   a browser. No handshake (DevTools answers one with "method not found"),
//	      and a URL that may be a plain http devtools endpoint to be resolved
//	      into the socket it advertises.
//
// Keeping them as three entries rather than one entry with a --protocol flag is
// the same argument `pipe`/`pty` makes: the difference is not a parameter, it is
// what the caller means, and a wrong guess is not recoverable later (an MCP peer
// sent a raw frame answers "not initialized" forever; a DevTools peer sent
// initialize answers "method not found" and stays usable only by ignoring it).
// The cost is three usage lines in `help`, which is the whole budget this
// surface spends on growth.
//
// `mcp` keeps its stdio form unchanged: a handle that is a SHELL is still a
// process to type at, and that path never touches this file's registry.

// jrpcChannels is the registry of opened frame channels, keyed by handle.
//
// ponytail: an in-memory map with no eviction. A channel is opened inside one
// conversation and closed with `close`; the leak would be one struct per opened
// URL. Add a reaper when a long-lived instance is observed accumulating them.
var (
	jrpcChannelsMu sync.Mutex
	jrpcChannels   = map[string]*jrpcHandle{}
)

// jrpcHandleSeq mints handles monotonically. len(map)+1 would reuse a handle
// after a close and silently point two names at one channel.
var jrpcHandleSeq atomic.Int64

// jrpcHandle is one opened channel plus the little state a door needs.
type jrpcHandle struct {
	ch   *jrpc.Channel
	kind string // jrpc | mcp | cdp

	// initMu guards init. It is not the channel's mutex: the channel is held for
	// a whole call, so a handshake under it would let a second caller begin
	// while the first is still inside the channel — which is exactly how
	// initialize ran once per concurrent caller instead of once per handle.
	initMu sync.Mutex
	// init reports that the MCP handshake has run. It is per handle, not per
	// channel: the channel does not know what MCP is, which is what lets `jrpc`
	// and `cdp` share it.
	init bool
}

// jrpcOpen connects a URL and hands back a handle.
func (s *Server) jrpcOpen(ctx context.Context, stmt directive.Statement, op, rawURL string) envelope.Slot {
	if rawURL == "" {
		return badRequest(stmt, op+" open needs a URL")
	}
	// The whitelist gate. A session whose template lists capabilities cannot reach
	// a door it did not name — and before the capability rows existed it could
	// reach every one of them, because the session-scoped guard resolves a
	// directive's TARGET and here the target is a URL that names no session. So
	// the check is made against the caller's own door instead.
	//
	// REVIEW is deliberately not run here: whether an admitted open is HELD for a
	// human is the event-hook layer's decision (issue #128), and duplicating that
	// decision at this layer is how the two would drift apart.
	if slot, refused := s.refuseDoor(ctx, stmt, op); refused {
		return slot
	}
	// `mcp open .../sse` used to be refused with a sentence about the legacy
	// transport. The streamable door speaks both shapes now (a POST answered by
	// JSON or by an event), so the refusal is gone rather than reworded.
	if op == "cdp" {
		resolved, err := devtoolsSocket(ctx, rawURL)
		if err != nil {
			return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed},
				Body: err.Error()}
		}
		rawURL = resolved
	}
	ch, err := jrpc.Open(ctx, rawURL)
	if err != nil {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed},
			Body: err.Error()}
	}
	handle := fmt.Sprintf("%s%d", op, jrpcHandleSeq.Add(1))
	jrpcChannelsMu.Lock()
	jrpcChannels[handle] = &jrpcHandle{ch: ch, kind: op}
	jrpcChannelsMu.Unlock()
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": handle},
		Body:    fmt.Sprintf("%s %s (%s, no shell channel consumed)\n", handle, rawURL, ch.Transport()),
	}
}

// refuseDoor applies a session's capability whitelist to one of the frame-channel
// doors.
//
// It answers a NARROWER question than refuseAction, and the difference is not an
// oversight: refuseAction resolves the session a statement names so it can read
// that session's restriction. A door names no session — its target is a URL — so
// the restriction that governs it is the one on the session the CALLER already
// holds.
//
// That is why the loop below is over sessions rather than a lookup of one: the
// namespaces this caller may see ARE the ones whose grants govern it. A
// restricted session among them that does not list the door refuses the open; if
// every visible session is unrestricted (the ordinary case, and every operator
// surface) the call proceeds, which is what keeps this from changing anything for
// a caller who was never restricted.
//
// ponytail: a linear scan over live sessions, on an operation a caller performs a
// handful of times per conversation. An index of "the restriction governing a
// caller" is the right shape once a caller can hold several restrictions at once;
// today it cannot.
func (s *Server) refuseDoor(ctx context.Context, stmt directive.Statement, op string) (envelope.Slot, bool) {
	if s.sessMgr == nil {
		return envelope.Slot{}, false
	}
	callerNS := callerNamespace(ctx)
	capID := op + ".open"
	for _, snap := range s.sessMgr.ListAll() {
		if callerNS != "" && snap.Namespace != callerNS {
			continue
		}
		// ListAll hands back snapshots; the restriction lives on the live
		// session object, which is what Get returns.
		sess := s.sessMgr.Get(snap.ID)
		if sess == nil || sess.Restriction().Allows(capID) {
			continue
		}
		return envelope.Slot{Code: 403, Headers: map[string]string{
			"Path":       stmt.Path,
			"Error-Code": CodeForbidden,
		}, Body: "this session's template does not grant " + capID}, true
	}
	return envelope.Slot{}, false
}

// devtoolsSocket turns a browser's debug endpoint into the socket it advertises.
//
// `--remote-debugging-port=0` writes the port to DevToolsActivePort and every
// DevTools build then publishes its real socket at GET /json/version. Accepting
// the http endpoint and resolving it here is what makes `cdp open
// http://127.0.0.1:9222` work, instead of making the caller read a JSON document
// with `file`/`run` and paste a URL out of it.
func devtoolsSocket(ctx context.Context, rawURL string) (string, error) {
	if strings.HasPrefix(rawURL, "ws://") || strings.HasPrefix(rawURL, "wss://") {
		return rawURL, nil
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("cdp open needs a ws:// socket or the http:// devtools endpoint that advertises one, got %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(rawURL, "/")+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("no devtools endpoint at %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	var doc struct {
		Socket string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("%s/json/version is not a devtools document: %w", rawURL, err)
	}
	if doc.Socket == "" {
		return "", fmt.Errorf("%s/json/version advertises no webSocketDebuggerUrl; pass the ws:// URL directly", rawURL)
	}
	return doc.Socket, nil
}

// jrpcChannel dispatches the subcommands that address an opened channel.
//
// A handle that is NOT in this registry is a shell handle, and for `mcp` that is
// the stdio form: a process on a channel, typed at. The other two doors have no
// stdio form, so an unknown handle there is an error rather than a fallback.
func (s *Server) jrpcChannel(ctx context.Context, stmt directive.Statement, win termArgs, op, sub string, rest []string) envelope.Slot {
	if len(rest) == 0 {
		return badRequest(stmt, op+" "+sub+" needs a handle (the one "+op+" open returned)")
	}
	handle := rest[0]
	jrpcChannelsMu.Lock()
	h, ok := jrpcChannels[handle]
	if ok && sub == "close" {
		delete(jrpcChannels, handle)
	}
	jrpcChannelsMu.Unlock()

	if !ok {
		if op == "mcp" && sub != "open" {
			// The stdio form: the handle names a shell running an MCP server.
			return s.mcpStdio(ctx, stmt, sub, handle, rest[1:])
		}
		return envelope.Slot{Code: 404, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "not_found"},
			Body: "no open channel named " + handle + " (copy a handle from " + op + " open, or from ps for a shell)"}
	}
	if sub == "close" {
		_ = h.ch.Close()
		return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
	}

	headers := map[string]string{"Path": stmt.Path}
	switch sub {
	case "recv":
		// recv WAITS, up to the call's own timeout, for the next frame that is
		// not an answer. That is the whole point of it: an event fires on its
		// own schedule (`Page.loadEventFired` lands after `Page.navigate` has
		// already answered), so a recv that only looked at what a previous call
		// happened to catch would never see the one being waited for.
		//
		// The window is the tool's, the same one `read` uses — a caller that
		// wants to wait three seconds says so with `timeout`, and one that wants
		// to peek says `timeout: 0`.
		// TAKE, NOT NEXT: recv is the counterpart of `send`, and send's answer carries
		// an id — which Next deliberately skips (it is an event reader, and an answer
		// to a call nobody is waiting for is filed in the buffer). Pairing `send` with
		// a recv that only calls Next therefore could never deliver the answer: the
		// frame was on the channel, in its own buffer, unreachable. Take sees both, so
		// a pushed event still arrives first and a sent call can finally be answered.
		waitCtx, cancel := recvContext(ctx, win)
		frame, ok := h.ch.Take(waitCtx)
		cancel()
		if ok {
			return envelope.Slot{Code: 200, Headers: headers, Body: frame}
		}
		if h.ch.Transport() == jrpc.HTTP {
			return envelope.Slot{Code: 204, Headers: headers,
				Body: "no frame. On an HTTP channel every frame is an answer to a call, so there is " +
					"nothing to wait for; a peer that pushes on its own needs a socket (cdp open ws://…)."}
		}
		return envelope.Slot{Code: 204, Headers: headers, Body: "no frame within the window"}
	case "tools":
		// MCP only: it is the one door whose peer has a tool list. On the others
		// the subcommand is a lie, and a lie that returns an error frame is worse
		// than a sentence.
		if h.kind != "mcp" {
			return badRequest(stmt, "`"+op+" tools` is MCP's tool list; use `"+op+` call `+handle+` '{"method":"…"}'`+"` instead")
		}
		frame := map[string]any{"jsonrpc": "2.0", "method": "tools/list"}
		return s.jrpcCall(ctx, stmt, h, frame)
	case "call", "send":
		frame, bad := parseJRPCFrame(stmt, rest[1:])
		if bad != nil {
			return *bad
		}
		if sub == "send" {
			if err := s.jrpcReady(ctx, h); err != nil {
				return jrpcError(stmt, err)
			}
			if err := h.ch.Send(ctx, frame); err != nil {
				return jrpcError(stmt, err)
			}
			return envelope.Slot{Code: 202, Headers: headers,
				Body: "sent; read the answer with `" + op + " recv " + handle + "`"}
		}
		return s.jrpcCall(ctx, stmt, h, frame)
	}
	return badRequest(stmt, "unknown "+op+" subcommand: "+sub)
}

// recvContext bounds the wait for a pushed frame by the call's timeout, so recv
// obeys the same window parameter as every other directive.
// recvContext bounds the wait for a pushed frame by the call's timeout, so recv
// obeys the same window parameter as every other directive, and hands back the
// cancel for the caller to release once the wait is over.
func recvContext(ctx context.Context, win termArgs) (context.Context, context.CancelFunc) {
	wait := readTimeout(win)
	if wait <= 0 {
		// timeout 0 is the non-blocking peek: a deadline already passed makes
		// Next return whatever is buffered and stop.
		return context.WithDeadline(ctx, time.Now())
	}
	return context.WithTimeout(ctx, time.Duration(wait*float64(time.Second)))
}

// jrpcCall sends one frame and returns its answer.
func (s *Server) jrpcCall(ctx context.Context, stmt directive.Statement, h *jrpcHandle, frame map[string]any) envelope.Slot {
	if err := s.jrpcReady(ctx, h); err != nil {
		return jrpcError(stmt, err)
	}
	reply, err := h.ch.Call(ctx, frame)
	if err != nil {
		return jrpcError(stmt, err)
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: reply}
}

// jrpcReady runs the MCP handshake once, before the first frame that is not the
// handshake itself.
//
// It is a no-op for the other two doors, and that is the point: DevTools has no
// handshake, and sending one is not harmless — the peer answers "method not
// found" and a caller reading the reply concludes the browser is broken.
func (s *Server) jrpcReady(ctx context.Context, h *jrpcHandle) error {
	if h.kind != "mcp" {
		return nil
	}
	// Held across the handshake, not just around the flag: the point of the
	// mutex is that the SECOND caller waits for the handshake instead of
	// starting a competing one. Checking the flag under the lock and releasing
	// before the call would leave the window open.
	h.initMu.Lock()
	defer h.initMu.Unlock()
	if h.init {
		return nil
	}
	if _, err := h.ch.Call(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpgo.LATEST_PROTOCOL_VERSION,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "termcp", "version": "jrpc"},
		},
	}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	// A notification: no id, so no answer is owed and none is waited for.
	if err := h.ch.Send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	h.init = true
	return nil
}

// parseJRPCFrame reads the frame out of the rest of the line.
//
// The frame is the REST of the statement, quotes and spaces included: a JSON body
// has spaces of its own (`"name": "x"`), so splitting it into words and rejoining
// them is what used to hand json.Unmarshal a fragment. Both spellings — bare and
// quoted — are accepted, because a caller has no way to know which one this layer
// wants.
func parseJRPCFrame(stmt directive.Statement, rest []string) (map[string]any, *envelope.Slot) {
	payload := unquoteWord(strings.TrimSpace(strings.Join(rest, " ")))
	if payload == "" {
		return nil, ptrSlot(badRequest(stmt, "needs a JSON-RPC frame, e.g. call <handle> '{\"method\":\"…\"}'"))
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return nil, ptrSlot(badRequest(stmt, "the frame is not JSON: "+err.Error()))
	}
	return frame, nil
}

// jrpcError renders a transport failure as a reply, never as a success shape.
func jrpcError(stmt directive.Statement, err error) envelope.Slot {
	return envelope.Slot{Code: 500,
		Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed},
		Body:    err.Error()}
}
