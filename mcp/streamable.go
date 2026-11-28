package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Streamable HTTP: one URL, four verbs.
//
//	POST   a JSON-RPC frame in, its answer out — as application/json when there is
//	       one answer, as text/event-stream when there are several.
//	GET    a server-initiated stream (notifications) for this session.
//	DELETE terminate the session.
//	other  405.
//
// The session is a header: initialize's answer carries Mcp-Session-Id, and every
// later request echoes it. That is what lets a stateless-looking endpoint keep
// per-client state without a cookie.

const (
	// sessionHeader carries the session id in both directions.
	sessionHeader = "Mcp-Session-Id"
	// versionHeader carries the protocol version a client settled on.
	versionHeader = "MCP-Protocol-Version"
	// DefaultStreamPath is where this transport is normally mounted.
	DefaultStreamPath = "/stream"
)

// streamClient is one client of the streamable transport. Unlike the SSE one it
// may have no open GET at all: a POST-only client is valid, and it simply cannot
// receive a server-initiated request. Saying so is better than blocking.
type streamClient struct {
	*client

	// out is the GET stream, once one is open. An atomic pointer rather than a
	// mutex: it is one field, and every operation is "read it", "install it if
	// empty", or "clear it if it is still mine" — each of which is a single atomic
	// step, so there is nothing for a lock to keep consistent across.
	out atomic.Pointer[streamOut]

	// lastSeen is unix nanos of the last request seen on this session. It is what the
	// reaper measures (see idle.go), and it lives here rather than on *client because
	// the SSE transport has its own liveness rule: its GET IS the session.
	lastSeen atomic.Int64

	// done is closed when the session ends, and it exists because closing a session used
	// to leave its GET stream OPEN: the stream loop waits on the out channel, a closed
	// session only clears the pointer and marks the client, and neither of those wakes a
	// reader parked on the old channel. The keep-alive tick then wrote to a live
	// connection, so nothing failed and nothing ended — the subscriber sat on a stream
	// that could never carry another frame, which is the failure with no symptom.
	done     chan struct{}
	closeOne sync.Once
}

// streamOut is the channel behind that pointer. Wrapped so the pointer identity is
// the stream's identity: closeStream compares what it was handed with what is
// installed, which a bare channel cannot express safely.
type streamOut struct{ ch chan []byte }

func newStreamClient(id string) *streamClient {
	sc := &streamClient{done: make(chan struct{})}
	sc.client = &client{id: id, write: sc.write}
	return sc
}

func (sc *streamClient) write(ctx context.Context, frame []byte) error {
	p := sc.out.Load()
	open := p != nil && !sc.client.closed.Load()
	if !open {
		return fmt.Errorf("mcpv1: session %s has no open stream", sc.SessionID())
	}
	out := p.ch
	select {
	case out <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("mcpv1: session %s is not reading", sc.SessionID())
	}
}

// openStream attaches the GET stream, refusing a second one: two readers would
// split the frames between them and each would see half.
func (sc *streamClient) openStream() (chan []byte, error) {
	so := &streamOut{ch: make(chan []byte, 64)}
	if !sc.out.CompareAndSwap(nil, so) {
		return nil, fmt.Errorf("stream already open")
	}
	return so.ch, nil
}

func (sc *streamClient) closeStream(out chan []byte) {
	p := sc.out.Load()
	if p != nil && p.ch == out {
		sc.out.CompareAndSwap(p, nil)
	}
}

// close ends the session and wakes anyone waiting on it — a pending call, and an
// attached GET stream, which is the half that used to keep running.
//
// ONCE: the idle reaper and a DELETE can reach the same session, and the reaper drops it
// from the map first, so this is not a second identity but a second report of the same
// end. sync.Once is what keeps close(done) from panicking on the duplicate.
func (sc *streamClient) close() {
	sc.client.closeAll()
	sc.out.Store(nil)
	sc.closeOne.Do(func() { close(sc.done) })
}

// Dispatcher is everything a transport needs from the thing it serves: three calls,
// and no knowledge of what answers them.
//
// WHY AN INTERFACE AND NOT *Server (issue #328). The transport is one implementation of
// the protocol's HTTP half — session ids, GET streams, the version header, body limits,
// 405/400/413 — and there used to be a second, hand-rolled copy of that half in the
// extension endpoint. Two copies of a protocol surface drift: the copy answered 202 to a
// request (which the spec forbids), 405 to the GET stream (which a client needs), and
// invented no session id. Naming the three methods the transport actually uses is what
// makes the second copy unnecessary.
type Dispatcher interface {
	// RegisterSession records a session that just began.
	RegisterSession(ctx context.Context, sess Session)
	// UnregisterSession drops one that ended.
	UnregisterSession(ctx context.Context, sess Session)
	// Handle answers one inbound frame. A notification returns nil.
	Handle(ctx context.Context, raw []byte, sess Session) []byte
}

