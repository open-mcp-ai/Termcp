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

	mu  sync.Mutex
	out chan []byte // nil until a GET opens
	// lastSeen is the last moment this session did anything, in Unix nanos.
	//
	// A SESSION HAS TO BE ABLE TO DIE WITHOUT BEING TOLD (#327). DELETE was the only
	// way out, so a client that crashed, was killed or lost its network kept its
	// session — and its fan-out slot, and its row in Connections() — for as long as
	// the process ran. Atomic rather than under mu because the touch happens on the
	// POST path, where taking this mutex would serialise every request against a GET
	// stream's bookkeeping.
	lastSeen atomic.Int64
	// streaming is true while a GET owns this session's out channel. The reaper
	// leaves such a session alone: an open stream is proof of life even if the
	// client has nothing to say.
	streaming atomic.Bool
}

func newStreamClient(id string) *streamClient {
	sc := &streamClient{}
	sc.client = &client{id: id, write: sc.write}
	sc.touch()
	return sc
}

// touch records activity, which is what the reaper measures.
func (sc *streamClient) touch() {
	sc.lastSeen.Store(time.Now().UnixNano())
}

// idleFor is how long this session has been silent.
func (sc *streamClient) idleFor(now time.Time) time.Duration {
	at := sc.lastSeen.Load()
	if at == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, at))
}

func (sc *streamClient) write(ctx context.Context, frame []byte) error {
	sc.mu.Lock()
	out := sc.out
	open := out != nil && !sc.client.closed.Load()
	sc.mu.Unlock()
	if !open {
		return fmt.Errorf("mcpv1: session %s has no open stream", sc.SessionID())
	}
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
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.out != nil {
		return nil, fmt.Errorf("stream already open")
	}
	sc.out = make(chan []byte, 64)
	return sc.out, nil
}

func (sc *streamClient) closeStream(out chan []byte) {
	sc.mu.Lock()
	if sc.out == out {
		sc.out = nil
	}
	sc.mu.Unlock()
}

// close ends the session and wakes anyone waiting on it.
func (sc *streamClient) close() {
	sc.client.closeAll()
	sc.mu.Lock()
	sc.out = nil
	sc.mu.Unlock()
}

// StreamableHTTPServer is the streamable-HTTP transport.
type StreamableHTTPServer struct {
	srv *Server
	// contextFunc lifts the HTTP request into handler context.
	contextFunc func(context.Context, *http.Request) context.Context
	// endpoint is the path this handler is mounted at, used for the GET stream's
	// advertised location. Empty means "the request's own path".
	endpoint string

	mu       sync.Mutex
	sessions map[string]*streamClient
	closed   bool
	// reaper runs the idle sweep. Started by the first session and stopped by
	// Shutdown; a transport nobody used costs no goroutine.
	reaperOnce sync.Once
	reaperStop chan struct{}
}

// sessionIdleTimeout is how long a silent session survives.
//
// AT LEAST TWICE THE KEEPALIVE, and the constant says so rather than hard-coding a
// number: a client whose GET stream is open receives a comment every 30s, so a
// window shorter than one keepalive would reap sessions that are working. Two
// keepalives is one missed tick of slack.
var sessionIdleTimeout = 2 * sseKeepAlive

// reaperInterval is how often the sweep runs. It is a fraction of the timeout so the
// reclaim happens close to the deadline without measuring every session constantly.
var reaperInterval = sseKeepAlive

// StreamableOption customises the streamable transport.
type StreamableOption func(*StreamableHTTPServer)

// WithHTTPContextFunc sets the function that lifts an HTTP request into handler
// context.
func WithHTTPContextFunc(f func(context.Context, *http.Request) context.Context) StreamableOption {
	return func(s *StreamableHTTPServer) { s.contextFunc = f }
}

