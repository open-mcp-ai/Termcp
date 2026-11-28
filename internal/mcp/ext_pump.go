package mcp

// The pump: one goroutine per extension channel that reads the peer's output and routes
// it (issue #328).
//
// WHY A READER IS NEEDED AT ALL. Until this file, the only thing that read an
// extension's channel was a request in flight: `mcpAwaitReply` waited for the id it had
// asked about, parked the frames that answered other ids, and gave up at the window. So a
// peer that spoke on its own — `notifications/acme/job`, a spec-legal custom method —
// produced bytes nobody was reading, and the client attached to /ext/{name}/stream, whose
// whole purpose is to receive them, received nothing. This is not MCP's
// `resources/subscribe`: the subscriber is a custom agent that consumes custom methods
// directly, and what it needs is "do not swallow the frame, deliver it".
//
// FOUR ROUTES, ONE PER SHAPE A LINE CAN HAVE:
//
//	method, no id      a notification → every session with an open GET stream
//	id, no method      a reply → the request waiting on that id (the id is the one WE
//	                   minted, so two clients may both say `id:1` without crossing)
//	id and method      a request the peer makes of us → answered with an error, because
//	                   nothing here accepts server-initiated requests
//	neither            a banner, an echo, a log line → skipped; the reader's cursor has
//	                   already moved past it, so nothing is lost that was ever addressed
//
// The reader is registered at "now" (RegisterReader starts at the current end), so a
// second client joining later does not get a replay of everything since the process
// started — and does not need one.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/mcp"
)

// mcpStreamable names the transport type once, so the fanout helper reads without a
// package qualifier on every line.
type mcpStreamable = mcp.StreamableHTTPServer

const (
	// extPumpReadWindow is how long one read waits for bytes before looking at the
	// context and reading again. It is an idle poll, not a deadline: a read returns as
	// soon as anything arrives.
	extPumpReadWindow = 30 * time.Second
	// extPumpMaxChunk bounds one read. A line longer than this is a peer that does not
	// speak line-delimited JSON-RPC, and the scanner drops what it cannot parse.
	extPumpMaxChunk = 8 << 20
	// extPumpAnswerWindow is how long a forwarded request waits for its reply.
	//
	// IT IS NOT A TOOL TIMEOUT (issue #328). The reason it exists is that a peer which never
	// answers must not pin a goroutine for ever; the reason it is this large is that a
	// legitimate `tools/call` may take a minute or two, and a window that cut it short would
	// turn a working tool into a protocol error. The client's own request deadline is the
	// authority on "too long" — this is the backstop for a client that has none, and it must
	// stay above a plausible tool call.
	extPumpAnswerWindow = 5 * time.Minute
)

// extPump is one extension channel's reader and its routing table.
type extPump struct {
	s     *Server
	shell string
	// name is the extension this channel belongs to, captured at start because the mapping
	// is exactly what stops existing when the channel ends — and ending it is when the name
	// is needed.
	name string
	// sessID is the connection the shell lives on, kept for Touch: the reconciler reaps
	// an idle extension, and a client that is attached and receiving pushes is not idle.
	sessID string

	// waiters is keyed by minted id, and NO INVARIANT SPANS TWO KEYS: a waiter is claimed
	// by exactly one reader, with LoadAndDelete deciding who got it. That is a sync.Map
	// rather than a lock over a map (AGENTS.md).
	waiters sync.Map // minted id string -> chan []byte
	// minted is a counter, so it is one atomic.
	minted atomic.Int64
	// init caches the peer's answer to initialize. N clients means one handshake: the
	// peer's serverInfo and capabilities are the same for all of them, and a second
	// handshake on a line-delimited stdio peer is a second state machine nobody needs.
	// `reply` is published as a whole value because "initialized" and "with what answer"
	// are one fact.
	init atomic.Pointer[pumpInit]
	// initSeen records that notifications/initialized was delivered to the peer once.
	initSeen atomic.Bool

	cancel context.CancelFunc
	done   chan struct{}
}

