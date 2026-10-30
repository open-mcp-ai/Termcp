package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPeerMetadataHidesEndpointSecretsAndKeepsTransport(t *testing.T) {
	const user = "synthetic-endpoint-user"
	const password = "synthetic-endpoint-password"
	const token = "synthetic-endpoint-token"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		gotUser, gotPassword, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPassword != password || r.URL.Query().Get("token") != token || r.URL.Path != "/private-endpoint-path" {
			t.Error("metadata projection changed transport credentials or path")
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		result := map[string]any{}
		switch req["method"] {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "privacy-peer", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "transport-ok"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
	}))
	defer peer.Close()
	endpoint, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.User = url.UserPassword(user, password)
	endpoint.Path = "/private-endpoint-path"
	endpoint.RawQuery = "token=" + token
	endpoint.Fragment = "private-fragment"
	log := withCapturedLogger(t)
	s := newTestServer(t)
	sess := internalSession(t, s)
	t.Cleanup(func() { closePeersBySession(sess) })
	opened := termText(t, s, map[string]any{"cmd": "open internal mcp $ENDPOINT", "secrets": map[string]any{"ENDPOINT": endpoint.String()}})
	if !strings.HasPrefix(opened, "TERM/1 200") {
		t.Fatal("peer did not open", opened)
	}
	handle := handleFrom(t, opened)
	assertSafe := func(label, text string) {
		t.Helper()
		for _, secret := range []string{user, password, token, "private-endpoint-path", "private-fragment"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s exposed endpoint credentials or private route", label)
			}
		}
	}
	assertSafe("open", opened)
	for _, cmd := range []string{"ls", "ls internal mcp", "get " + handle} {
		result := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(result, "TERM/1 200") {
			t.Fatalf("metadata failed for %s: %s", cmd, result)
		}
		assertSafe(cmd, result)
	}
	if result := termText(t, s, map[string]any{"cmd": "call " + handle + " status {}"}); !strings.Contains(result, "transport-ok") {
		t.Fatal("transport failed after metadata projection", result)
	}
	for _, record := range log.snapshot() {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "reply" {
				assertSafe("log reply", attr.Value.String())
			}
			return true
		})
	}
}

func TestPeerFailureHidesConfiguredEndpoint(t *testing.T) {
	endpoint := "https://synthetic-user:synthetic-password@peer.example/private-route?token=synthetic-token#private-fragment"
	// Both the generic transport and CDP paths must mask generated error text.
	stmt := directive.Statement{Op: "call", Path: "call peer"}
	failure := &url.Error{Op: "Post", URL: endpoint + "/method", Err: errors.New("connection failed")}
	for _, body := range []string{jrpcError(stmt, failure).Body, cdpFailure(stmt, "method", failure).Body, jrpcError(stmt, fmt.Errorf("dial %s failed", endpoint), endpoint).Body} {
		for _, secret := range []string{"synthetic-user", "synthetic-password", "synthetic-token", "private-route", "private-fragment"} {
			if strings.Contains(body, secret) {
				t.Errorf("generated error exposed endpoint data: %s", body)
			}
		}
	}
}
