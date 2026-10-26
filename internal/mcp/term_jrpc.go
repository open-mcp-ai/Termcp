package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/jrpc"
	"github.com/open-mcp-ai/termcp/internal/ns"
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

// jrpcChannels is the registry of opened peers, keyed by their ADDRESS:
// `<session> <kind>:<index>` as one string, "a1f3 cdp:1".
//
// THE KEY IS THE TREE ADDRESS, NOT A MINTED NAME. It used to be `cdp1` — a counter
// printed once in a reply header and never by `ls`, so a caller that lost it had no
// way to get it back, and a caller that had it could not tell which session it
// belonged to. Every other node in the tool is addressed `<session> <kind> <key>`;
// a peer is `<session> <kind>`, and that is now the name.
//
// ponytail: an in-memory map with no eviction. A peer is opened inside one
// conversation and closed with `close`; the leak would be one struct per opened
// URL. Add a reaper when a long-lived instance is observed accumulating them.
var (
	jrpcChannelsMu sync.Mutex
	jrpcChannels   = map[string]*jrpcHandle{}
	peerSeqMu      sync.Mutex
	peerSeq        = map[string]int{}
)

// peerKey renders a peer's address: `<session> <kind>:<index>`.
//
// THE INDEX NAMES THE CONNECTION, and it is what makes two browsers on one machine
// representable. A cdp peer manages a whole browser instance — pages are reached
// through it with the browser's own target ids, so they are not addresses here.
// Whatever is the second one to be opened on that session gets :2 and stays :2.
func peerKey(sessionID, kind, index string) string {
	return sessionID + " " + kind + ":" + index
}

// nextPeerIndex mints the connection number for a new peer.
//
// MONOTONIC, NEVER REUSED, for the same reason the old handle counter was: reusing
// `:1` after a close would make every address a caller remembered point at a
// different browser, and the mistake would be invisible.
//
// ponytail: an in-memory counter beside the registry. It resets when the process
// restarts, which is fine — so did every peer.
func nextPeerIndex(sessionID, kind string) string {
	peerSeqMu.Lock()
	defer peerSeqMu.Unlock()
	peerSeq[peerSeqKey(sessionID, kind)]++
	return strconv.Itoa(peerSeq[peerSeqKey(sessionID, kind)])
}

func peerSeqKey(sessionID, kind string) string { return sessionID + " " + kind }

