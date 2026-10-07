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

// The watcher keeps four fields in agreement (inFlight, timer, gen, fired) and
// arms or disarms a timer as a side effect of counting. The failure mode of that
// state machine is quiet: a request overlapping a firing timer could leave the
// countdown disarmed, so the daemon would never exit on its own again - and an idle
// daemon that stays alive looks exactly like a daemon with traffic.
//
// Each field is individually safe to touch, which is why this is worth testing
// rather than reading: the invariant is about the four together, and a request
// racing a firing timer is the case that exercises it. The overlap is driven
// deliberately and the watcher must still be able to fire afterwards.
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
