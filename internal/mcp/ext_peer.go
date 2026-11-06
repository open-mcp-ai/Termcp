package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
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
//
// THE TABLE IS A sync.Map AND EACH ENTRY IS ITS OWN SINGLE-FLIGHT. One key is one
// independent entry with no invariant spanning keys, which is the shape sync.Map
// is for. The open it guards is a network handshake — a dial, an `initialize`, a
// `tools/list` — and that is exactly what must not sit under a lock: a peer that
// accepts the connection and then stalls in userspace would hold the lock forever
// and queue every other extension behind it. So the creator publishes an entry
// with an unclosed `ready` channel, dials with nothing held, and closes it; a
// concurrent ask for the same key finds that entry and waits on the channel (or
// on its own ctx), then reads the result. That is also what makes a repeated
// `open ext` return the peer already mounted instead of dialling a second one.
var extPeers sync.Map // key string -> *extPeerEntry

// extPeerEntry is one in-flight or completed mount.
//
// handle/err are written by the creator BEFORE close(ready) and are read only
// after <-ready, so the channel close is the happens-before edge and no field
// needs a lock of its own.
type extPeerEntry struct {
	ready  chan struct{}
	handle *jrpcHandle
	err    string
	// termcp carries the verified (or refused) `termcp_tool` declaration (#274).
	// It lives with the mount because it is a fact about this peer's tool list,
	// which is only knowable after the handshake.
	termcp termcpToolCheck
	// abandoned is set by a release that arrived while this mount was still in
	// flight. The creator reads it after close(ready) and closes whatever it
	// opened, because nobody else can reach that handle any more.
	abandoned atomic.Bool
}

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
	for {
		if v, ok := extPeers.Load(key); ok {
			entry := v.(*extPeerEntry)
			select {
			case <-entry.ready:
			case <-ctx.Done():
				return nil, "", envelope.Slot{Code: 504, Headers: map[string]string{"Error-Code": CodeConnectionFailed},
					Body: "still connecting to " + name + "'s endpoint when this call gave up; the mount is in flight and a retry will join it"}, true
			}
			if entry.err != "" {
				extPeers.CompareAndDelete(key, entry)
				return nil, "", envelope.Slot{Code: 502, Headers: map[string]string{"Error-Code": CodeConnectionFailed},
					Body: entry.err}, true
			}
			if entry.handle != nil {
				h := entry.handle
				return h, peerKey(h.sessionID, h.kind, h.index), envelope.Slot{}, true
			}
			return nil, "", envelope.Slot{}, true
		}

		entry := &extPeerEntry{ready: make(chan struct{})}
		if _, loaded := extPeers.LoadOrStore(key, entry); loaded {
			continue // another caller published first; join its entry
		}
		// THIS CALLER IS THE CREATOR. Nothing is held while the endpoint is dialled:
		// the entry is already visible to joiners, and they wait on its channel.
		h, handle, slot, _ := s.dialExtPeer(ctx, ext, sessionID, name)
		entry.handle = h
		if h == nil {
			entry.err = slot.Body
		} else {
			// THE DECLARATION IS CHECKED ONCE, AT MOUNT. Doing it per call would
			// put a second round trip in front of every command, and a downstream
			// that changed its tool list mid-life is a deployment event the next
			// mount will see.
			entry.termcp = s.checkTermcpTool(ctx, ext, h)
		}
		close(entry.ready)
		if entry.abandoned.Load() {
			// A release ran while this was dialling: it could not wait without
			// capping its own latency on a network handshake, so it marked the
			// entry and left the cleanup here, where the handle is now known.
			if h != nil && h.ch != nil {
				s.closeExtPeer(h)
			}
			return nil, "", envelope.Slot{Code: 409, Headers: map[string]string{"Error-Code": CodeConflict},
				Body: name + " was released while its endpoint was being opened; nothing is mounted"}, true
		}
		if h == nil {
			extPeers.CompareAndDelete(key, entry)
			return nil, "", slot, true
		}
		return h, handle, slot, true
	}
}

// dialExtPeer performs the mount itself: the open, the handle lookup and the
// registry entry. It is called by exactly one caller per key, with no lock held.
func (s *Server) dialExtPeer(ctx context.Context, ext *extensions.Extension, sessionID, name string) (*jrpcHandle, string, envelope.Slot, bool) {
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
	return h, handle, slot, true
}

