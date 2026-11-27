package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newIdleServer wraps a handler in a watcher with a short timeout and returns
// the server plus the channel closed when the countdown fires.
func newIdleServer(t *testing.T, timeout time.Duration, h http.HandlerFunc) (*httptest.Server, chan struct{}) {
	t.Helper()
	fired := make(chan struct{})
	w := NewIdleWatcher(timeout, func() { close(fired) })
	ts := httptest.NewServer(w.Track(h))
	t.Cleanup(ts.Close)
	return ts, fired
}

func TestIdleWatcherFiresWhenIdle(t *testing.T) {
	ts, fired := newIdleServer(t, 50*time.Millisecond, func(http.ResponseWriter, *http.Request) {})

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("idle countdown never fired")
	}
}

// A request that blocks for its whole lifetime (the shape of the webui
// WebSocket and the MCP GET streams) must hold the countdown off until the
// connection closes.
func TestIdleWatcherHeldByOpenConnection(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	ts, fired := newIdleServer(t, 50*time.Millisecond, func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})

	go func() {
		resp, err := http.Get(ts.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered

	time.Sleep(300 * time.Millisecond) // several times the timeout
	select {
	case <-fired:
		t.Fatal("countdown fired while a connection was still open")
	default:
	}

	unblock()
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("countdown did not fire after the connection closed")
	}
}

// Requests arriving faster than the timeout must keep resetting it; it fires
// only after activity actually stops.
func TestIdleWatcherActivityResetsCountdown(t *testing.T) {
	ts, fired := newIdleServer(t, 100*time.Millisecond, func(http.ResponseWriter, *http.Request) {})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		select {
		case <-fired:
			t.Fatal("countdown fired although requests kept arriving")
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("countdown never fired after activity stopped")
	}
}

func TestIdleWatcherDisabled(t *testing.T) {
	ts, fired := newIdleServer(t, 0, func(http.ResponseWriter, *http.Request) {})

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	time.Sleep(300 * time.Millisecond)
	select {
	case <-fired:
		t.Fatal("disabled countdown fired")
	default:
	}
}

// Arming a timer starts it, so the callback can run expire() before
// NewIdleWatcher has finished setting up the two fields expire() writes (timer,
// gen). This drives the collision on purpose: a one-nanosecond countdown
// guarantees the callback lands inside the construction window.
func TestIdleWatcherConstructionArmsUnderLock(t *testing.T) {
	for range 25 {
		fired := make(chan struct{})
		watcher := NewIdleWatcher(time.Nanosecond, func() { close(fired) })
		if watcher.timeout != time.Nanosecond {
			t.Fatalf("watcher recorded timeout %v", watcher.timeout)
		}
		select {
		case <-fired:
		case <-time.After(3 * time.Second):
			t.Fatal("countdown never fired")
		}
	}
}

// The daemon probe is exempt: a status query reports on idleness, so it must
// never count as activity. Probing every 50ms against a 200ms timeout must
// still let the countdown (armed when the watcher was created) fire.
func TestIdleWatcherExemptRequestsDoNotFeedCountdown(t *testing.T) {
	fired := make(chan struct{})
	w := NewIdleWatcher(200*time.Millisecond, func() { close(fired) })
	w.Exempt = func(r *http.Request) bool { return r.URL.Path == "/api/daemon" }
	ts := httptest.NewServer(w.Track(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {})))
	t.Cleanup(ts.Close)

	for range 12 {
		resp, err := http.Get(ts.URL + "/api/daemon")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		select {
		case <-fired:
			return // fired despite the probes, exactly right
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("exempt probes kept the countdown from firing")
}

// THE PREDICATE MUST EXEMPT THE WHOLE PROBE, not just its first request: a caller falls
// back to a public document when the status route is rejected, and that fallback is still
// a liveness question. Exempting only the first path let an unauthenticated status query
// feed the countdown of the very instance it was reporting on.
func TestIdleWatcherProbeRequestsDoNotFeedCountdown(t *testing.T) {
	fired := make(chan struct{})
	w := NewIdleWatcher(200*time.Millisecond, func() { close(fired) })
	w.Exempt = func(r *http.Request) bool { return IsProbe(r, "/status") }
	ts := httptest.NewServer(w.Track(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" && r.Header.Get("Authorization") == "" {
			rw.WriteHeader(http.StatusUnauthorized) // what a guarded instance answers
			return
		}
		fmt.Fprint(rw, "the public document")
	})))
	t.Cleanup(ts.Close)

	// Probe on a loop shorter than the countdown, taking BOTH requests each round: the
	// rejected status route and the document it falls back to.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, path := range []string{"/status", "/document"} {
			req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(MarkProbe(req))
			if err != nil {
				t.Fatalf("probe %s: %v", path, err)
			}
			resp.Body.Close()
		}
		select {
		case <-fired:
			return // fired despite continuous probing, exactly right
		default:
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("exempt probes kept the countdown from firing")
}

func TestIdleWatcherStillFiresAfterOverlappingRequests(t *testing.T) {
	for round := 0; round < 20; round++ {
		fired := make(chan struct{}, 1)
		w := NewIdleWatcher(3*time.Millisecond, func() {
			select {
			case fired <- struct{}{}:
			default:
			}
		})
		ts := httptest.NewServer(w.Track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 12; j++ {
					resp, err := http.Get(ts.URL)
					if err == nil {
						resp.Body.Close()
					}
					time.Sleep(time.Duration(round%3) * time.Millisecond)
				}
			}()
		}
		wg.Wait()
		ts.Close()

		select {
		case <-fired:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: watcher never fired after traffic stopped - countdown left disarmed",
				round)
		}
	}
}