// jrpcHandle is one opened channel plus the little state a door needs.
type jrpcHandle struct {
	ch   *jrpc.Channel
	kind string // jrpc | mcp | cdp

	// sessionID is the session this peer tunnels through, and it is part of the
	// peer's ADDRESS (`<session> <kind>:<n>`). It belongs to the handle rather than
	// to a directive because the choice was made at `open` time.
	sessionID string
	// index is the connection number inside `<session> <kind>:` — the address is
	// `<sessionID> <kind>:<index>` and nowhere else.
	index string
	// dial is the session's dialer, kept so a door can make its own HTTP requests
	// through the same tunnel the socket uses. `get <peer> protocol` reads the
	// browser's live signature over it, which is the difference between a URL that
	// works and one that only works from here.
	dial jrpc.DialFunc
	// httpBase is the http:// endpoint the peer was opened through, when it was.
	// A browser publishes /json/protocol there; a bare ws:// URL has no such
	// address, and `get <peer> protocol` says so rather than guessing.
	httpBase string

	// cdp is the browser state: attached targets and their session ids. Nil for
	// the other two doors, which have no such concept.
	cdp *cdpState

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

// jrpcOpen connects a URL THROUGH a session and hands back a handle.
//
// THE SESSION IS NOT OPTIONAL, and that is a redesign rather than a restriction.
// A gateway URL names a port on some machine's loopback — 9222 for devtools, 8080 for
// a service that only listens locally — and "which machine" was previously answered by
// accident: the one termcp happened to run on. That made the same call succeed or fail
// depending on the deployment, and the only way to find out which was to try it. Naming
// the session makes the URL mean what it looks like it means.
//
// It also puts gateways back inside the model the rest of the tool uses. A session is
// an SSH link; its loopback, its internal DNS and its proxy settings are the ones a URL
// reaches. Nothing new is exposed by travelling inside that link — the alternative,
// dialling from this process, is the thing that would be a new capability, in the sense
// of one the target host's own network never granted.
func (s *Server) jrpcOpen(ctx context.Context, stmt directive.Statement, op, sessionID, rawURL string) envelope.Slot {
	if sessionID == "" {
		return badRequest(stmt, op+" open needs a session first: "+op+" open <session> <url>\n"+
			"A gateway URL names a port on the network of the machine the session points at, "+
			"so the session decides what the URL can reach. Use `internal` for this machine.")
	}
	if rawURL == "" {
		return badRequest(stmt, op+" open needs a URL after the session: "+op+" open <session> <url>\n"+
			"Example: "+op+" open internal http://localhost:9222")
	}
	dial, bad := s.dialerForSession(sessionID)
	if bad != nil {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeSessionNotRunning},
			Body: fmt.Sprintf("session %q is not running, so there is no tunnel to reach %s through", sessionID, rawURL)}
	}
	// THE SESSION IS RESOLVED TO ITS ID. `open internal cdp …` names a host, and a host
	// resolves to that host's one running link; everything downstream (the protocol
	// fetch, the address itself) names the LINK. Keeping the caller's spelling
	// would leave the reply naming a host that the registry is not keyed by, so the
	// address printed there could not be pasted back into the next command.
	if sess, bad := s.resolveTunnelSession(sessionID); bad == nil {
		sessionID = sess.ID
	}
	var httpBase string
	// The whitelist gate. A session whose template lists capabilities cannot reach
	// a door it did not name — and before the capability rows existed it could
	// reach every one of them, because the session-scoped guard resolves a
	// directive's TARGET and here the target is a URL that names no session. So
	// the check is made against the caller's own door instead.
	//
	// REVIEW is deliberately not run here: whether an admitted open is HELD for a
	// human is the event-hook layer's decision (issue #128), and duplicating that
	// decision at this layer is how the two would drift apart.
	if slot, refused := s.refuseDoor(ctx, stmt, op, sessionID); refused {
		return slot
	}
	// `mcp open .../sse` used to be refused with a sentence about the legacy
	// transport. The streamable door speaks both shapes now (a POST answered by
	// JSON or by an event), so the refusal is gone rather than reworded.
	if op == "cdp" {
		resolved, err := devtoolsSocket(ctx, rawURL, dial)
		if err != nil {
			return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed},
				Body: "discovery: cannot find DevTools websocket endpoint at " + rawURL + ": " + err.Error()}
		}
		// THE HTTP ENDPOINT IS KEPT, not just the socket it advertises. The live
		// protocol document lives at /json/protocol on that endpoint, and it is the
		// only non-guessing source for a method signature.
		if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
			httpBase = strings.TrimRight(rawURL, "/")
		} else {
			// A ws:// socket still has the http endpoint on the same port, which is
			// where DevTools publishes /json/protocol. Deriving it is exact (same host,
			// same port), not a guess — and it keeps `get <peer> protocol` working for
			// a caller who opened the socket directly.
			httpBase = httpBaseOf(resolved)
		}
		rawURL = resolved
	}
	ch, err := jrpc.Open(ctx, rawURL, dial)
	if err != nil {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed},
			Body: "transport: " + err.Error()}
	}
	// EVERY OPEN GETS ITS OWN NUMBER. Two browsers on one machine used to be
	// impossible because the address was `<session> <kind>` and a second open had to
	// either collide or replace it; the number is that address's missing dimension.
	index := nextPeerIndex(sessionID, op)
	handle := peerKey(sessionID, op, index)
	h := &jrpcHandle{ch: ch, kind: op, sessionID: sessionID, index: index, dial: dial, httpBase: httpBase}
	if op == "cdp" {
		h.cdp = newCDPState()
	}
	jrpcChannelsMu.Lock()
	jrpcChannels[handle] = h
	jrpcChannelsMu.Unlock()

	// "OPEN" REPORTS WHAT IT FOUND, NOT JUST THAT IT OPENED.
	//
	// A caller that does not know the MCP specification had no way forward from the
	// old reply: the next step is an `initialize` handshake, then `tools/list`, then
	// a call addressed by tool name, and none of that is guessable. The layer exists
	// precisely to carry that state machine, so it should hand back the state machine's
	// result rather than a receipt.
	//
	// This is also the answer to "is the self-describing layer working?" — a caller
	// that has never read the spec should be able to make the next call correctly from
	// this reply alone.
	body := fmt.Sprintf("%s %s (%s, no shell channel consumed)\n", handle, rawURL, ch.Transport())
	if op == "cdp" {
		// A BROWSER'S NEXT STEP IS NOT A FRAME. Everything useful needs a
		// sessionId, and a caller that does not know that writes Page.navigate at the
		// browser process, reads a frameId, and concludes the browser is broken. So
		// the open reply says which call to make next, and how to find its argument.
		// The flag is `--target`. Without the dashes this line reads as a positional
		// sub-command, which the parser tries as a URL and rejects with a discovery
		// error about "1" — the one instruction this reply gives was wrong, and it was
		// wrong in the same reply that explains why the next call matters.
		body += "\nNext: `ls " + handle + "` lists the tabs. Attach one with `open " + handle +
			" --target <targetId>`, then call through the channel it returns.\n"
		body += "Events are kept: record the number from `get " + handle + "`, act, then `read " + handle + " --since <n>`.\n"
	}
	if op == "mcp" {
		discover, ready, err := s.jrpcDiscoverMCP(ctx, h, handle)
		body += discover
		if !ready {
			// A PEER THAT ANSWERED AND REFUSED IS NOT AN OPEN PEER. This used to answer 200,
			// hand back a handle, and put the failure in the body — so every later call
			// repeated a fact this call already knew, and reported it as 500
			// connection_failed (the transport was fine; the URL was not an MCP endpoint).
			// Found by dogfooding: `open … mcp http://plain-http-server/` answered 200, and
			// the next `call` answered Internal Server Error.
			//
			// A PEER THAT COULD NOT BE REACHED IS STILL AN OPEN PEER, deliberately: an HTTP
			// channel dials on its first frame, so a server that is not up yet is a handle
			// worth having — the same lazy-connect contract the jrpc door has always had.
			// Only "answered, and the answer is not MCP" is final; retrying that URL gives
			// the same refusal forever.
			//
			// THE CHANNEL IS FREED, not left addressable: a handle that cannot carry a tool
			// call has no use but `close`.
			if peerAnswered(err) {
				s.closePeer(stmt, h)
				return envelope.Slot{Code: 502, Headers: map[string]string{
					"Path": stmt.Path, "Error-Code": CodeConnectionFailed,
				}, Body: "the MCP handshake failed" + discover + ", so this URL is not an MCP endpoint.\n" +
					"A 404 usually means the path is wrong (an MCP server answers POST " + rawURL + ", not GET);\n" +
					"a version mismatch means the peer does not speak this protocol. Nothing was opened."}
			}
			body += "\nprotocol: the MCP handshake failed" + discover + ", so tool calls cannot work until\n" +
				"something answers there. The address still names the channel — `close " + handle + "` frees it.\n"
		}
	}
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": handle},
		Body:    body,
	}
}