// unmountExtPeer closes the peer mounted for one extension instance and forgets it.
//
// IT NEVER WAITS ON THE MOUNT. A release is a lifecycle operation and the mount it
// might collide with is a network handshake: waiting there would put an unbounded
// external event on the release path, which is how a stuck peer turns "close this
// extension" into a hung request. A mount still in flight is marked abandoned and
// the creator closes what it opened.
func (s *Server) unmountExtPeer(sessionID, name string) {
	key := extPeerKey(sessionID, name)
	v, ok := extPeers.Load(key)
	if !ok {
		return
	}
	entry := v.(*extPeerEntry)
	select {
	case <-entry.ready:
	default:
		entry.abandoned.Store(true)
		extPeers.CompareAndDelete(key, entry)
		return
	}
	if !extPeers.CompareAndDelete(key, entry) {
		return
	}
	if entry.handle != nil {
		s.closeExtPeer(entry.handle)
	}
}

// closeExtPeer drops a mounted peer from the registry and closes its connection.
func (s *Server) closeExtPeer(h *jrpcHandle) {
	if h == nil || h.ch == nil {
		return
	}
	jrpcChannelsMu.Lock()
	delete(jrpcChannels, peerKey(h.sessionID, h.kind, h.index))
	jrpcChannelsMu.Unlock()
	_ = h.ch.Close()
}

// extPeerHandleOf reports the peer mounted for an extension instance, if any.
func (s *Server) extPeerHandleOf(sessionID, name string) string {
	v, ok := extPeers.Load(extPeerKey(sessionID, name))
	if !ok {
		return ""
	}
	h := v.(*extPeerEntry).handle
	if h == nil {
		return ""
	}
	return peerKey(h.sessionID, h.kind, h.index)
}

// extTermcpOf reports the verified declaration for a mounted peer, so `open ext`
// and the status listing can say whether this downstream can be routed to.
func (s *Server) extTermcpOf(sessionID, name string) termcpToolCheck {
	v, ok := extPeers.Load(extPeerKey(sessionID, name))
	if !ok {
		return termcpToolCheck{}
	}
	return v.(*extPeerEntry).termcp
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

// extDocSummary is the summary view of an extension document (issue #271).
//
// The template's notes are the operating instructions, and the longest ones run to
// thirty lines of prose about a protocol the caller reads once. Printing them whole
// on every `open ext` buried the two lines that matter — the handle and the tools —
// under an essay, so a caller paid ~450 tokens to be re-told what a peer is.
//
// A notes block is written in paragraphs, and the first paragraph is the summary by
// construction: it is what the prose opens with. The rest is the detail a caller
// asks for by name with `--all`, which is the same split `ls ext` already makes.
// Empty fields are dropped: `command: ` for an endpoint extension is not
// information, it is the renderer printing a key it never checked.
func extDocSummary(doc map[string]any, peerHandle string, all bool) map[string]any {
	if all || len(doc) == 0 {
		return doc
	}
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		out[k] = v
	}
	if peerHandle != "" {
		// THE PEER'S OWN NOTE REPLACES THE TEMPLATE'S (issue #271).
		//
		// A mounted endpoint extension answers below with `call <handle> <tool>`,
		// spelled with this session's real address. The template's notes say the
		// same thing in the abstract, so keeping both told the caller twice that
		// there is no handshake to write, and the second telling had no handle in
		// it. The abstract half is what `--all` is for.
		out["notes"] = "(full notes under --all; concrete peer syntax follows below)"
	} else if notes, ok := out["notes"].(string); ok {
		if idx := strings.Index(notes, "\n\n"); idx > 0 {
			out["notes"] = strings.TrimRight(notes[:idx], "\n") +
				"\n(add --all for the rest of the notes)"
		}
	}
	if allow, ok := out["allow"].([]any); ok && len(allow) > 3 {
		// The capability list is the template's permission surface, not the answer
		// to `open ext`: a caller driving the extension does not act on it, and it
		// is the widest block left after the notes fold. A count keeps the shape
		// visible; `--all` lists the entries.
		out["allow"] = fmt.Sprintf("%d capabilities (--all to list)", len(allow))
	}
	return out
}