// NewStreamableHTTPServer builds the streamable-HTTP transport.
func NewStreamableHTTPServer(srv *Server, opts ...StreamableOption) *StreamableHTTPServer {
	s := &StreamableHTTPServer{srv: srv, sessions: map[string]*streamClient{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

	// THE VERSION HEADER IS READ, NOT JUST WRITTEN (#327).
	//
	// A client that speaks a version this server does not know will interpret every
	// frame by different rules, and the symptom — a member it ignores, an error code
	// it misreads — never points back at the version. Refusing names the mismatch.
	//
	// ABSENT BEFORE initialize IS FINE: the handshake is where the version is
	// negotiated, so requiring it there would refuse the one request that establishes
	// it. Absent afterwards keeps working, because the header is optional in the spec
	// and a client that omits it has not claimed anything false.
	if v := strings.TrimSpace(r.Header.Get(versionHeader)); v != "" && !isInitializeFrame(body) && !s.acceptsVersion(v) {
		http.Error(w, "unsupported "+versionHeader+" "+v+": this instance speaks "+ProtocolVersion, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r)
	}

	// Which session, if any. No header on initialize; required afterwards.
	var sess *streamClient
	if id := r.Header.Get(sessionHeader); id != "" {
		s.mu.Lock()
		sess = s.sessions[id]
		s.mu.Unlock()
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
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		s.sessions[sess.id] = sess
		s.mu.Unlock()
		w.Header().Set(sessionHeader, sess.id)
		s.startReaper()
		s.srv.registerSession(ctx, sess)
	}

	if sess == nil {
		http.Error(w, "missing "+sessionHeader, http.StatusBadRequest)
		return
	}
	sess.touch()

	// A CALL THAT ASKED FOR PROGRESS GETS A STREAM (#327).
	//
	// The client sends `_meta.progressToken` and `Accept: text/event-stream`; the
	// request runs while the response is open, the tool's progress frames are written
	// into it, and the result follows. Without this a tool had nowhere to put a frame:
	// the response was one JSON document written at the end, and a notification goes
	// to the GET stream while the client is waiting on its POST.
	if token, ok := progressTokenOf(body); ok && acceptsEventStream(r) {
		s.answerWithStream(w, r, ctx, sess, body, token)
		return
	}

	answer := s.srv.Handle(WithSessionContext(ctx, sess), body, sess)
	if answer == nil {
		// A notification: accepted, nothing to say.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(versionHeader, ProtocolVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer)
}

// answerWithStream runs one request with a progress sink and writes the response as an
// event stream: progress frames as they are emitted, then the result.
//
// THE ORDER IS THE CONTRACT. A progress frame that arrives after the result is a client
// parsing "one more event" out of a finished response, so the result is written last
// and the stream ends there.
func (s *StreamableHTTPServer) answerWithStream(w http.ResponseWriter, r *http.Request,
	ctx context.Context, sess *streamClient, body []byte, token ProgressToken) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// A transport that cannot stream must not pretend: fall back to the JSON
		// answer, which is legal, and the client simply gets no progress.
		answer := s.srv.Handle(WithSessionContext(ctx, sess), body, sess)
		if answer == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(versionHeader, ProtocolVersion)
		_, _ = w.Write(answer)
		return
	}

	sink := &progressSink{}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set(versionHeader, ProtocolVersion)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// THE WORK RUNS IN ITS OWN GOROUTINE so this one can keep writing progress while it
	// runs. A synchronous call would write every frame at the end, which is the
	// behaviour this exists to replace.
	runCtx := WithProgressSink(WithSessionContext(ctx, sess), sink, token)
	done := make(chan []byte, 1)
	go func() {
		done <- s.srv.Handle(runCtx, body, sess)
	}()

	tick := time.NewTicker(progressFlushInterval)
	defer tick.Stop()
	for {
		select {
		case answer := <-done:
			for _, frame := range sink.drain() {
				if err := writeSSEFrame(w, flusher, "message", frame); err != nil {
					return
				}
			}
			if answer != nil {
				_ = writeSSEFrame(w, flusher, "message", answer)
			}
			return
		case <-ctx.Done():
			// The client hung up. The tool's own context is derived from ctx, so it
			// stops too; the sink drops anything emitted after this point.
			sink.drain()
			return
		case <-tick.C:
			for _, frame := range sink.peek() {
				if err := writeSSEFrame(w, flusher, "message", frame); err != nil {
					return
				}
			}
		}
	}
}

// progressFlushInterval is how often a running call's queued frames are written.
//
// A TICK RATHER THAN A WAKEUP: the sink is a slice, not a channel, so the writer never
// blocks. 50ms is faster than a human reads a progress bar and slow enough that a
// chatty tool does not turn into a syscall per frame.
var progressFlushInterval = 50 * time.Millisecond

// progressTokenOf reads `_meta.progressToken` out of a request body, without decoding
// the whole frame: the only member that matters is in one place, and an 8 MiB arguments
// object does not need to be parsed twice.
func progressTokenOf(body []byte) (ProgressToken, bool) {
	var probe struct {
		Params struct {
			Meta struct {
				Token json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false
	}
	if len(probe.Params.Meta.Token) == 0 || string(probe.Params.Meta.Token) == "null" {
		return nil, false
	}
	return ProgressToken(probe.Params.Meta.Token), true
}

// acceptsEventStream reports whether the client will take an event stream. An absent
// Accept header is NOT consent for a stream: a client that said nothing expects JSON,
// and the spec requires both to be accepted but not that the server guess.
func acceptsEventStream(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

// supportedVersions is what this transport speaks, oldest first.
//
// ONE ENTRY IS THE HONEST ANSWER TODAY, and the list exists so the refusal above can
// name it rather than repeating a literal. A second entry is only worth adding when
// the code has been checked against that version's rules, which is a different job
// from adding a string.
var supportedVersions = []string{ProtocolVersion}

// acceptsVersion reports whether a client's declared version is one this server
// interprets correctly.
func (s *StreamableHTTPServer) acceptsVersion(v string) bool {
	for _, ok := range supportedVersions {
		if v == ok {
			return true
		}
	}
	return false
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
	s.mu.Lock()
	sess := s.sessions[id]
	s.mu.Unlock()
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	ctx := r.Context()
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r)
	}
	sess.touch()

	out, err := sess.openStream()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// A GET IS PROOF OF LIFE: the reaper must not take a session whose stream is
	// open, however quiet the client is.
	sess.streaming.Store(true)
	defer func() {
		sess.streaming.Store(false)
		sess.closeStream(out)
	}()

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
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r)
	}
	sess.close()
	s.srv.unregisterSession(ctx, sess)
	w.WriteHeader(http.StatusNoContent)
}