// peerAnswered reports whether the failure came from a server that replied, as
// opposed to one that could not be reached at all.
//
// The distinction decides whether an `mcp open` is an open: a URL that answers and
// refuses the handshake will answer the same way forever, while a port that is not up
// yet is the ordinary case for a channel that dials lazily. httpCall wraps a non-2xx
// answer as a "<status>: <body>" error and a transport failure as a *url.Error, which
// is what this reads.
func peerAnswered(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	return !errors.As(err, &urlErr)
}

// jrpcDiscoverMCP runs the handshake and lists the peer's tools, so `mcp open`
// can report what the connection is actually for.
//
// Best effort by design: a peer that refuses `tools/list` still opened a usable
// channel, and failing the whole open over a discovery query would turn a partial
// success into a total failure. The boolean says whether the handshake itself
// worked, which IS a fact about the open.
func (s *Server) jrpcDiscoverMCP(ctx context.Context, h *jrpcHandle, handle string) (string, bool, error) {
	if err := s.jrpcReady(ctx, h); err != nil {
		// THE REASON TRAVELS OUT, not just the fact. The caller below used to print
		// "the transport is fine and the URL is right" whoever failed, which is true for a
		// peer that answers with the wrong protocol version and false for a port with
		// nothing on it — and the second is the common case. Swallowing err here forced
		// that sentence to be a guess.
		return " (" + err.Error() + ")", false, err
	}
	raw, err := h.ch.Call(ctx, map[string]any{"jsonrpc": "2.0", "method": "tools/list"})
	if err != nil {
		return "\ntools: the peer negotiated but refused tools/list (" + err.Error() + ")\n", true, nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "\ntools: the peer's tools/list reply was not JSON (" + err.Error() + ")\n", true, nil
	}
	result, _ := doc["result"].(map[string]any)
	items, _ := result["tools"].([]any)
	if len(items) == 0 {
		return "\ntools: none (the peer advertises no tools)\n", true, nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\ntools (%d) — call one with `call %s <tool> {…}` (`get %s tools` lists required keys):\n", len(items), handle, handle))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := getString(item, "name", "?")
		desc := strings.ReplaceAll(getString(item, "description", ""), "\n", " ")
		if len(desc) > 80 {
			desc = desc[:80] + "…"
		}
		b.WriteString("  " + name)
		if desc != "" {
			b.WriteString(" — " + desc)
		}
		b.WriteString("\n")
	}
	return b.String(), true, nil
}

