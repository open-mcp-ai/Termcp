package mcp

import (
	"context"
	"time"
)

// SESSION EXPIRY: a client that leaves without saying goodbye must not keep a slot.
//
// The streamable transport keeps per-client state — an open GET stream, the client's
// self-description, the pendings of a sampling request — and the only thing that ever
// removed it was DELETE (streamable.go's handleDelete). A client that crashed, was
// killed, or lost the network never sends DELETE, so the session stayed in the map for
// the life of the process: one dead entry per crash, and in a long-lived instance the
// registry is a list of clients that are not there. `Connections()` reports them, and
// every notification fans out to a destination nobody is reading.
//
// THE WINDOW IS DERIVED FROM THE KEEPALIVE, not invented: a client with an open GET is
// sent a comment every sseKeepAlive (30s), and a window shorter than two of those would
// reclaim a client that is merely quiet between heartbeats. Two of them is the smallest
// number that cannot do that, and it is expressed that way in the constant so the
// relationship is visible rather than being a magic 60.
//
// AN OPEN GET STREAM IS EXEMPT. Its liveness is decided by that request's own context:
// a client watching the stream is present by definition, and reaping it would cut a
// live SSE connection on a long think. When the request ends, ctx.Done fires, the
// stream closes, and the session becomes reapable — which is the correct order.
const defaultIdleWindow = 2 * sseKeepAlive

// touch records activity on this session, which is what the reaper measures.
func (sc *streamClient) touch() { sc.lastSeen.Store(time.Now().UnixNano()) }

// idleFor reports how long this session has gone untouched.
func (sc *streamClient) idleFor(now time.Time) time.Duration {
	last := sc.lastSeen.Load()
	if last == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, last))
}

// hasOpenStream reports whether a GET stream is attached, which exempts the session
// from the reaper.
func (sc *streamClient) hasOpenStream() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.out != nil
}

// startJanitor runs the reaper until Shutdown.
//
// IT IS ONE GOROUTINE PER TRANSPORT, not one per session: the check is a scan of a map
// that is already under a lock, and a timer per client would be a second thing to
// cancel on every disconnect.
func (s *StreamableHTTPServer) startJanitor() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopJanitor = cancel
	window := s.idleWindow
	if window <= 0 {
		window = defaultIdleWindow
	}
	// FOUR CHECKS PER WINDOW: the granularity a caller can observe is a quarter of the
	// window, which is fine for a reaper and keeps a short --idle-window option
	// meaningful in a test.
	tick := window / 4
	if tick < time.Second {
		tick = time.Second
	}
	go func() {
		defer close(s.janitorDone)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reapIdle(ctx, window)
			}
		}
	}()
}

// reapIdle closes every session that has gone quiet and has no stream attached.
//
// SNAPSHOT, THEN ACT: the map is read under the lock and the closing happens outside
// it, because close() wakes waiters and unregisterSession runs hooks that belong to
// other packages. Holding this transport's lock across those would put a caller's hook
// inside the reaper's critical section.
func (s *StreamableHTTPServer) reapIdle(ctx context.Context, window time.Duration) {
	now := time.Now()
	var doomed []*streamClient
	s.mu.Lock()
	for id, sess := range s.sessions {
		if sess.hasOpenStream() {
			continue
		}
		if sess.idleFor(now) >= window {
			delete(s.sessions, id)
			doomed = append(doomed, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range doomed {
		sess.close()
		s.srv.UnregisterSession(ctx, sess)
	}
}
