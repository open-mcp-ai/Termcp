package mcp

import (
	"context"
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// extPeers maps an extension instance to the MCP peer mounted for it (issue #271).
//
// WHY THIS EXISTS. An extension that declares `url = ...` is an MCP server, and
// termcp already knows how to be a client of one: `jrpc.Open` gives it the
// handshake, id dispatch, tool discovery and text unpacking, and `call <session>
// mcp:1 <tool> {…}` is the surface an agent should be using. Before this, the
// reconciler instead started `termcp-mcp-bridge` as a CHILD PROCESS on a pipe
// channel and handed the agent that channel — a relay that forwards bytes and
// manages nothing, so every tool call meant the caller hand-writing
// `initialize`, `tools/list` and a `tools/call` envelope, then reading frames off
// a byte stream and matching ids itself. That is the "low efficiency" this file
// removes.
//
// WHY NOT IN extensions. The reconciler owns session and channel lifecycle; it
// cannot own a peer, because `jrpcHandle` lives in this package and extensions
// must not import it. So the mount happens here, on the surface where `open ext`
// is answered, and the reconciler keeps doing what it is good at: dialling the
// session.
//
// THE KEY IS THE SESSION AND THE NAME, because that is what a second `open ext`
// on the same extension resolves to: the peer is per instance, and an instance is
// a (session, extension name) pair.
var (
	extPeersMu sync.Mutex
	extPeers   = map[string]*jrpcHandle{}
)

func extPeerKey(sessionID, name string) string { return sessionID + "\x00" + name }

// mountExtPeer opens an extension's endpoint as a real MCP peer and returns its
// address. It is idempotent: a second ask for the same instance returns the peer
// already mounted, which is what makes `open ext` safe to repeat.
//
// headers carries the template's `--headers`, which is the whole reason this path
// could not exist before `jrpc.OpenWithHeaders`: a credential had nowhere to go.
func (s *Server) mountExtPeer(ctx context.Context, ext *extensions.Extension, sessionID, name string) (*jrpcHandle, string, envelope.Slot, bool) {
	if ext == nil || strings.TrimSpace(ext.URL) == "" {
		return nil, "", envelope.Slot{}, false
	}
	if sessionID == "" {
		return nil, "", envelope.Slot{Code: 400, Headers: map[string]string{"Error-Code": CodeSessionNotRunning},
			Body: "extension " + name + " needs a running session to reach its endpoint through"}, true
	}
	key := extPeerKey(sessionID, name)
	extPeersMu.Lock()
	if h, ok := extPeers[key]; ok && h != nil && h.ch != nil {
		extPeersMu.Unlock()
		return h, peerKey(h.sessionID, h.kind, h.index), envelope.Slot{}, true
	}
	extPeersMu.Unlock()

	// The open itself goes through the same path a hand-written `open <session> mcp
	// <url>` does, so admission, the capability door, the handshake and the tool
	// listing are not re-implemented here — they are the peer layer's, unchanged.
	stmt := directive.Statement{Op: "mcp", Path: "open ext " + name, Target: sessionID}
	slot := s.jrpcOpenWithHeaders(ctx, stmt, "mcp", sessionID, ext.URL, ext.Headers)
	if slot.Code >= 400 {
		return nil, "", slot, true
	}
	handle := slot.Headers["Handle"]
	if handle == "" {
		return nil, "", slot, true
	}
	jrpcChannelsMu.Lock()
	h := jrpcChannels[handle]
	jrpcChannelsMu.Unlock()
	if h == nil {
		return nil, "", slot, true
	}
	extPeersMu.Lock()
	extPeers[key] = h
	extPeersMu.Unlock()
	return h, handle, slot, true
}

// unmountExtPeer closes the peer mounted for one extension instance and forgets it.
func (s *Server) unmountExtPeer(sessionID, name string) {
	key := extPeerKey(sessionID, name)
	extPeersMu.Lock()
	h := extPeers[key]
	delete(extPeers, key)
	extPeersMu.Unlock()
	if h != nil && h.ch != nil {
		jrpcChannelsMu.Lock()
		delete(jrpcChannels, peerKey(h.sessionID, h.kind, h.index))
		jrpcChannelsMu.Unlock()
		_ = h.ch.Close()
	}
}

// extPeerHandleOf reports the peer mounted for an extension instance, if any.
func (s *Server) extPeerHandleOf(sessionID, name string) string {
	extPeersMu.Lock()
	h := extPeers[extPeerKey(sessionID, name)]
	extPeersMu.Unlock()
	if h == nil {
		return ""
	}
	return peerKey(h.sessionID, h.kind, h.index)
}

// extPeerNote renders the paragraph an `open ext` reply carries so an agent knows
// it is talking to a peer and not to a pipe, and what to type next.
func extPeerNote(handle string) string {
	var b strings.Builder
	b.WriteString("\nThis extension is a real MCP peer, not a pipe: no handshake and no frames to write.\n")
	b.WriteString("  get " + handle + " tools            list its tools and their parameters\n")
	b.WriteString("  call " + handle + " <tool> {…}      call one and get its text back\n")
	return b.String()
}
