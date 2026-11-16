package mcp

// The resident pump behind /ext/{name}/stream (issue #328).
//
// ONE EXTENSION'S MCP SERVER, MULTIPLEXED. An extension's server lives on a
// line-delimited channel: a frame goes in as a typed line and the answer comes back
// on the same stream, mixed in with whatever the process printed. Until this file
// existed, a POST wrote its frame and waited for the matching id inside its own
// request window — so a frame the extension emitted on its own (a
// `notifications/<ns>/<event>`) was read by nobody, and a reply that arrived after
// the window was lost with a 202 that claimed success.
//
// The shape here is: one goroutine reads the channel continuously, splits it into
// frames, and routes each frame to exactly one of four destinations. Nothing polls.
// The reader is independent of every other consumer of that channel (shell_reader's
// own reader starts at the current end), so `read`, `recv` and `call` are untouched.
//
// FOUR DESTINATIONS, and the distinction is the whole design:
//
//	notification (method, no id)   fan out to every session's GET stream
//	answer to our upstream id      wake the POST waiting for it, id rewritten back
//	request from the peer          hand to a client that declared capabilities,
//	                               or answer the peer with an error — never silently drop
//	anything else                  skip: a banner, a prompt, an echo
//
// THE IDS ARE MINTED HERE. Two clients both numbering their requests `1` is
// ordinary, and the peer has one id space. So a request's id is replaced with a
// unique upstream spelling on the way in and restored on the way out, and the
// rewrite touches ONLY the id member: every other byte of the peer's frame reaches
// the client exactly as the peer wrote it, because "the client is talking to the
// extension" is this endpoint's entire promise.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	mcplib "github.com/open-mcp-ai/termcp/mcp"

	"github.com/open-mcp-ai/termcp/internal/session"
)

const (
	// extPumpWindow is how long one read waits for bytes before looping. It is not
	// a frame timeout: a read that returns nothing simply comes back and waits
	// again, which is also the moment the extension's idle clock is renewed.
	extPumpWindow = 30 * time.Second
	// extPumpBytes bounds one read. It matches the transport's frame limit, so a
	// frame the client was allowed to send is a frame this pump can receive.
	extPumpBytes = 8 << 20
	// extPumpCarryLimit bounds the unparsed remainder. A peer that prints a stream
	// of bytes which never becomes a frame must not grow this buffer forever.
	extPumpCarryLimit = 16 << 20
	// extSentMemory is how long a frame this endpoint typed stays recognisable as
	// its own echo. A pty echoes input before the process sees it, so an echoed
	// request would otherwise look like a request from the peer.
	extSentMemory = 30 * time.Second
	// extMCPStartWait is how long a request waits for an on-demand extension to
	// come up before answering. It is bounded because an HTTP client is holding a
	// connection: a target that takes minutes to answer must not turn into a
	// request that never answers, or the client's own timeout fires with no
	// explanation and it blames the network.
	extMCPStartWait = 20 * time.Second
)

// extChannel is the byte pipe to one extension's MCP server.
//
// An interface rather than the shell API directly, for two reasons: the pump needs
// only "write a line" and "read some bytes", and a test can then drive the whole
// endpoint — fan-out, id rewriting, the four routes — without an SSH session.
type extChannel interface {
	// Write types one frame as a line, complete with its terminator.
	Write(ctx context.Context, frame string) error
	// Open returns a reader positioned at the CURRENT end of the stream: a late
	// subscriber must not receive the history it missed.
	Open(ctx context.Context) (extReader, error)
	// Touch renews the extension's idle clock, so a resident pump is not reaped
	// out from under a subscriber.
	Touch()
	// Alive reports whether the channel can still be read. A nil check is not
	// enough: an extension that exited leaves a channel object behind, and the
	// failure only shows up on the next read — as a hang rather than an error.
	Alive() bool
}

// extReader is one independent read position on a channel.
type extReader interface {
	// Read returns bytes that arrived since the last read, blocking up to an
	// internal window. A nil slice with a nil error means "nothing yet".
	Read(ctx context.Context) ([]byte, error)
	Close()
}

// shellExtChannel drives a real extension channel through the shell tools.
type shellExtChannel struct {
	srv           *Server
	shellID       string
	parentSession string
}