// extPumpFor returns the pump for a channel, starting it on first use.
func (s *Server) extPumpFor(shellID string) (*extPump, error) {
	if v, ok := s.extPumps.Load(shellID); ok {
		return v.(*extPump), nil
	}
	child := s.sessMgr.GetChildShell(shellID)
	if child == nil {
		return nil, fmt.Errorf("channel %s is not running", shellID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &extPump{
		s:      s,
		shell:  shellID,
		name:   s.extNameOfShell(shellID),
		sessID: child.ParentSessionID(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	actual, loaded := s.extPumps.LoadOrStore(shellID, p)
	if loaded {
		cancel()
		return actual.(*extPump), nil
	}
	go p.run(ctx)
	return p, nil
}

// stop ends the pump and wakes every waiter with the reason.
func (p *extPump) stop(reason string) {
	p.cancel()
	// LOADANDDELETE DECIDES WHO CLOSES: a waiter already claimed by route (which read and
	// removed it before sending the reply) must not be closed here, and closing a channel
	// twice panics.
	p.waiters.Range(func(k, _ any) bool {
		if v, ok := p.waiters.LoadAndDelete(k); ok {
			close(v.(chan []byte))
		}
		return true
	})
	p.s.extPumps.Delete(p.shell)
}

// run reads the channel for as long as it lives.
func (p *extPump) run(ctx context.Context) {
	defer close(p.done)
	defer p.s.extPumps.Delete(p.shell)
	// WHATEVER ENDS THIS CHANNEL ALSO ENDS THE SESSIONS ATTACHED TO IT. A subscriber's GET
	// stream is not a socket to the peer: it is a stream from this instance, so nothing
	// about the peer dying reaches it. Closing those sessions is the only "the thing behind
	// this endpoint is gone" a client can observe, and its next request answering 404 is
	// what makes it initialize again.
	defer func() {
		if n := p.s.closeExtSessions(p.name); n > 0 {
			slog.Info("extension endpoint ended", "name", p.name, "sessions", n)
		}
	}()
	child := p.s.sessMgr.GetChildShell(p.shell)
	if child == nil {
		return
	}
	// FROM NOW, NOT FROM THE START. A reader registered at the beginning would replay
	// every frame the peer ever emitted — including other clients' replies — and the first
	// client to attach would receive answers to questions it never asked.
	rid, err := child.RegisterReader()
	if err != nil {
		slog.Debug("ext pump: cannot register reader", "shell", p.shell, "err", err)
		return
	}
	// AN UNREGISTERED READER PINS THE BUFFER'S COMPACTION WATERMARK: the channel's output
	// would grow without bound for as long as the process runs.
	defer child.UnregisterReader(rid)

	var carry []byte
	for ctx.Err() == nil {
		if p.s.sessMgr.GetChildShell(p.shell) == nil {
			// The channel ended (the process exited, or the extension was restarted). A
			// client waiting on a reply gets told, instead of waiting for the window.
			p.failAll("the extension's channel ended")
			return
		}
		_, raw, _, rerr := child.ReadTerminalStream(ctx, rid, extPumpReadWindow, false, 0, extPumpMaxChunk, nil)
		if rerr != nil {
			if ctx.Err() != nil {
				return
			}
			// io.EOF from a closed channel is the normal end.
			p.failAll("reading the extension's channel: " + rerr.Error())
			return
		}
		if len(raw) == 0 {
			// A DEAD PROCESS IS NOT A QUIET ONE, and the read does not say which: once the
			// peer's pipe is closed a read returns nothing with no error, immediately and
			// for ever — which spun this loop and left the pump convinced the channel was
			// alive, so nothing downstream was ever told it had ended. Alive() is the
			// measurement of the process rather than the published status (internal/session);
			// IsBufferClosed is the other fact that means "no byte will ever arrive again" —
			// a sealed transcript, which outlives a process that is still there.
			//
			// ponytail: a transcript that STOPPED (a failed append — "stopping transcript")
			// leaves the buffer open and the process alive with no writer, and nothing
			// observable says so. This loop then waits the window for ever without ending
			// the subscriptions. A writer-gone flag on ChildShell is the fix when a case
			// needs it; exit and seal are what this covers, and they are what happens.
			if !child.Alive() || child.IsBufferClosed() {
				p.failAll("the extension's channel ended")
				return
			}
			continue
		}
		// A CLIENT ATTACHED TO THIS ENDPOINT IS USING THE EXTENSION (issue #183): without
		// this the idle sweep would reap the process out from under a subscriber between
		// two events.
		if p.s.extensions != nil {
			p.s.extensions.Touch(p.shell, p.sessID)
		}
		frames, rest := splitFrames(carry, raw)
		carry = rest
		for _, frame := range frames {
			p.route(ctx, frame)
		}
	}
}

// route sends one frame down the road its shape names.
func (p *extPump) route(ctx context.Context, frame []byte) {
	var env struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params json.RawMessage  `json:"params"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		return // a banner or a log line, not a message
	}
	hasID := env.ID != nil

	switch {
	case env.Method != "" && !hasID:
		// A NOTIFICATION IS THE POINT OF THIS FILE: it goes to every attached client,
		// whether or not anybody asked for it.
		if p.tr() != nil {
			p.tr().Broadcast(frame)
		}
		// AND TO WHOEVER SUBSCRIBED ON THE MAIN ENDPOINT (#275's narrow half), which is
		// the same event wrapped with the extension's name — the extension endpoint
		// cannot tell a client which extension spoke, because each endpoint serves one.
		p.s.publishExtEvent(ctx, p.name, env.Method, env.Params)
	case env.Method != "" && hasID:
		// THE PEER ASKS US SOMETHING. Nothing on this endpoint accepts server-initiated
		// requests (no client here declared sampling or elicitation), so answering with an
		// error is the honest reply — silently dropping it would leave the peer waiting for
		// a response that never comes, which is how a stdio server deadlocks.
		p.s.extWriteLine(ctx, p.shell, string(extErrorFrame(*env.ID, extCodePeer,
			"this endpoint does not accept server-initiated requests: attach with `call`/`recv` on the "+
				"directive surface, where a request from the extension can be answered")))
	case hasID:
		// THE KEY IS THE RAW SPELLING, quotes and all: `"1"` and `1` are different ids, and
		// normalising here would answer a client whose id merely looked similar.
		id := mintedKey(strings.TrimSpace(string(*env.ID)))
		if v, ok := p.waiters.LoadAndDelete(id); ok {
			ch := v.(chan []byte)
			// THE BYTES PASS THROUGH; ONLY THE ID IS PUT BACK. A reply body is the
			// extension's answer, and the client compares it by the id it sent.
			ch <- frame
		}
	}
}

// tr returns the transport serving this channel, for fanout.
func (p *extPump) tr() *mcpStreamable {
	return p.s.extTransportOfChannel(p.shell)
}

// closeExtSessions ends every session of an extension's transport, and forgets the channel
// it was serving.
//
// BY NAME, NOT BY CHANNEL: extTransportOfChannel asks GetChildShell first, which is nil in
// exactly the case that needs this. It LOADS rather than calls extTransport, because
// creating a transport in order to end its (empty) sessions would leave one behind for
// every extension that ever died.
func (s *Server) closeExtSessions(name string) int {
	if name == "" {
		return 0
	}
	s.extShells.Delete(name)
	v, ok := s.extTransports.Load(name)
	if !ok {
		return 0
	}
	return v.(*mcpStreamable).EndSessions(context.Background())
}

// failAll wakes every waiter with a message instead of leaving them to time out.
func (p *extPump) failAll(reason string) {
	var waiters []struct {
		id string
		ch chan []byte
	}
	p.waiters.Range(func(k, v any) bool {
		if claimed, ok := p.waiters.LoadAndDelete(k); ok {
			waiters = append(waiters, struct {
				id string
				ch chan []byte
			}{k.(string), claimed.(chan []byte)})
		}
		return true
	})
	for _, w := range waiters {
		id, ch := w.id, w.ch
		// THE KEY IS THE UNQUOTED MINTED ID (`tc7`), and a JSON-RPC id has to be valid JSON:
		// handing the bare token to json.Marshal failed, and the client got the generic
		// "internal error" instead of the sentence saying what happened.
		ch <- extErrorFrame(json.RawMessage(`"`+id+`"`), extCodePeer, reason)
		close(ch)
	}
}

// ask forwards one request to the peer and waits for its reply.
//
// THE ID IS MINTED HERE, and that is what makes two clients on one extension independent:
// both may send `id:1`, each gets its own `tc<N>` on the wire, and each receives the reply
// to its own. Without it the first client to ask would take the other's answer.
func (p *extPump) ask(ctx context.Context, frame string) []byte {
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &env); err != nil {
		return extErrorFrame(nil, extCodeParse, "parse error: "+err.Error())
	}
	origID := strings.TrimSpace(string(env.ID))

	// INITIALIZE IS ANSWERED FROM THE CACHE AFTER THE FIRST ONE. The peer's identity does
	// not change between clients, and a second handshake against a stdio server is a
	// second session in a process that has one.
	if env.Method == "initialize" {
		if cached := p.init.Load(); cached != nil {
			return rewriteFrameID(cached.reply, origID)
		}
	}
	minted := fmt.Sprintf(`"tc%d"`, p.minted.Add(1))
	ch := make(chan []byte, 1)
	key := mintedKey(minted)
	p.waiters.Store(key, ch)

	if err := p.s.extWriteLine(ctx, p.shell, rewriteFrameIDRaw(frame, minted)); err != nil {
		p.forget(key)
		return extErrorFrame(json.RawMessage(origID), extCodePeer, "the extension's channel refused the frame: "+err.Error())
	}

	timer := time.NewTimer(extPumpAnswerWindow)
	defer timer.Stop()
	select {
	case reply, ok := <-ch:
		if !ok || len(reply) == 0 {
			return extErrorFrame(json.RawMessage(origID), extCodePeer, "the extension's channel ended before it answered")
		}
		out := rewriteFrameID(reply, origID)
		if env.Method == "initialize" {
			// WHAT THE PEER SAYS IT SPEAKS (issue #328): the transport answers with this
			// instead of its own constant, so a client negotiates against the version the
			// extension actually implements.
			p.s.noteExtVersion(p.s.extNameOfShell(p.shell), initializeVersion(reply))
			p.init.Store(&pumpInit{reply: out})
		}
		return out
	case <-timer.C:
		p.forget(key)
		return extErrorFrame(json.RawMessage(origID), extCodePeer, fmt.Sprintf(
			"the extension did not answer within %s. The frame was delivered — this is the window, not a "+
				"lost message — and a call that legitimately takes longer belongs on the directive surface "+
				"(`call <peer>`), where the caller sets the window", extPumpAnswerWindow))
	case <-ctx.Done():
		p.forget(key)
		return extErrorFrame(json.RawMessage(origID), extCodePeer, "the client went away before the extension answered")
	}
}

// note sends a notification to the peer, exactly once per method.
//
// a second client's notifications/initialized is dropped: the peer finished initializing
// on the first one, and sending it again is a second state transition in a process that
// has already made it.
func (p *extPump) note(ctx context.Context, frame, method string) []byte {
	// ONE DELIVERY, WHOEVER WINS THE RACE: CompareAndSwap is what makes the second
	// client's notifications/initialized a no-op instead of a second state transition in
	// a process that has already made it.
	dup := false
	if method == "notifications/initialized" {
		dup = !p.initSeen.CompareAndSwap(false, true)
	}
	if dup {
		return nil
	}
	if err := p.s.extWriteLine(ctx, p.shell, frame); err != nil {
		return extErrorFrame(nil, extCodePeer, "the extension's channel refused the notification: "+err.Error())
	}
	return nil
}

func (p *extPump) forget(id string) {
	p.waiters.Delete(id)
}

// pumpInit is the cached handshake answer: the bytes to replay, rewritten per client.
type pumpInit struct{ reply []byte }

// splitFrames cuts raw output into complete lines, returning the leftover tail.
//
// A LINE IS THE UNIT because the channel is line-delimited: a JSON-RPC message ends with
// the newline the peer wrote, and a read may stop in the middle of one.
func splitFrames(carry, raw []byte) ([][]byte, []byte) {
	buf := make([]byte, 0, len(carry)+len(raw))
	buf = append(buf, carry...)
	buf = append(buf, raw...)
	var out [][]byte
	for {
		idx := indexByte(buf, '\n')
		if idx < 0 {
			return out, append([]byte(nil), buf...)
		}
		line := trimSpaceBytes(buf[:idx])
		if len(line) > 0 {
			out = append(out, append([]byte(nil), line...))
		}
		buf = buf[idx+1:]
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func trimSpaceBytes(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// rewriteFrameID puts the caller's id back into a reply, leaving every other byte alone.
func rewriteFrameID(frame []byte, id string) []byte {
	if id == "" {
		return frame
	}
	return []byte(rewriteFrameIDRaw(string(frame), id))
}

// rewriteFrameIDRaw replaces the top-level id of a frame with rawID (already quoted if it
// is a string).
//
// THE REST OF THE FRAME IS NOT RE-ENCODED. A reply may hold numbers the peer wrote in a
// form Go would print differently, and an answer that is not byte-identical to what the
// extension sent is no longer "the extension's answer".
func rewriteFrameIDRaw(frame, rawID string) string {
	idx := strings.Index(frame, `"id"`)
	if idx < 0 {
		return frame
	}
	rest := frame[idx+len(`"id"`):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return frame
	}
	tail := strings.TrimLeft(rest[colon+1:], " \t")
	if len(tail) == 0 {
		return frame
	}
	var end int
	if tail[0] == '"' {
		end = 1
		for end < len(tail) {
			if tail[end] == '\\' {
				end += 2
				continue
			}
			if tail[end] == '"' {
				end++
				break
			}
			end++
		}
	} else {
		end = 0
		for end < len(tail) && strings.IndexByte("0123456789-+.eE", tail[end]) >= 0 {
			end++
		}
	}
	if end == 0 || end > len(tail) {
		return frame
	}
	idStart := len(frame) - len(tail)
	return frame[:idStart] + rawID + frame[idStart+end:]
}

// channelForTransport maps a transport back to the channel it serves, for fanout.
//
// The transport is per extension, and an extension may declare several channels; the one
// this endpoint talks to is the same one extMCPChannelShell picks, so the pump asks the
// same question instead of keeping a second answer.
func (s *Server) extTransportOfChannel(shellID string) *mcpStreamable {
	if shellID == "" || s.sessMgr.GetChildShell(shellID) == nil {
		return nil
	}
	name := s.extNameOfShell(shellID)
	if name == "" {
		return nil
	}
	return s.extTransport(name)
}

// extNameOfShell finds the extension that owns a channel, by asking the reconciler.
func (s *Server) extNameOfShell(shellID string) string {
	if s.extensions == nil {
		return ""
	}
	return s.extensions.NameOfShell(shellID)
}

// mintedKey normalises an id as it appears on the wire (`"tc1"` and `tc1` name the same
// waiter, since we minted it ourselves).
func mintedKey(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), `"`)
}

// initializeVersion reads protocolVersion out of an initialize answer.
func initializeVersion(reply []byte) string {
	var env struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reply, &env); err != nil {
		return ""
	}
	return strings.TrimSpace(env.Result.ProtocolVersion)
}