// startReaper launches the idle sweep once.
func (s *StreamableHTTPServer) startReaper() {
	s.reaperOnce.Do(func() {
		s.reaperStop = make(chan struct{})
		go s.reapLoop(s.reaperStop)
	})
}

// reapLoop ends sessions nobody is using.
//
// A SESSION HAD NO WAY OUT BUT DELETE (#327), so a client that crashed kept its
// session, its 64-frame fan-out slot and its row in Connections() until the process
// stopped. A GET stream is proof of life and is left alone; so is anything that did
// something within the window.
func (s *StreamableHTTPServer) reapLoop(stop <-chan struct{}) {
	tick := time.NewTicker(reaperInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			s.reap(now)
		}
	}
}

// reap drops the sessions that have been silent past the window.
func (s *StreamableHTTPServer) reap(now time.Time) {
	var expired []*streamClient
	s.mu.Lock()
	for id, sess := range s.sessions {
		if sess.streaming.Load() {
			// An open GET is a live client even when it is saying nothing.
			continue
		}
		if sess.idleFor(now) > sessionIdleTimeout {
			delete(s.sessions, id)
			expired = append(expired, sess)
		}
	}
	s.mu.Unlock()

	for _, sess := range expired {
		// The context is background: the session is gone, and the hook that fires
		// must not inherit a request that already returned.
		sess.close()
		s.srv.unregisterSession(context.Background(), sess)
	}
}

// Shutdown ends every session and refuses new ones. The ctx bounds the wait for
// in-flight handlers, which this transport does not track; it is accepted for
// symmetry with the SSE transport's signature.
func (s *StreamableHTTPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.reaperStop != nil {
		select {
		case <-s.reaperStop:
		default:
			close(s.reaperStop)
		}
	}
	sessions := make([]*streamClient, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = map[string]*streamClient{}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
		s.srv.unregisterSession(ctx, sess)
	}
	return nil
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