func (c *shellExtChannel) Write(ctx context.Context, frame string) error {
	// ONE LINE AT A TIME (#212). The text and the enter are two calls into the
	// shell layer, and two writers interleaving between them produce a stream no
	// line reader can parse — two requests fused into one corrupt line.
	line := c.srv.lineLock(c.shellID)
	line.Lock()
	defer line.Unlock()
	if _, fail := c.srv.callTool(ctx, "shell_input", argsOf("shell_id", c.shellID, "text", frame)); fail != nil {
		return fmt.Errorf("%s", fail.msg)
	}
	if _, fail := c.srv.callTool(ctx, "shell_key", argsOf("shell_id", c.shellID, "key", "enter")); fail != nil {
		return fmt.Errorf("%s", fail.msg)
	}
	return nil
}

func (c *shellExtChannel) Open(ctx context.Context) (extReader, error) {
	cs := c.srv.sessMgr.GetChildShell(c.shellID)
	if cs == nil {
		return nil, fmt.Errorf("channel %s is not running", c.shellID)
	}
	id, err := cs.RegisterReader()
	if err != nil {
		return nil, err
	}
	return &shellExtReader{cs: cs, id: id}, nil
}

func (c *shellExtChannel) Touch() {
	if c.srv.extensions != nil {
		c.srv.extensions.Touch(c.shellID, c.parentSession)
	}
}

func (c *shellExtChannel) Alive() bool {
	return c.srv.sessMgr.GetChildShell(c.shellID) != nil
}

type shellExtReader struct {
	cs *session.ChildShell
	id int
}

func (r *shellExtReader) Read(ctx context.Context) ([]byte, error) {
	// strip_ansi is FALSE: the frame is a byte sequence the peer wrote, and
	// removing escape sequences from it would corrupt a JSON string that happened to
	// contain one.
	_, raw, _, err := r.cs.ReadTerminalStream(ctx, r.id, extPumpWindow, false, 0, extPumpBytes, nil)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *shellExtReader) Close() {
	r.cs.UnregisterReader(r.id)
}

// extPending is one POST waiting for the answer to the id it sent.
type extPending struct {
	ch chan []byte
	// sessionID names the session to fall back to when the POST has already given
	// up: the answer is still deliverable, just not through that request.
	sessionID string
	// originalID is the client's own spelling, restored before delivery.
	originalID json.RawMessage
}

// extSession is what this endpoint remembers about one attached client.
type extSession struct {
	sess mcplib.Session
	// caps is what the client declared in initialize. A peer-initiated request is
	// handed to a client that declared something; a client that declared nothing
	// cannot be expected to answer a method it never advertised.
	caps map[string]any
}

// extEndpoint is one extension's MCP surface: a transport, a pump, and the id
// bookkeeping that makes N clients look like one peer.
type extEndpoint struct {
	srv  *Server
	name string
	// transport is the streamable-HTTP implementation this endpoint reuses. It
	// owns the session ids, the GET stream and DELETE; nothing about them is
	// re-implemented here.
	transport *mcplib.StreamableHTTPServer
	// newChannel builds the byte pipe. A test replaces it; production uses
	// openShellChannel.
	newChannel func(ctx context.Context) (extChannel, error)

	mu       sync.Mutex
	channel  extChannel
	sessions map[string]*extSession
	// pumpCancel/pumpDone track the reader goroutine. It runs exactly while at
	// least one session is attached: an idle pump would keep touching the
	// extension's idle clock and defeat the reaper.
	pumpCancel context.CancelFunc
	pumpDone   chan struct{}
	// channelEnded is the reason the pump stopped, kept so a later POST can say
	// what happened instead of reporting a bare timeout.
	channelEnded string
	// channelShellID is the handle the cached channel runs on, and lastShellID is
	// the one the most recent Acquire produced. The second exists because
	// openShellChannel runs outside the lock: it records what it found, and the
	// caller adopts it once the channel object exists.
	channelShellID string
	lastShellID    string
	parentSession  string

	upstreamSeq uint64
	pending     sync.Map // upstream id spelling -> *extPending
	// peerOutstanding holds ids of requests the PEER initiated and we forwarded,
	// so its answer can be recognised and sent back without an id rewrite.
	peerOutstanding sync.Map // id spelling -> struct{}

	initMu      sync.Mutex
	initReply   []byte // the peer's initialize answer, verbatim
	initSent    bool   // the peer has been initialised
	initialized bool   // notifications/initialized has been forwarded once
	protoVer    string // the version the peer reported

	sentMu sync.Mutex
	sent   []sentFrame
}

