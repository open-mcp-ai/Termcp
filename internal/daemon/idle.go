package daemon

import (
	"net/http"
	"sync"
	"time"
)

// DefaultIdleTimeout is how long a daemon instance stays alive without any
// activity before it exits on its own. The bridge's long-lived GET stream and
// the web UI's WebSocket keep it alive while someone is connected.
const DefaultIdleTimeout = 30 * time.Second

// IdleWatcher fires a callback once the wrapped handler has seen no in-flight
// requests for a given duration. "In flight" spans the whole request, so
// handlers that block for a connection's lifetime — the webui WebSocket, the
// MCP SSE and streamable-HTTP GET streams — count as activity until the peer
// disconnects.
type IdleWatcher struct {
	timeout time.Duration
	fire    func()

	// Exempt, when set, marks requests that are served without touching the
	// in-flight count at all. Daemon management probes are exempt: a status
	// query reports on idleness, so it must never count as activity and keep
	// the instance alive. The predicate is daemon.IsProbe, which matches the
	// probe's whole request set — the daemon route and the /api.md fingerprint
	// it falls back to — not just a path.
	Exempt func(*http.Request) bool

	mu       sync.Mutex
	inFlight int
	timer    *time.Timer
	gen      int // bumped on every arm, so a spent timer can't fire early
	fired    bool
}

// NewIdleWatcher returns a watcher for the given idle timeout. A timeout <= 0
// disables firing entirely. The countdown starts right away — a fresh instance
// has seen no activity yet — so a daemon nobody ever connects to still exits
// on its own.
func NewIdleWatcher(timeout time.Duration, fire func()) *IdleWatcher {
	w := &IdleWatcher{timeout: timeout, fire: fire}
	if timeout > 0 {
		w.gen++
		gen := w.gen
		w.timer = time.AfterFunc(timeout, func() { w.expire(gen) })
	}
	return w
}

// Track wraps next with the in-flight count. Wrap the outermost handler so no
// request path bypasses the count; requests the Exempt predicate claims are
// passed through uncounted.
func (w *IdleWatcher) Track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.Exempt != nil && w.Exempt(r) {
			next.ServeHTTP(rw, r)
			return
		}
		w.enter()
		defer w.leave()
		next.ServeHTTP(rw, r)
	})
}

func (w *IdleWatcher) enter() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight++
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

func (w *IdleWatcher) leave() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight--
	if w.inFlight == 0 && !w.fired && w.timeout > 0 && w.timer == nil {
		w.gen++
		gen := w.gen
		w.timer = time.AfterFunc(w.timeout, func() { w.expire(gen) })
	}
}

func (w *IdleWatcher) expire(gen int) {
	w.mu.Lock()
	if w.fired || w.inFlight != 0 || gen != w.gen {
		w.mu.Unlock()
		return
	}
	w.timer = nil
	w.fired = true
	w.mu.Unlock()
	w.fire()
}