// StreamableHTTPServer is the streamable-HTTP transport.
type StreamableHTTPServer struct {
	srv Dispatcher
	// contextFunc lifts the HTTP request into handler context.
	contextFunc func(context.Context, *http.Request) context.Context
	// endpoint is the path this handler is mounted at, used for the GET stream's
	// advertised location. Empty means "the request's own path".
	endpoint string

	// SAME SHAPE AS THE SSE TRANSPORT: one session per key, one boolean for the
	// shutdown flag, no invariant spanning them (a session created as the server
	// closes is refused by the flag check before the store).
	sessions sync.Map // sessionID string -> *streamClient
	closed   atomic.Bool

	// speaks is the version this transport answers with, when the thing behind it is not
	// this library's own server.
	//
	// AN EXTENSION'S PEER HAS ITS OWN VERSION (issue #328). Writing 2025-06-18 over an
	// extension that negotiated something else is the transport lying about the only thing
	// a client uses to decide which frames are legal.
	speaks func() string
	// idleWindow is how long a session may go untouched before it is reclaimed. Zero
	// means defaultIdleWindow.
	idleWindow time.Duration
	// stopJanitor ends the reaper started by NewStreamableHTTPServer.
	stopJanitor context.CancelFunc
	janitorDone chan struct{}
}

// StreamableOption customises the streamable transport.
type StreamableOption func(*StreamableHTTPServer)

// WithProtocolVersion makes the transport answer with the version the thing behind it
// actually speaks, instead of this library's own.
func WithProtocolVersion(fn func() string) StreamableOption {
	return func(s *StreamableHTTPServer) { s.speaks = fn }
}

// spokenVersion is the version to answer with.
func (s *StreamableHTTPServer) spokenVersion() string {
	if s.speaks != nil {
		if v := s.speaks(); v != "" {
			return v
		}
	}
	return ProtocolVersion
}

// WithHTTPContextFunc sets the function that lifts an HTTP request into handler
// context.
func WithHTTPContextFunc(f func(context.Context, *http.Request) context.Context) StreamableOption {
	return func(s *StreamableHTTPServer) { s.contextFunc = f }
}

// WithStreamableIdleWindow sets how long a session may go untouched before the
// transport reclaims it. A value of zero (or a negative one) means the default.
func WithStreamableIdleWindow(d time.Duration) StreamableOption {
	return func(s *StreamableHTTPServer) { s.idleWindow = d }
}

// NewStreamableHTTPServer builds the streamable-HTTP transport.
func NewStreamableHTTPServer(srv Dispatcher, opts ...StreamableOption) *StreamableHTTPServer {
	s := &StreamableHTTPServer{srv: srv, janitorDone: make(chan struct{})}
	for _, opt := range opts {
		opt(s)
	}
	s.startJanitor()
	return s
}

// Broadcast delivers a frame to every session that has an open GET stream, and reports how
// many took it.
//
// THIS IS THE SERVER-INITIATED HALF, and it is a broadcast rather than a per-session send
// on purpose (issue #328). A shared stdio peer has no notion of which client a notification
// is for: it says `notifications/acme/job` and the line-delimited channel carries it to
// whoever is reading. Fanning out is the honest mapping of that; stopping at one session
// would invent a routing rule the peer never expressed.
//
// DELIVERY IS LOSSY BY DESIGN: each session holds 64 frames and a client that is not
// reading drops them (write returns an error, which is counted as not delivered). The
// extension's own sequence numbers are what makes that recoverable — see the transport
// docs — and a queue that grows instead would turn one stuck client into a leak.
func (s *StreamableHTTPServer) Broadcast(frame []byte) int {
	var sessions []*streamClient
	s.sessions.Range(func(_, v any) bool {
		sessions = append(sessions, v.(*streamClient))
		return true
	})
	sent := 0
	for _, sc := range sessions {
		sc.touch()
		if err := sc.write(context.Background(), frame); err == nil {
			sent++
		}
	}
	return sent
}