type sentFrame struct {
	body string
	at   time.Time
}

func newExtEndpoint(srv *Server, name string) *extEndpoint {
	ep := &extEndpoint{
		srv:      srv,
		name:     name,
		sessions: map[string]*extSession{},
	}
	ep.newChannel = ep.openShellChannel
	ep.transport = mcplib.NewStreamableHTTPServer(ep)
	return ep
}

// ProtocolVersion reports the version the EXTENSION settled on, so the transport
// can stop claiming one on the peer's behalf (issue #328). An extension that has
// not answered initialize yet has no opinion, and the transport's own default
// stands until it does.
func (ep *extEndpoint) ProtocolVersion() string {
	ep.initMu.Lock()
	defer ep.initMu.Unlock()
	return ep.protoVer
}

// RegisterSession records a client and makes sure the channel is being read.
//
// THE PUMP STARTS HERE RATHER THAN AT THE FIRST POST, because a client's GET stream
// can carry an event before that client posts anything at all — the extension does
// not know or care who is listening. The read position is the current end of the
// stream, so nothing from before this moment is replayed.
//
// A failure is not reported here: the transport has no error channel for it, and the
// client is about to POST initialize, which is where Handle answers with the reason.
func (ep *extEndpoint) RegisterSession(ctx context.Context, sess mcplib.Session) {
	ep.mu.Lock()
	ep.sessions[sess.SessionID()] = &extSession{sess: sess}
	ep.mu.Unlock()
	_, _ = ep.ensureChannel(ctx)
}

// UnregisterSession forgets a client, and stops the pump when it was the last one.
//
// THE CHANNEL IS NOT CLOSED. It belongs to the extension layer: a resident
// extension is up for as long as termcp runs, and an on-demand one is reaped by its
// own idle clock. Closing it here would let any MCP client evict an instance
// another caller is using — the reason DELETE used to report success without doing
// anything.
func (ep *extEndpoint) UnregisterSession(ctx context.Context, sess mcplib.Session) {
	ep.mu.Lock()
	delete(ep.sessions, sess.SessionID())
	empty := len(ep.sessions) == 0
	cancel, done := ep.pumpCancel, ep.pumpDone
	if empty {
		ep.pumpCancel, ep.pumpDone = nil, nil
	}
	ep.mu.Unlock()
	if empty && cancel != nil {
		cancel()
		if done != nil {
			<-done
		}
	}
}

