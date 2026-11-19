package daemon_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/daemon"
)

func TestHTTPProbeOwnEndpointOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	mux.Handle("/moved", http.RedirectHandler("/ready", 302))
	s := httptest.NewServer(mux)
	defer s.Close()
	for _, path := range []string{"/ready", "/moved", "/missing"} {
		err := daemon.HTTPProbe(s.URL + path)(t.Context())
		if (err == nil) != (path == "/ready") || errors.Is(err, daemon.ErrNotRunning) {
			t.Errorf("%s: %v", path, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if daemon.HTTPProbe(s.URL+"/ready")(ctx) == nil {
		t.Error("cancelled probe reported ready")
	}
	if !daemon.PortReachable(t.Context(), s.Listener.Addr().String()) {
		t.Error("test listener should be reachable")
	}
	s.Close()
	if err := daemon.HTTPProbe(s.URL)(t.Context()); !errors.Is(err, daemon.ErrNotRunning) {
		t.Errorf("closed local listener: %v", err)
	}
}
