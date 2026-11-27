package daemon

import (
	"net/http"
	"sync"
	"sync/atomic"
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

	// THE FOUR FIELDS ARE ONE VALUE, published atomically. inFlight, the timer, the
	// generation and fired only ever change together — arming is "stop the old timer,
	// bump the generation, install a new one", and expiring is "no; or mark fired and
	// drop the timer". A mutex is one way to say that; an immutable snapshot swapped by
	// CompareAndSwap is the other, and it removes the two obligations a lock carries
	// (unlock on every path, run the fire callback outside it).
	state atomic.Pointer[idleState]
	fireOnce sync.Once
}

// idleState is the watcher's state, written once per transition.
type idleState struct {
	inFlight int
	timer    *time.Timer
	gen      uint64 // bumped on every arm, so a spent timer can't fire early
	fired    bool
}

// load returns the current state, never nil.
func (w *IdleWatcher) load() *idleState {
	if st := w.state.Load(); st != nil {
		return st
	}
	return &idleState{}
}

// swap applies fn to the current state and publishes the result, retrying when
// another goroutine got there first. fn returns the timer it created (if any) so a
// losing attempt can stop it rather than leak a stray callback.
func (w *IdleWatcher) swap(fn func(*idleState) *time.Timer) *idleState {
	for {
		cur := w.state.Load()
		next := idleState{}
		if cur != nil {
			next = *cur
		}
		stray := fn(&next)
		if w.state.CompareAndSwap(cur, &next) {
			return &next
		}
		if stray != nil {
			stray.Stop()
		}
	}
}

// NewIdleWatcher returns a watcher for the given idle timeout. A timeout <= 0
// disables firing entirely. The countdown starts right away — a fresh instance
// has seen no activity yet — so a daemon nobody ever connects to still exits
// on its own.
func NewIdleWatcher(timeout time.Duration, fire func()) *IdleWatcher {
	w := &IdleWatcher{timeout: timeout, fire: fire}
	if timeout <= 0 {
		return w
	}
	// ARM ONCE, BEFORE THE WATCHER IS SHARED: the pointer below publishes the armed
	// state, so no expire() can observe a half-built one.
	st := &idleState{gen: 1}
	st.timer = time.AfterFunc(timeout, func() { w.expire(1) })
	w.state.Store(st)
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
	w.swap(func(st *idleState) *time.Timer {
		st.inFlight++
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		return nil
	})
}

func (w *IdleWatcher) leave() {
	w.swap(func(st *idleState) *time.Timer {
		st.inFlight--
		if st.inFlight == 0 && !st.fired && w.timeout > 0 && st.timer == nil {
			st.gen++
			t := time.AfterFunc(w.timeout, func() { w.expire(st.gen) })
			st.timer = t
			return t
		}
		return nil
	})
}

func (w *IdleWatcher) expire(gen uint64) {
	st := w.swap(func(st *idleState) *time.Timer {
		// A SPENT TIMER IS RECOGNISED, NOT ASSERTED: the generation it was created
		// with no longer matches, or activity arrived, or it already fired. Reporting
		// that by leaving the state untouched is what makes the retry loop safe.
		if st.fired || st.inFlight != 0 || gen != st.gen {
			return nil
		}
		st.timer = nil
		st.fired = true
		return nil
	})
	// THE CALLBACK RUNS OUTSIDE ANY LOCK, and only the transition that flipped
	// `fired` from false to true calls it: a losing attempt saw fired already true
	// or saw the generation bump, and neither sets this flag.
	if st.fired && st.gen == gen && st.inFlight == 0 {
		w.fireOnce.Do(w.fire)
	}
}