// Handle answers one inbound frame. It never returns nil for a REQUEST: a client
// that gets nothing back waits forever.
func (ep *extEndpoint) Handle(ctx context.Context, raw []byte, sess mcplib.Session) []byte {
	frame := strings.TrimSpace(string(raw))
	if frame == "" {
		return jsonRPCError(nil, mcplib.CodeParseError, "empty frame")
	}
	if isBatch([]byte(frame)) {
		// 2025-06-18 has no batch, and this endpoint's fan-out has no per-element
		// routing. Refusing says so; forwarding it as one notification would hand
		// the client an array it never asked for.
		return jsonRPCError(nil, mcplib.CodeInvalidRequest,
			"batch requests are not supported by this endpoint (protocol 2025-06-18 has no batch form)")
	}
	fields, ok := parseFrame([]byte(frame))
	if !ok {
		return jsonRPCError(nil, mcplib.CodeParseError, "the frame is not a JSON-RPC message")
	}

	if fields.method == "initialize" {
		ep.noteCapabilities(sess.SessionID(), raw)
	}
	if fields.method == "notifications/initialized" {
		// The peer is one process behind many clients: it is told once that the
		// handshake is done, and every later client's notification is absorbed here.
		ep.initMu.Lock()
		already := ep.initialized
		ep.initialized = true
		ep.initMu.Unlock()
		if already {
			return nil
		}
	}

	ch, err := ep.ensureChannel(ctx)
	if err != nil {
		if len(fields.id) == 0 {
			return nil
		}
		return jsonRPCError(fields.id, mcplib.CodeInternalError, "extension "+ep.name+": "+err.Error())
	}

	// A frame with an id and no method answers something. If it answers a request
	// the PEER made, it goes back to the peer verbatim: that id was never rewritten.
	if fields.method == "" {
		if len(fields.id) == 0 {
			return nil
		}
		if _, ok := ep.peerOutstanding.LoadAndDelete(string(fields.id)); ok {
			if err := ch.Write(ctx, frame); err != nil {
				return jsonRPCError(fields.id, mcplib.CodeInternalError, err.Error())
			}
			return nil
		}
		// An answer nobody here is waiting for. Forwarding it keeps the peer's
		// state honest; there is no reply to it, because it IS a reply.
		_ = ch.Write(ctx, frame)
		return nil
	}

	// initialize, from here on, may be answered from the cache: the peer is one
	// process, so its serverInfo and capabilities do not change per client.
	if fields.method == "initialize" {
		if cached, ok := ep.cachedInitialize(fields.id); ok {
			return cached
		}
	}

	if len(fields.id) == 0 {
		// A notification: no answer, and no id to rewrite. It is still noted as ours,
		// because a pty echoes it back and a client must not receive its own frame.
		ep.noteSent(frame)
		if err := ch.Write(ctx, frame); err != nil {
			log.Printf("mcp: ext %s: cannot deliver notification: %v", ep.name, err)
		}
		return nil
	}

	upstream := ep.nextUpstreamID()
	// THE KEY IS THE ID AS THE FRAME SPELLS IT, quotes included: the reply will be
	// routed by the same spelling (`"ext-1"`), and a table keyed `ext-1` would never
	// match a frame whose id member is a string.
	upstreamKey := `"` + upstream + `"`
	rewritten, ok := rewriteFrameID([]byte(frame), []byte(upstreamKey))
	if !ok {
		return jsonRPCError(fields.id, mcplib.CodeInvalidRequest, "the frame has no id member to route by")
	}
	entry := &extPending{
		ch:         make(chan []byte, 1),
		sessionID:  sess.SessionID(),
		originalID: append(json.RawMessage(nil), fields.id...),
	}
	// REGISTERED BEFORE THE WRITE, because the pump is already running: a reply can
	// land before this line returns.
	ep.pending.Store(upstreamKey, entry)
	ep.noteSent(string(rewritten))

	if err := ch.Write(ctx, string(rewritten)); err != nil {
		ep.pending.Delete(upstreamKey)
		return jsonRPCError(fields.id, mcplib.CodeInternalError, err.Error())
	}

	if fields.method == "initialize" {
		select {
		case reply := <-entry.ch:
			ep.cacheInitialize(reply)
			return reply
		case <-ctx.Done():
			ep.pending.Delete(upstreamKey)
			return jsonRPCError(fields.id, mcplib.CodeInternalError, "the client went away before the extension answered initialize")
		case <-time.After(extReplyWindow):
			ep.pending.Delete(upstreamKey)
			return jsonRPCError(fields.id, mcplib.CodeInternalError,
				"the extension did not answer initialize within "+extReplyWindow.String())
		}
	}

	select {
	case reply := <-entry.ch:
		return reply
	case <-ctx.Done():
		// THE REQUEST IS CANCELLED, NOT THE WORK: the pending entry goes, so a late
		// answer falls through to the GET stream instead of being dropped, and the
		// peer is not written to again.
		ep.pending.Delete(upstreamKey)
		return jsonRPCError(fields.id, mcplib.CodeInternalError, "the client cancelled the request")
	case <-time.After(extReplyWindow):
		ep.pending.Delete(upstreamKey)
		// NOT A 202. The spec does not allow "accepted" for a REQUEST, and a client
		// told 202 goes to its GET stream and waits for a frame that may never come.
		return jsonRPCError(fields.id, mcplib.CodeInternalError,
			"the extension did not answer within "+extReplyWindow.String()+
				"; the request is still outstanding and its answer, if it arrives, will be sent on this session's stream")
	}
}

