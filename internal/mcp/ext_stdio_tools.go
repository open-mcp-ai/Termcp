package mcp

// MCP OVER A PIPE CHANNEL, AS A PEER (issue #316).
//
// An extension whose channel runs a stdio MCP server is a peer in every sense the HTTP
// form is: there is a handshake, a tool table, and answers that arrive as frames. Only
// the HTTP form was TREATED as one — `open ext` mounted a real peer for a template with a
// url, while the same server reached over a pipe was handed back as a bare channel and
// the caller was left to write `initialize`, `notifications/initialized` and a
// `tools/call` envelope, then read frames off a byte stream with a cursor and match ids
// by hand. Same protocol, two very different amounts of work.
//
// WHAT MAKES THE TOOL FORM LEGAL HERE IS NOT "IT IS A PIPE". A channel is an unmanaged
// byte pipe: whatever runs on it may be anything at all, which is why `call <channel>
// <tool> {…}` is refused there and stays refused (#271). The tool form is legal because
// the reconciler OWNS the channel and the template says the process speaks MCP — so the
// shape of the conversation is a fact rather than a guess about bytes.
//
// THE WRITES AND THE READS ARE THE EXISTING ONES: extWriteLine holds the channel's line
// lock (two writes make one line, #212) and mcpAwaitReply correlates by id, parks the
// frames that answer other ids, and gives up at the caller's own window. This file adds
// only the surface — a tool name and an arguments object in, the tool's text out.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// extStdioHandshakeWindow bounds the initialize round trip.
//
// IT IS NOT THE CALLER'S WINDOW, and that is deliberate: the handshake happens once per
// channel and every caller after the first waits for the same one, so it must not be
// short because one caller passed `timeout: 0.2`. Fifteen seconds is the budget
// jrpcReady gives the HTTP form's handshake.
const extStdioHandshakeWindow = 15 * time.Second

// stdioPeer is one extension channel's MCP client state.
//
// THE HANDSHAKE IS A CHANNEL BROADCAST, NOT A MUTEX (AGENTS.md): the first caller starts
// it in a goroutine and closes done when it settles, and every other caller selects on
// done against its own ctx. A mutex would make the second caller wait for a round trip
// with no way out, which is how a stalled peer becomes a hung request.
type stdioPeer struct {
	once sync.Once
	done chan struct{}
	err  error
	// seq mints ids for the frames this layer sends. It is atomic because two callers on
	// one channel must never share an id: a reply is matched by its raw spelling, so a
	// duplicate would hand one caller the other's answer.
	seq atomic.Int64
}

// extStdioPeerFor returns the client state for a channel, creating it on first use.
func (s *Server) extStdioPeerFor(shellID string) *stdioPeer {
	if v, ok := s.stdioPeers.Load(shellID); ok {
		return v.(*stdioPeer)
	}
	actual, _ := s.stdioPeers.LoadOrStore(shellID, &stdioPeer{})
	return actual.(*stdioPeer)
}

// stdioMCPChannel reports the extension that owns this channel when the process on it
// speaks MCP over a pipe, and "" otherwise.
//
// A PTY IS EXCLUDED BY NAME, not by preference: a terminal echoes what is typed at it, so
// the echo of a request arrives before the answer and a line-delimited frame stream would
// be corrupted by its own reflection.
func (s *Server) stdioMCPChannel(handle string) string {
	if s.extensions == nil {
		return ""
	}
	cs, bad := s.requireShell(handle)
	if bad != nil || cs == nil {
		return ""
	}
	if cs.Info().Mode == api.ModePTY {
		return ""
	}
	name, kind, ok := s.extensions.KindOfShell(cs.ID)
	if !ok || kind != extensions.ChannelStdio {
		return ""
	}
	return name
}

// extStdioToolCall answers `call <extension stdio channel> <tool> [<json-args>]`.
//
// send writes the frame and returns at once, the way `--send` does for every other peer:
// a fire-and-forget tool call has no answer to wait for, and waiting would spend the
// caller's window on a reply nobody reads.
func (s *Server) extStdioToolCall(ctx context.Context, stmt directive.Statement, handle, payload string, win termArgs, send bool) envelope.Slot {
	tool, args, bad := splitToolCallAt(stmt, payload, "call "+handle+" <tool> [<json-args>]")
	if bad != nil {
		return *bad
	}
	cs, why := s.requireShell(handle)
	if why != nil {
		return s.slotFromFailure(ctx, stmt, failureFromResult(why))
	}
	if send {
		peer, ready := s.extStdioReadyFor(ctx, cs)
		if ready != nil {
			return *ready
		}
		rawID, err := s.extStdioSend(ctx, stmt, cs, peer, "tools/call",
			map[string]any{"name": tool, "arguments": args})
		if err != nil {
			return envelope.Slot{Code: 502, Headers: map[string]string{
				"Path": stmt.Path, "Error-Code": CodeConnectionFailed,
			}, Body: err.Error()}
		}
		_ = rawID
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path},
			Body: "sent `" + tool + "`; the answer arrives on `read " + handle + " --frame`"}
	}
	reply, slot := s.extStdioAsk(ctx, stmt, cs, handle, "tools/call",
		map[string]any{"name": tool, "arguments": args}, win)
	if slot != nil {
		return *slot
	}
	if boolFlag(stmt, "json") {
		// The escape hatch back to the envelope, exactly as it is for the HTTP form: the
		// content items, the isError flag and any structuredContent are still there.
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: reply}
	}
	return extToolReply(stmt, reply, win, handle)
}

