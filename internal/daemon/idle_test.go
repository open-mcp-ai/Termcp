package daemon

import (
	"context"
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

// The predicate the daemon installs (daemon.IsProbe) must exempt the *whole*
// probe, not just its first request: InspectEndpoint falls back to the public
// /api.md fingerprint whenever the daemon probe is rejected, and that fallback
// is still a liveness question. Exempting only /api/daemon let an unauthenticated
// `termcp daemon status` feed the countdown of the guarded instance it was
// reporting on.
func TestIdleWatcherProbeRequestsDoNotFeedCountdown(t *testing.T) {
	fired := make(chan struct{})
	w := NewIdleWatcher(200*time.Millisecond, func() { close(fired) })
	w.Exempt = IsProbe // the predicate main installs
	ts := httptest.NewServer(w.Track(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiDaemonPath && r.Header.Get("Authorization") == "" {
			rw.WriteHeader(http.StatusUnauthorized) // what a guarded instance answers
			return
		}
		fmt.Fprint(rw, "# Termcp HTTP API\n")
	})))
	t.Cleanup(ts.Close)
	host, port := hostPort(t, ts.URL)

	// Probe the way the CLI does, on a loop shorter than the countdown: the
	// fallback to /api.md must not keep the instance alive either.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rep := InspectEndpoint(context.Background(), host, port, "")
		if !rep.Termcp {
			t.Fatalf("probe lost the instance: %+v", rep)
		}
		select {
		case <-fired:
			return // fired despite continuous probing, exactly right
		default:
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("repeated daemon status probes kept the countdown from firing")
}