// extReplyWindow bounds one forwarded request. It is the window the hand-written
// endpoint used, kept because a real tools/call over stdio routinely takes longer
// than an HTTP client's patience.
//
// A var rather than a const so a test can shorten it: the alternative is a test
// that sleeps a minute to prove a timeout, which nobody runs.
var extReplyWindow = 60 * time.Second

// noteCapabilities records what a client declared, which is what decides whether a
// peer-initiated request can be handed to it.
func (ep *extEndpoint) noteCapabilities(sessionID string, raw []byte) {
	var probe struct {
		Params struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return
	}
	ep.mu.Lock()
	if s, ok := ep.sessions[sessionID]; ok {
		s.caps = probe.Params.Capabilities
	}
	ep.mu.Unlock()
}

func (ep *extEndpoint) cachedInitialize(id json.RawMessage) ([]byte, bool) {
	ep.initMu.Lock()
	cached := ep.initReply
	ep.initMu.Unlock()
	if len(cached) == 0 {
		return nil, false
	}
	rewritten, ok := rewriteFrameID(cached, id)
	if !ok {
		return nil, false
	}
	return rewritten, true
}

func (ep *extEndpoint) cacheInitialize(reply []byte) {
	var probe struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	version := ""
	if err := json.Unmarshal(reply, &probe); err == nil {
		version = probe.Result.ProtocolVersion
	}
	ep.initMu.Lock()
	ep.initReply = append([]byte(nil), reply...)
	ep.initSent = true
	if version != "" {
		ep.protoVer = version
	}
	ep.initMu.Unlock()
}

func (ep *extEndpoint) nextUpstreamID() string {
	ep.mu.Lock()
	ep.upstreamSeq++
	n := ep.upstreamSeq
	ep.mu.Unlock()
	// A string id, because a client's ids are usually numbers and the two spellings
	// must never collide with a frame the client itself sent.
	return fmt.Sprintf("ext-%d", n)
}

// noteSent remembers a frame this endpoint typed, so its echo can be recognised.
func (ep *extEndpoint) noteSent(frame string) {
	now := time.Now()
	ep.sentMu.Lock()
	defer ep.sentMu.Unlock()
	trimmed := make([]sentFrame, 0, len(ep.sent)+1)
	for _, s := range ep.sent {
		if now.Sub(s.at) < extSentMemory {
			trimmed = append(trimmed, s)
		}
	}
	ep.sent = append(trimmed, sentFrame{body: frame, at: now})
}

// takeSent reports whether frame is the echo of something this endpoint wrote, and
// forgets it if so.
//
// A PTY ECHOES INPUT BEFORE THE PROCESS SEES IT, so an echoed request carries both
// an id and a method and is indistinguishable from a request the peer made. Without
// this, every request would come back as a duplicate peer-initiated request and
// every reply would be preceded by a copy of its own question.
func (ep *extEndpoint) takeSent(frame string) bool {
	ep.sentMu.Lock()
	defer ep.sentMu.Unlock()
	for i, s := range ep.sent {
		if s.body == frame {
			ep.sent = append(ep.sent[:i], ep.sent[i+1:]...)
			return true
		}
	}
	return false
}

// ensureChannel returns the live channel, bringing the extension up when the
// cached one is gone.
//
// A CHANNEL THAT ENDED CLEARS THE INITIALIZE CACHE: a restarted process has not
// been initialized, and replaying the previous serverInfo to a new client would be
// this endpoint inventing an answer on the peer's behalf.
func (ep *extEndpoint) ensureChannel(ctx context.Context) (extChannel, error) {
	ep.mu.Lock()
	ch := ep.channel
	ep.mu.Unlock()
	if ch != nil && ch.Alive() {
		return ch, nil
	}

	ep.mu.Lock()
	ep.channel = nil
	ep.channelShellID = ""
	ep.channelEnded = ""
	ep.mu.Unlock()
	ep.initMu.Lock()
	ep.initReply = nil
	ep.initSent = false
	ep.initialized = false
	ep.protoVer = ""
	ep.initMu.Unlock()

	fresh, err := ep.newChannel(ctx)
	if err != nil {
		return nil, err
	}
	ep.mu.Lock()
	ep.channel = fresh
	ep.channelShellID = ep.lastShellID
	ep.mu.Unlock()
	// THE PUMP STARTS WITH THE FIRST SESSION, not with the first POST that needs a
	// reply: an extension can emit an event between two calls, and a reader started
	// later would never see it. The reader's own position is the current end of the
	// stream, so nothing that arrived before this moment is replayed.
	ep.startPump(fresh)
	return fresh, nil
}