// refuseDoor applies a session's capability whitelist to one of the frame-channel
// doors (mcp / cdp / jrpc).
//
// THE SESSION CHECKED IS THE ONE THE CALL NAMES, and nothing else. It used to walk
// every session in the caller's namespace and refuse as soon as ANY of them lacked
// the capability — so one restricted profile anywhere in the instance (any extension
// template that does not list `mcp.open`) refused the door for every caller, and
// `--allow mcp.open` on the session actually being used changed nothing. A door is
// opened THROUGH a session, and that session's template is whose whitelist applies
// (#159).
func (s *Server) refuseDoor(ctx context.Context, stmt directive.Statement, op, sessionID string) (envelope.Slot, bool) {
	if s.sessMgr == nil || sessionID == "" {
		return envelope.Slot{}, false
	}
	sess, bad := s.resolveTunnelSession(sessionID)
	if bad != nil || sess == nil {
		return envelope.Slot{}, false
	}
	if callerNS := callerNamespace(ctx); callerNS != "" && sess.Namespace != callerNS {
		return envelope.Slot{Code: 403, Headers: map[string]string{
			"Path":       stmt.Path,
			"Error-Code": CodeForbidden,
		}, Body: ns.ForbiddenForeign(callerNS)}, true
	}
	capID := op + ".open"
	if sess.Restriction().Allows(capID) {
		return envelope.Slot{}, false
	}
	return envelope.Slot{Code: 403, Headers: map[string]string{
		"Path":       stmt.Path,
		"Error-Code": CodeForbidden,
	}, Body: "this session's template does not grant " + capID}, true
}