// extStdioToolList answers `get <extension stdio channel> tools [<name>]`.
//
// THE SAME TABLE THE HTTP FORM PRINTS, from the same renderer: a caller that switched
// transports must not have to read a second shape to find out what the server offers.
func (s *Server) extStdioToolList(ctx context.Context, stmt directive.Statement, handle string, win termArgs, tool string) envelope.Slot {
	cs, why := s.requireShell(handle)
	if why != nil {
		return s.slotFromFailure(ctx, stmt, failureFromResult(why))
	}
	reply, slot := s.extStdioAsk(ctx, stmt, cs, handle, "tools/list", nil, win)
	if slot != nil {
		return *slot
	}
	tools, ok := parseToolList(reply)
	if !ok {
		if msg := frameErrorMessage(reply); msg != "" {
			return envelope.Slot{Code: 500, Headers: map[string]string{
				"Path": stmt.Path, "Error-Code": "peer_error",
			}, Body: "the extension answered with an error: " + msg}
		}
		return envelope.Slot{Code: 200, Headers: map[string]string{
			"Path": stmt.Path, "Hint": "this is not a tools/list answer; `read " + handle + " --frame` shows the frames as they arrive",
		}, Body: reply}
	}
	return renderToolTable(stmt, tools, handle, handle, tool)
}

// extStdioAsk sends one request and waits for its answer.
//
// THE RETURNED SLOT IS A FAILURE, and nil means "the reply is in the string": every
// failure shape this surface has — a dead channel, a handshake that did not complete, a
// window that ran out — is decided in one place, so a caller reading a refusal reads the
// same sentence whichever request produced it.
func (s *Server) extStdioAsk(ctx context.Context, stmt directive.Statement, cs *session.ChildShell, handle, method string, params map[string]any, win termArgs) (string, *envelope.Slot) {
	peer, ready := s.extStdioReadyFor(ctx, cs)
	if ready != nil {
		return "", ready
	}
	rawID, err := s.extStdioSend(ctx, stmt, cs, peer, method, params)
	if err != nil {
		return "", &envelope.Slot{Code: 502, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": CodeConnectionFailed,
		}, Body: "the extension's channel refused the frame: " + err.Error()}
	}
	// THE WAIT IS KEYED BY THE CHILD SHELL, the same key the handshake used: the frames of
	// one conversation share one cursor, and a second cursor for a second spelling of the
	// same channel would re-read the answers the first one already delivered.
	slot := s.mcpAwaitReply(ctx, stmt, s.channelStateKey(cs.ID), rawID, win, map[string]string{"Path": stmt.Path})
	if slot.Code >= 400 {
		return "", &slot
	}
	return slot.Body, nil
}

// extStdioSend writes one request frame and returns the id it minted.
func (s *Server) extStdioSend(ctx context.Context, stmt directive.Statement, cs *session.ChildShell, peer *stdioPeer, method string, params map[string]any) (string, error) {
	rawID := fmt.Sprintf("%q", fmt.Sprintf("termcp-%d", peer.seq.Add(1)))
	frame := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(rawID), "method": method}
	if params != nil {
		frame["params"] = params
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return "", fmt.Errorf("cannot encode the %s frame: %w", method, err)
	}
	if err := s.extWriteLine(ctx, cs.ID, string(body)); err != nil {
		return "", err
	}
	return rawID, nil
}

