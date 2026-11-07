package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSE session: the client opens ONE long-lived GET and every answer rides back
// down it, while calls arrive on a separate POST.
//
// The split is why the endpoint event exists at all. A POST to an unknown URL
// would have nowhere to put its answer; the GET stream is the answer channel, and
// the first frame on it tells the client where to send questions.

const (
	// sseSessionParam names the session in a POST /message query string.
	sseSessionParam = "sessionId"
	// sseKeepAlive is the comment interval. A proxy that sees no bytes for long
	// enough closes the connection, and a client then has to reconnect and lose its
	// session; a comment is the cheapest thing that resets that timer.
	sseKeepAlive = 30 * time.Second
	// sseWriteTimeout bounds one frame write. A client that stopped reading must
	// not pin the goroutine holding the lock forever.
	sseWriteTimeout = 10 * time.Second
)

// newSSEClient builds the per-session state for a client whose only outbound
// path is the open GET stream. A frame that cannot be queued is an error rather
// than a silent drop: a caller that asked for something to be delivered should
// learn it was not.
func newSSEClient(id string, out chan []byte) *client {
	return &client{
		id: id,
		write: func(ctx context.Context, frame []byte) error {
			select {
			case out <- frame:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			default:
				return fmt.Errorf("mcpv1: sse session %s is not reading", id)
			}
		},
	}
}

// sseSessionEntry is one attached client: the shared per-session state plus the
// channel its GET stream reads from. The channel lives here rather than in
// *client because only this transport has one.
type sseSessionEntry struct {
	client *client
	out    chan []byte
}

// SSEServer is the legacy SSE transport: GET /sse plus POST /message.
type SSEServer struct {
	srv *Server
	// contextFunc lifts the HTTP request into the handler context, which is how a
	// per-request fact (the address reached, a namespace claim) reaches a handler.
	contextFunc func(context.Context, *http.Request) context.Context
	// basePath is where the endpoints are mounted, used to build the endpoint
	// event so a client behind a path prefix is told the right URL.
	basePath    string
	messagePath string

	mu       sync.Mutex
	sessions map[string]*sseSessionEntry
	closed   bool
}

// SSEOption customises the SSE transport.
type SSEOption func(*SSEServer)

// WithSSEContextFunc sets the function that lifts an HTTP request into handler
// context.
func WithSSEContextFunc(f func(context.Context, *http.Request) context.Context) SSEOption {
	return func(s *SSEServer) { s.contextFunc = f }
}

// WithSSEBasePath sets the prefix the endpoint event advertises, for a deployment
// served under a path.
func WithSSEBasePath(p string) SSEOption {
	return func(s *SSEServer) { s.basePath = strings.TrimSuffix(p, "/") }
}

// NewSSEServer builds the legacy SSE transport.
func NewSSEServer(srv *Server, opts ...SSEOption) *SSEServer {
	s := &SSEServer{
		srv:         srv,
		messagePath: "/message",
		sessions:    map[string]*sseSessionEntry{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SSEHandler is the long-lived GET stream.
func (s *SSEServer) SSEHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both endpoints are mounted on one mux in practice, and a mux routes by
		// path only. Saying which verb is wrong here turns a silent "the stream
		// opened and nothing happened" into a sentence.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		out := make(chan []byte, 64)
		sess := newSSEClient(nextSessionID(), out)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		s.sessions[sess.SessionID()] = &sseSessionEntry{client: sess, out: out}
		s.mu.Unlock()

		ctx := r.Context()
		if s.contextFunc != nil {
			ctx = s.contextFunc(ctx, r)
		}
		// Register AFTER the handler owns the stream, so a hook that inspects the
		// session sees one that can already accept a notification.
		s.srv.registerSession(ctx, sess)

		defer func() {
			s.mu.Lock()
			delete(s.sessions, sess.SessionID())
			s.mu.Unlock()
			s.srv.unregisterSession(ctx, sess)
		}()

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("Connection", "keep-alive")
		// Tell nginx and friends not to buffer: a buffered SSE stream delivers
		// every frame at once when the response ends, which for a long-lived stream
		// means never.
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// The endpoint event, first and only once.
		endpoint := fmt.Sprintf("%s%s?%s=%s", s.basePath, s.messagePath, sseSessionParam, sess.SessionID())
		if err := writeSSEFrame(w, flusher, "endpoint", []byte(endpoint)); err != nil {
			return
		}

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
	})
}

// MessageHandler accepts a JSON-RPC frame and hands the answer to the GET stream.
//
// It answers 202 with no body: the answer is NOT this response. A client that
// expected one here would read the 202 as an empty result.
func (s *SSEServer) MessageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get(sseSessionParam)
		if id == "" {
			http.Error(w, "missing "+sseSessionParam, http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		entry, ok := s.sessions[id]
		s.mu.Unlock()
		if !ok {
			// 404 is what makes a client re-open its stream, and the alternative
			// (accepting the frame) would drop it with a 202 that lied.
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		body, err := readLimitedBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		if s.contextFunc != nil {
			ctx = s.contextFunc(ctx, r)
		}
		if answer := s.srv.Handle(WithSessionContext(ctx, entry.client), body, entry.client); answer != nil {
			select {
			case entry.out <- answer:
			case <-ctx.Done():
			default:
				http.Error(w, "session not reading", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

// Shutdown ends every session and refuses new ones.
func (s *SSEServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	sessions := make([]*sseSessionEntry, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	// Closing the answer channel would race with a producer; the streams end
	// through ctx, which is the GET request's own deadline. Waiting here is what
	// makes Shutdown mean "the sessions are gone", not "a flag is set".
	_ = sessions
	<-ctx.Done()
	return nil
}

// writeSSEFrame writes one `event:`/`data:` frame and flushes it.
func writeSSEFrame(w http.ResponseWriter, f http.Flusher, event string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	// A data payload is one line per newline in SSE: an embedded newline that is
	// not split with its own `data:` prefix is silently dropped by the parser.
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// writeSSEComment writes a comment line, which keeps a proxy connection alive
// without appearing as an event to the client.
func writeSSEComment(w http.ResponseWriter, f http.Flusher) error {
	if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// readLimitedBody reads a request body with a cap, so a large frame is refused
// rather than buffered entirely.
func readLimitedBody(r *http.Request) ([]byte, error) {
	const maxFrame = 8 << 20 // 8 MB: larger than any command line, smaller than a file
	if r.Body == nil {
		return nil, fmt.Errorf("empty body")
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			if len(buf)+n > maxFrame {
				return nil, fmt.Errorf("frame too large (limit %d bytes)", maxFrame)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err == io.EOF || n == 0 {
				break
			}
			return nil, err
		}
	}
	if len(buf) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	return buf, nil
}