// devtoolsSocket turns a browser's debug endpoint into the socket it advertises.
//
// `--remote-debugging-port=0` writes the port to DevToolsActivePort and every
// DevTools build then publishes its real socket at GET /json/version. Accepting
// the http endpoint and resolving it here is what makes `cdp open
// http://127.0.0.1:9222` work, instead of making the caller read a JSON document
// with `file`/`run` and paste a URL out of it.
func devtoolsSocket(ctx context.Context, rawURL string, dial jrpc.DialFunc) (string, error) {
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
	// THE DISCOVERY REQUEST TRAVELS THE SAME WAY THE SOCKET WILL. A devtools endpoint
	// reachable only from the target host would otherwise be discovered from here and
	// then fail to connect, reporting a connection error for a URL that is in fact
	// correct on the host that owns it. The client is built here rather than reused
	// because it is used exactly once and its transport must carry this session's dial.
	client := &http.Client{Transport: &http.Transport{DialContext: dial}}
	resp, err := client.Do(req)
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
		return badRequest(stmt, op+" "+sub+" needs a channel address (the one "+op+" open returned)")
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
			return s.mcpStdio(ctx, stmt, sub, handle, rest[1:], win)
		}
		return envelope.Slot{Code: 404, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "not_found"},
			Body: "no open channel named " + handle + " (copy a channel address from " + op + " open, or from ps for a shell)"}
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
			return s.frameReply(ctx, stmt, h, frame, win)
		}
		// A RECV THAT TOOK NOTHING DELIVERED NOTHING, so it is a 204 — and the two
		// sentences below are not the same news: on HTTP there is nothing to wait for at
		// all, on a socket the window was simply empty. Each rides in the Hint header,
		// never the body, for the reason noContentSaying documents.
		if h.ch.Transport() == jrpc.HTTP {
			return noContentSaying(headers, "no frame. On an HTTP channel every frame is an answer to a call, so there is "+
				"nothing to wait for; a peer that pushes on its own needs a socket (cdp open ws://…).")
		}
		return noContentSaying(headers, "no frame within the window")
	case "call", "send":
		// A TOOL NAME IS NOT A FRAME, and the two cannot be confused: a frame is a JSON
		// object and starts with '{', a tool name never does (#159). The test peels the
		// caller's grouping quotes first, because `call <peer> '{"…"}'` is how a frame
		// with spaces in it is spelled — the quote is grouping, not part of the value,
		// and reading it as "not a frame" would refuse the commonest spelling there is.
		// Every other frame — notifications, progress, sampling, anything custom —
		// keeps the escape hatch unchanged.
		if op == "mcp" && len(rest) > 1 && !strings.HasPrefix(unquoteWord(strings.TrimSpace(rest[1])), "{") {
			return s.mcpToolCall(ctx, stmt, h, strings.Join(rest[1:], " "), win)
		}
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
		if err := s.jrpcReady(ctx, h); err != nil {
			return jrpcError(stmt, err)
		}
		reply, err := h.ch.Call(ctx, frame)
		if err != nil {
			return jrpcError(stmt, err)
		}
		return s.frameReply(ctx, stmt, h, reply, win)
	}
	// NO FALLBACK: the subcommand is not the caller's word here. The verbs that reach
	// this function (call, write --send, read --frame, close) each name the one
	// subcommand they mean, so an unrecognised one is a bug in this file rather than
	// something a caller typed, and answering it with a sentence would hide that.
	return envelope.Slot{Code: 500, Headers: map[string]string{
		"Path": stmt.Path, "Error-Code": CodeOperationFailed,
	}, Body: "internal: no handler for " + op + " " + sub}
}

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
		return nil, ptrSlot(badRequest(stmt, "needs a JSON-RPC frame, e.g. call <channel> '{\"method\":\"…\"}'"))
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return nil, ptrSlot(badRequest(stmt, "the frame is not JSON: "+err.Error()+"\n"+frameSpelling))
	}
	return frame, nil
}

// frameSpelling is the one sentence that turns "invalid character" into a fix.
//
// A JSON body is full of double quotes, so the instinct is to wrap it in double
// quotes and escape the ones inside — which is what a shell would need and this
// layer does not do: quoting here groups the line into one word and is then
// removed, one layer, with the bytes inside left alone. The escaped spelling
// therefore arrives at the parser as backslashes and is refused for it.
const frameSpelling = "A frame is passed bare or in single quotes — quoting here only groups the line, " +
	"it does not escape: `call <peer> '{\"method\":\"tools/list\"}'`."

// jrpcError renders a transport failure as a reply, never as a success shape.
func jrpcError(stmt directive.Statement, err error) envelope.Slot {
	return envelope.Slot{Code: 500,
		Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed},
		Body:    err.Error()}
}