// extStdioReadyFor returns the channel's client state once its handshake has completed.
func (s *Server) extStdioReadyFor(ctx context.Context, cs *session.ChildShell) (*stdioPeer, *envelope.Slot) {
	peer := s.extStdioPeerFor(cs.ID)
	peer.once.Do(func() {
		peer.done = make(chan struct{})
		go func() {
			defer close(peer.done)
			// THE HANDSHAKE'S OWN DEADLINE, not the caller's: it is shared, so one
			// caller's short window must not cancel it for the callers behind it.
			initCtx, cancel := context.WithTimeout(context.Background(), extStdioHandshakeWindow)
			defer cancel()
			peer.err = s.extStdioHandshake(initCtx, cs.ID)
		}()
	})
	select {
	case <-peer.done:
	case <-ctx.Done():
		return nil, &envelope.Slot{Code: 504, Headers: map[string]string{
			"Error-Code": "peer_timeout",
		}, Body: "the extension's MCP handshake is still in flight and this call gave up waiting for it. " +
			"A retry joins the handshake already running; give the call a larger `timeout` if the server is slow to start."}
	}
	if peer.err != nil {
		return nil, &envelope.Slot{Code: 502, Headers: map[string]string{
			"Error-Code": CodeConnectionFailed,
		}, Body: "the extension's MCP handshake did not complete: " + peer.err.Error()}
	}
	return peer, nil
}

// extStdioHandshake sends initialize, waits for its answer, then sends
// notifications/initialized.
//
// ONE HANDSHAKE PER PROCESS, which is what the cache above buys: a stdio MCP server has
// one session, and a second initialize is a second state transition in a process that has
// already made it.
func (s *Server) extStdioHandshake(ctx context.Context, shellID string) error {
	const initID = `"termcp-init"`
	initFrame := `{"jsonrpc":"2.0","id":` + initID + `,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"termcp","version":"term"}}}`
	if err := s.extWriteLine(ctx, shellID, initFrame); err != nil {
		return err
	}
	stmt := directive.Statement{Path: "initialize", Target: shellID}
	win := termArgs{TimeoutSet: true, TimeoutSeconds: extStdioHandshakeWindow.Seconds()}
	slot := s.mcpAwaitReply(ctx, stmt, shellID, initID, win, map[string]string{"Path": "initialize"})
	if slot.Code >= 400 {
		return fmt.Errorf("%s", strings.TrimSpace(slot.Body))
	}
	if msg := frameErrorMessage(slot.Body); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	// A NOTIFICATION HAS NO REPLY, so it is written and not awaited.
	return s.extWriteLine(ctx, shellID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// extToolReply renders a tools/call answer as the text the tool returned.
//
// THE ENVELOPE IS NOISE HERE, the same way it is for an HTTP peer: a caller that asked
// for a tool's text should not have to walk {"result":{"content":[…],"isError":false}}
// itself, in a shell, with no JSON parser.
func extToolReply(stmt directive.Statement, reply string, win termArgs, handle string) envelope.Slot {
	body, isError, ok := unpackToolResult(reply)
	if !ok {
		// A SHAPE THIS DOES NOT RECOGNISE IS SHOWN AS IT ARRIVED. That covers the peer's
		// own error object — which is a failure and must not read as success — and any
		// method whose result is not a tool result.
		if msg := frameErrorMessage(reply); msg != "" {
			return envelope.Slot{Code: 500, Headers: map[string]string{
				"Path": stmt.Path, "Error-Code": "peer_error",
			}, Body: "the extension answered with an error: " + msg}
		}
		return envelope.Slot{Code: 200, Headers: map[string]string{
			"Path": stmt.Path, "Hint": "this is not a tools/call result; `read " + handle + " --frame` shows the frames as they arrive",
		}, Body: reply}
	}
	if isError {
		// A TOOL THAT REPORTED FAILURE DID NOT SUCCEED (issue #159): MCP carries that in
		// the result rather than in an error object, so a status-only caller would read
		// 200 and conclude the work was done.
		return envelope.Slot{Code: 500, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "tool_error",
		}, Body: "ERROR: " + body}
	}
	inlineLimit := win.BytesLimit
	if inlineLimit <= 0 {
		inlineLimit = defaultBytesLimit
	}
	if len(body) > inlineLimit {
		// The frame is still in the channel, so the pointer is the channel itself rather
		// than a frame number this surface does not keep.
		return envelope.Slot{Code: 200, Headers: map[string]string{
			"Path": stmt.Path, "Decoded-Bytes": fmt.Sprint(len(body)),
		}, Body: fmt.Sprintf("the tool's text is %d bytes, over the %d-byte window this reply may carry.\n"+
			"The frame is on the channel: `read %s --frame` hands it out whole, and `--json` returns the envelope.",
			len(body), inlineLimit, handle)}
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: body}
}

// frameErrorMessage reads the message out of a JSON-RPC error object, or "" when the
// frame carries none.
func frameErrorMessage(frame string) string {
	var doc struct {
		Error *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(frame), &doc); err != nil || doc.Error == nil {
		return ""
	}
	if doc.Error.Message == "" {
		return fmt.Sprintf("error code %d", doc.Error.Code)
	}
	return doc.Error.Message
}