// openShellChannel brings the extension up and returns a channel on it.
func (ep *extEndpoint) openShellChannel(ctx context.Context) (extChannel, error) {
	if ep.srv.extensions == nil {
		return nil, fmt.Errorf("extensions are not available on this instance")
	}
	st, err := ep.srv.extensions.Acquire(ep.name, extMCPStartWait)
	if err != nil {
		return nil, err
	}
	if st.ShellID == "" {
		// A template with no channel (a read-only file window, a forwards-only
		// tunnel) has no MCP server to talk to. Naming which shape it is beats "not
		// found", which would send the caller looking for a typo.
		return nil, fmt.Errorf("extension %s has no channel, so it has no MCP server to talk to: "+
			"its template declares no command. Add --command (or --url) to give it one", ep.name)
	}
	ep.mu.Lock()
	ep.lastShellID = st.ShellID
	ep.parentSession = st.SessionID
	ep.mu.Unlock()
	return &shellExtChannel{srv: ep.srv, shellID: st.ShellID, parentSession: st.SessionID}, nil
}

// startPump launches the reader if it is not already running.
func (ep *extEndpoint) startPump(ch extChannel) {
	ep.mu.Lock()
	if ep.pumpCancel != nil {
		ep.mu.Unlock()
		return
	}
	pumpCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ep.pumpCancel, ep.pumpDone = cancel, done
	ep.mu.Unlock()

	go func() {
		defer close(done)
		// THE SLOT IS RELEASED BEFORE THE PUMP STOPS COUNTING AS RUNNING, so a channel
		// that ended can be replaced by the next request. endChannel has already
		// cleared the channel; this clears the goroutine's own bookkeeping.
		defer func() {
			ep.mu.Lock()
			// The done channel identifies this run: a replacement pump may already
			// own the slot, and clearing it here would strand that one.
			if ep.pumpDone == done {
				ep.pumpCancel, ep.pumpDone = nil, nil
			}
			ep.mu.Unlock()
		}()
		ep.pump(pumpCtx, ch)
	}()
}

// pump reads the channel forever, splitting it into frames and routing each one.
func (ep *extEndpoint) pump(ctx context.Context, ch extChannel) {
	reader, err := ch.Open(ctx)
	if err != nil {
		ep.endChannel(err.Error())
		return
	}
	defer reader.Close()

	var carry []byte
	for ctx.Err() == nil {
		// THE IDLE CLOCK IS RENEWED HERE, because a subscriber reading frames is a
		// caller using the extension. Without it the reaper closes the channel
		// underneath an open GET stream (guard.go is the other renewal point, and it
		// only fires on a tool call).
		ch.Touch()
		chunk, err := reader.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				ep.endChannel(err.Error())
			}
			return
		}
		if len(chunk) == 0 {
			continue
		}
		carry = append(carry, chunk...)
		for {
			frame, consume, needMore := nextFrame(carry)
			if consume > 0 {
				carry = carry[consume:]
			}
			if needMore {
				break
			}
			if frame == nil {
				break
			}
			ep.route(ctx, ch, frame)
		}
		if len(carry) > extPumpCarryLimit {
			log.Printf("mcp: ext %s: dropping %d bytes that never became a frame", ep.name, len(carry))
			carry = nil
		}
	}
}

