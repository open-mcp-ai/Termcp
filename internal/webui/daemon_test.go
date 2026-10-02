package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDaemonInfoEndpoint pins what the management probe learns over HTTP: the
// instance's pid, version and daemon identity — the payload `termcp daemon
// status` renders.
func TestDaemonInfoEndpoint(t *testing.T) {
	h := &Handler{Version: "v0.0.0-test", Daemon: true, StartedAt: "2026-09-30T10:00:00Z", DaemonLog: "/data/termcp.log", IdleTimeout: 6 * time.Second}
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/daemon", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/daemon = %d, want 200", rr.Code)
	}
	var body struct {
		Daemon        bool   `json:"daemon"`
		PID           int    `json:"pid"`
		Version       string `json:"version"`
		StartedAt     string `json:"started_at"`
		Log           string `json:"log"`
		IdleTimeoutMS int64  `json:"idle_timeout_ms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode daemon body: %v (%s)", err, rr.Body.String())
	}
	if !body.Daemon || body.PID <= 0 || body.Version != "v0.0.0-test" || body.StartedAt != "2026-09-30T10:00:00Z" || body.Log != "/data/termcp.log" {
		t.Errorf("daemon info = %+v, want the handler's fields", body)
	}
	// The effective countdown is published so a client that must count as
	// activity (the stdio bridge) can ping faster than it.
	if body.IdleTimeoutMS != 6000 {
		t.Errorf("idle_timeout_ms = %d, want 6000", body.IdleTimeoutMS)
	}
}

// A manually started instance has no daemon lifecycle: the stop route refuses
// it instead of pretending to stop something.
func TestDaemonStopRefusesManualInstance(t *testing.T) {
	called := make(chan struct{}, 1)
	h := &Handler{Daemon: false, StopDaemon: func() { called <- struct{}{} }}
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/daemon/stop", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST /api/daemon/stop (manual) = %d, want 409", rr.Code)
	}
	select {
	case <-called:
		t.Error("stop callback ran for a manual instance")
	default:
	}
}

// The stop answers first and triggers the graceful shutdown after, so the
// caller's request can never be cut off by the shutdown it asked for.
func TestDaemonStopTriggersShutdown(t *testing.T) {
	called := make(chan struct{})
	h := &Handler{Daemon: true, StopDaemon: func() { close(called) }}
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/daemon/stop", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/daemon/stop = %d, want 200", rr.Code)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("stop callback never ran")
	}
}