// ServeHTTP dispatches on the verb.
func (s *StreamableHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *StreamableHTTPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r)
	}

	// Which session, if any. No header on initialize; required afterwards.
	var sess *streamClient
	if id := r.Header.Get(sessionHeader); id != "" {
		if v, ok := s.sessions.Load(id); ok {
			sess, _ = v.(*streamClient)
		}
		if sess == nil {
			// 404 is the spec's signal for "that session is gone, start over".
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	}

	// initialize is the only call that may arrive without a session, and it is
	// the one that creates one.
	if isInitializeFrame(body) && sess == nil {
		sess = newStreamClient(nextSessionID())
		if s.closed.Load() {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		s.sessions.Store(sess.id, sess)
		sess.touch()
		w.Header().Set(sessionHeader, sess.id)
		s.srv.RegisterSession(ctx, sess)
	}

	if sess == nil {
		http.Error(w, "missing "+sessionHeader, http.StatusBadRequest)
		return
	}
	sess.touch()

	// MCP-Protocol-Version IS READ, NOT ONLY WRITTEN (issue #327). This transport wrote
	// the header on every answer and never looked at the one the client sent, so a
	// client speaking a version this server does not implement was served anyway and
	// the two sides interpreted the same frames differently — a failure that looks like
	// a handler bug from every angle.
	//
	// A MISSING HEADER IS NOT AN ERROR. The spec has clients omit it before the
	// handshake settles, and the handshake is where the version is actually agreed;
	// refusing here would break the clients that do the right thing.
	if v := strings.TrimSpace(r.Header.Get(versionHeader)); v != "" && v != ProtocolVersion {
		msg := "unsupported " + versionHeader + ": " + v + " (this instance speaks " + ProtocolVersion + ")"
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// A CALL THAT ASKED TO BE WATCHED ANSWERS AS A STREAM (see streamable_progress.go).
	// The token is read from the raw frame, because the content type has to be chosen
	// before dispatch runs.
	if token, ok := progressTokenOf(body); ok {
		s.serveWithProgress(ctx, w, sess, body, token)
		return
	}
	s.servePlain(ctx, w, sess, body)
}

func (s *StreamableHTTPServer) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionHeader)
	if id == "" {
		// No session, no stream. 400 names the missing thing rather than 405,
		// which would say the verb is wrong when it is the header.
		http.Error(w, "missing "+sessionHeader, http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var sess *streamClient
	if v, ok := s.sessions.Load(id); ok {
		sess, _ = v.(*streamClient)
	}
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	ctx := r.Context()
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r)
	}

	out, err := sess.openStream()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer sess.closeStream(out)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.done:
			// THE SESSION ENDED UNDER US. Returning closes the response, which is the only
			// signal this transport has for "start over": a client reconnects, the GET names
			// a session that no longer exists, and the 404 tells it to initialize again.
			return
		case <-keepAlive.C:
			if err := writeSSEComment(w, flusher); err != nil {
				return
			}
		case frame := <-out:
			if err := writeSSEFrame(w, flusher, "message", frame); err != nil {
				return
			}
		}
	}
}

func (s *StreamableHTTPServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionHeader)
	if id == "" {
		http.Error(w, "missing "+sessionHeader, http.StatusBadRequest)
		return
	}
	var sess *streamClient
	if v, ok := s.sessions.LoadAndDelete(id); ok {
		sess, _ = v.(*streamClient)
	}
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r)
	}
	sess.close()
	s.srv.UnregisterSession(ctx, sess)
	w.WriteHeader(http.StatusNoContent)
}

// Shutdown ends every session and refuses new ones. The ctx bounds the wait for
// in-flight handlers, which this transport does not track; it is accepted for
// symmetry with the SSE transport's signature.
func (s *StreamableHTTPServer) Shutdown(ctx context.Context) error {
	if s.stopJanitor != nil {
		s.stopJanitor()
		// WAIT FOR THE REAPER, because it may be inside UnregisterSession, whose hooks
		// belong to the caller. Returning while a hook is still running would let the
		// caller tear down state a hook is reading.
		<-s.janitorDone
	}
	s.closed.Store(true)
	var sessions []*streamClient
	s.sessions.Range(func(k, v any) bool {
		s.sessions.Delete(k)
		sessions = append(sessions, v.(*streamClient))
		return true
	})
	for _, sess := range sessions {
		sess.close()
		s.srv.UnregisterSession(ctx, sess)
	}
	return nil
}

// EndSessions ends every session this transport holds, and leaves the transport usable:
// the next initialize mints a new one.
//
// THIS IS "THE THING BEHIND THIS ENDPOINT IS GONE", which Shutdown cannot express — it
// also refuses new sessions, and here the endpoint recovers (an on-demand extension is
// restarted by the next request). A client that was attached finds its GET stream closed
// and its next request answered 404, which is the protocol's way of saying "initialize
// again"; without it a subscriber kept a stream that could never carry another frame.
func (s *StreamableHTTPServer) EndSessions(ctx context.Context) int {
	var sessions []*streamClient
	s.sessions.Range(func(k, v any) bool {
		s.sessions.Delete(k)
		sessions = append(sessions, v.(*streamClient))
		return true
	})
	for _, sess := range sessions {
		sess.close()
		s.srv.UnregisterSession(ctx, sess)
	}
	return len(sessions)
}

// isInitializeFrame reports whether a frame is an initialize request. It is
// cheap and lenient on purpose: the only consequence of a wrong answer is which
// error the client sees.
func isInitializeFrame(body []byte) bool {
	var probe struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Method == "initialize"
}

// nextSessionID mints a session id. Random rather than sequential: a sequential
// id is guessable, and a client that guessed one could attach to another's
// stream.
func nextSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on; a fallback that
		// is predictable is worse than a loud failure, so this panics into the
		// recovery middleware instead of handing out a colliding id.
		panic("mcpv1: cannot read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
