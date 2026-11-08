package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
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
}

func newStreamClient(id string) *streamClient {
	sc := &streamClient{}
	sc.client = &client{id: id, write: sc.write}
	return sc
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
}

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
		s.srv.registerSession(ctx, sess)
	}

	if sess == nil {
		http.Error(w, "missing "+sessionHeader, http.StatusBadRequest)
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

// Shutdown ends every session and refuses new ones. The ctx bounds the wait for
// in-flight handlers, which this transport does not track; it is accepted for
// symmetry with the SSE transport's signature.
func (s *StreamableHTTPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
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