// route sends one frame where it belongs.
func (ep *extEndpoint) route(ctx context.Context, ch extChannel, frame []byte) {
	text := string(frame)
	if ep.takeSent(text) {
		// Our own line, echoed by a pty.
		return
	}
	fields, ok := parseFrame(frame)
	if !ok {
		return
	}

	switch {
	case len(fields.id) == 0 && fields.method != "":
		// A notification the extension raised on its own. This is the case the whole
		// file exists for.
		ep.fanOut(ctx, frame)

	case len(fields.id) > 0 && fields.method == "":
		// An answer. If a POST is waiting for it, that is where it goes; otherwise
		// it rides the session's stream, which the spec allows and is the only way a
		// late answer is deliverable at all.
		key := string(fields.id)
		if v, ok := ep.pending.Load(key); ok {
			entry := v.(*extPending)
			restored, _ := rewriteFrameID(frame, entry.originalID)
			select {
			case entry.ch <- restored:
				return
			default:
				// The waiter is gone; fall through to the stream.
			}
			ep.deliver(ctx, entry.sessionID, restored)
			return
		}
		ep.fanOut(ctx, frame)

	default:
		// A request the PEER initiated. It needs a client that can answer it, and
		// the id must survive untouched so the answer comes back recognisable.
		ep.forwardPeerRequest(ctx, ch, frame, string(fields.id))
	}
}

// fanOut sends a frame to every attached session.
//
// LOSSY BY DESIGN, and the contract says so: each session's queue is 64 frames and
// a client that does not read drops them. A reliable event stream needs an
// acknowledgement protocol, which is a different feature; an extension that cares
// puts a sequence number in its event and the agent calls a tool to catch up.
func (ep *extEndpoint) fanOut(ctx context.Context, frame []byte) {
	ep.mu.Lock()
	sessions := make([]*extSession, 0, len(ep.sessions))
	for _, s := range ep.sessions {
		sessions = append(sessions, s)
	}
	ep.mu.Unlock()
	for _, s := range sessions {
		ep.notifyRaw(ctx, s, frame)
	}
}

func (ep *extEndpoint) notifyRaw(ctx context.Context, s *extSession, frame []byte) {
	notifier, ok := s.sess.(interface {
		NotifyRaw(context.Context, []byte) error
	})
	if !ok {
		return
	}
	if err := notifier.NotifyRaw(ctx, frame); err != nil {
		log.Printf("mcp: ext %s: session %s could not take a frame: %v", ep.name, s.sess.SessionID(), err)
	}
}

func (ep *extEndpoint) deliver(ctx context.Context, sessionID string, frame []byte) {
	ep.mu.Lock()
	s := ep.sessions[sessionID]
	ep.mu.Unlock()
	if s != nil {
		ep.notifyRaw(ctx, s, frame)
	}
}

// forwardPeerRequest hands a peer-initiated request to a client that declared
// capabilities, or answers the peer with an error.
//
// SILENCE IS NOT AN OPTION. A peer that asked and heard nothing waits forever, and
// the reason it heard nothing — no client can answer this — is a fact it can act
// on.
func (ep *extEndpoint) forwardPeerRequest(ctx context.Context, ch extChannel, frame []byte, id string) {
	ep.peerOutstanding.Store(id, struct{}{})
	ep.mu.Lock()
	var target *extSession
	for _, s := range ep.sessions {
		if len(s.caps) > 0 {
			target = s
			break
		}
	}
	ep.mu.Unlock()
	if target != nil {
		ep.notifyRaw(ctx, target, frame)
		return
	}
	ep.peerOutstanding.Delete(id)
	errFrame := jsonRPCError(json.RawMessage(id), mcplib.CodeMethodNotFound,
		"no connected client declared the capability to answer this request")
	if err := ch.Write(ctx, string(errFrame)); err != nil {
		log.Printf("mcp: ext %s: cannot tell the peer that nobody can answer id %s: %v", ep.name, id, err)
	}
}

// endChannel records why the pump stopped and fails everything waiting on it, so a
// client gets a sentence rather than a hang.
func (ep *extEndpoint) endChannel(reason string) {
	ep.mu.Lock()
	ep.channelEnded = reason
	ep.channel = nil
	ep.channelShellID = ""
	ep.mu.Unlock()
	ep.initMu.Lock()
	ep.initReply = nil
	ep.initSent = false
	ep.initialized = false
	ep.initMu.Unlock()

	ep.pending.Range(func(k, v any) bool {
		entry := v.(*extPending)
		ep.pending.Delete(k)
		select {
		case entry.ch <- jsonRPCError(entry.originalID, mcplib.CodeInternalError,
			"extension "+ep.name+" stopped: "+reason):
		default:
		}
		return true
	})
}
