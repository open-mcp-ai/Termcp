package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestManagedMCPTransportCredentials(t *testing.T) {
	dir := t.TempDir()
	vault, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vault.Close() })
	account, err := auth.OpenManaged(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _, _ := newTestServerWithHistory(t)
	account.SetMCPConnectionInvalidator(func(principalID, credentialID string) {
		go s.InvalidateConnections(principalID, credentialID)
	})
	mux := http.NewServeMux()
	mux.Handle("/stream", s.StreamableHTTPHandler())
	mux.Handle("/sse", s.SSEHandler())
	mux.Handle("/message", s.MessageHandler())
	ts := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	var csrf string
	call := func(method, path, body string) map[string]any {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("X-Termcp-CSRF", csrf)
		resp, err := admin.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var data map[string]any
		if err = json.NewDecoder(resp.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, data)
		}
		return data
	}
	csrf = call("GET", "/auth/status", "")["csrf"].(string)
	csrf = call("POST", "/auth/setup", `{"password":"synthetic-transport-password","config_password":"synthetic-transport-password"}`)["csrf"].(string)
	first := call("POST", "/auth/principals", `{"name":"agent A","issue_token":true}`)
	token := first["token"].(string)
	principal := first["principal"].(map[string]any)["id"].(string)
	second := call("POST", "/auth/principals", `{"name":"agent B","issue_token":true}`)
	other := second["token"].(string)
	credential := first["credential"].(map[string]any)["id"].(string)
	networkCtx, networkCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer networkCancel()
	request := func(method, path, body, credential, session string) *http.Response {
		t.Helper()
		r, _ := http.NewRequestWithContext(networkCtx, method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Mcp-Session-Id", session)
		r.Header.Set(NamespaceHeader, "pretend-to-be-another-agent")
		r.Header.Set(ClientModeHeader, ClientModeStdioBridge)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"auth-test","version":"1"}}}`
	resp := request("POST", "/stream", init, token, "")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	connections := waitForConnections(t, s, 1)
	if got := connections[0]; got.SessionID != sid || got.PrincipalID != principal ||
		got.AgentName != "agent A" || got.CredentialID != credential ||
		got.Transport != ConnectionTransportStreamableHTTP || got.ClientMode != ClientModeStdioBridge {
		t.Fatalf("attributed streamable connection = %+v", got)
	}
	rawConnectionJSON, _ := json.Marshal(connections)
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	if strings.Contains(string(rawConnectionJSON), token) || strings.Contains(string(rawConnectionJSON), digest) {
		t.Fatalf("connection listing exposed bearer material: %s", rawConnectionJSON)
	}

	// The same current Token may hold multiple independent MCP sessions. Count
	// sessions, not tokens: two bridges sharing one Agent token are still two
	// live connections.
	resp = request("POST", "/stream", init, token, "")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	secondSID := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || secondSID == "" || secondSID == sid {
		t.Fatalf("second initialize: status=%d session=%q", resp.StatusCode, secondSID)
	}
	connections = waitForConnections(t, s, 2)
	if connections[0].PrincipalID != principal || connections[1].PrincipalID != principal ||
		connections[0].CredentialID != credential || connections[1].CredentialID != credential {
		t.Fatalf("multi-session attribution = %+v", connections)
	}

	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	createdGroup := call("POST", "/auth/groups", `{"name":"transport-policy","preset_id":"host-operator"}`)["group"].(map[string]any)
	changeGroup, _ := json.Marshal(map[string]any{"revision": 1, "group_id": createdGroup["id"]})
	call("PUT", "/auth/principals/"+principal+"/group", string(changeGroup))
	// An ordinary permission change applies to the next business call without
	// rotating the static credential or invalidating either live MCP session.
	resp = request("POST", "/stream", ping, token, sid)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("static-token session did not survive an ordinary group change: %d", resp.StatusCode)
	}
	if got := waitForConnections(t, s, 2); len(got) != 2 || got[0].CredentialID != credential && got[1].CredentialID != credential {
		t.Fatalf("group change replaced static-token sessions: %+v", got)
	}

	// Valid authentication plus an unknown session is recoverable: 404 tells the
	// client to initialize again without changing its credential. An invalid
	// bearer is still an authentication failure, not a session ownership error.
	resp = request("POST", "/stream", ping, token, "missing-session")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session status = %d, want 404", resp.StatusCode)
	}
	resp = request("POST", "/stream", ping, token+"-invalid", "missing-session")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid bearer status = %d, want 401", resp.StatusCode)
	}

	// A terminated session can be reinitialized with the same valid credential,
	// preserving Agent and credential ownership across the new transport session.
	resp = request("DELETE", "/stream", "", token, sid)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("terminate stream session: %d", resp.StatusCode)
	}
	resp = request("POST", "/stream", init, token, "")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	reinitializedSID := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || reinitializedSID == "" || reinitializedSID == sid {
		t.Fatalf("reinitialize after session loss: status=%d session=%q old=%q", resp.StatusCode, reinitializedSID, sid)
	}
	reinitialized := waitForConnection(t, s, func(connection Connection) bool {
		return connection.SessionID == reinitializedSID
	})
	if reinitialized.PrincipalID != principal || reinitialized.CredentialID != credential {
		t.Fatalf("reinitialized session changed ownership: %+v", reinitialized)
	}
	sid = reinitializedSID

	resp = request("POST", "/stream", ping, other, sid)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("foreign Agent adopted stream session")
	}
	resp = request("POST", "/stream", ping, token, sid)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("owner lost stream session")
	}
	if got := s.Connections(); len(got) != 2 || got[0].Namespace != "ns:agent-"+principal || got[1].Namespace != "ns:agent-"+principal {
		t.Fatalf("identity namespace: %+v", got)
	}
	// The real stdio bridge uses the same Bearer credential on every request.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := range 3 {
		wg.Go(func() {
			var out bytes.Buffer
			err := mcpbridge.Run(ctx, mcpbridge.Config{URL: ts.URL + "/stream", Token: token, In: strings.NewReader(init + "\n" + ping + "\n"), Out: &out})
			if err == nil && !strings.Contains(out.String(), `"result"`) {
				err = fmt.Errorf("bridge %d got no result", i)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// SSE messages are tied to the credential that opened their event stream.
	resp = request("GET", "/sse", "", token, "")
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var endpoint string
	for endpoint == "" {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			endpoint = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
	msg := request("POST", endpoint, init, other, "")
	msg.Body.Close()
	if msg.StatusCode != 403 {
		t.Fatal("foreign Agent adopted SSE session")
	}
	msg = request("POST", endpoint, init, token, "")
	msg.Body.Close()
	if msg.StatusCode != 202 {
		t.Fatalf("SSE initialize: %d", msg.StatusCode)
	}
	sseConnection := waitForConnection(t, s, func(connection Connection) bool {
		return connection.Transport == ConnectionTransportSSE
	})
	if sseConnection.PrincipalID != principal || sseConnection.AgentName != "agent A" ||
		sseConnection.CredentialID != credential || sseConnection.ClientMode != ClientModeStdioBridge {
		t.Fatalf("attributed SSE connection = %+v", sseConnection)
	}
	groupRevision := int(createdGroup["revision"].(float64))
	denyPolicy, _ := json.Marshal(map[string]any{"revision": groupRevision, "policy": auth.EmptyAgentPolicy()})
	call("PATCH", "/auth/groups/"+createdGroup["id"].(string), string(denyPolicy))
	msg = request("POST", endpoint, ping, token, "")
	msg.Body.Close()
	if msg.StatusCode != http.StatusAccepted {
		t.Fatalf("SSE session did not survive an ordinary group change: %d", msg.StatusCode)
	}
	if got := waitForConnections(t, s, 3); len(got) != 3 {
		t.Fatalf("ordinary group change replaced SSE or stream sessions: %+v", got)
	}
	call("DELETE", "/auth/tokens/"+credential, "")
	finished := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("revoked SSE connection survived")
	}
	waitForCredentialAbsent(t, s, credential)
	resp = request("POST", "/stream", ping, other, "")
	resp.Body.Close()
	if resp.StatusCode == 401 {
		t.Fatal("other Agent was revoked")
	}
}

func TestAuthorizeTransportAllowsOnlyOAuthRefreshForSameAgent(t *testing.T) {
	tests := []struct {
		name       string
		owner      transportOwner
		actorID    string
		credential string
		oauth      bool
		grant      string
		client     string
		resource   string
		want       transportAuthorization
	}{
		{
			name:       "oauth refresh same Agent",
			owner:      transportOwner{principal: "agent-a", credential: "token:oauth-old", oauth: true, grantID: "grant-a", oauthClient: "client-a", resource: "https://termcp.example/stream"},
			actorID:    "agent-a",
			credential: "token:oauth-new",
			oauth:      true,
			grant:      "grant-a",
			client:     "client-a",
			resource:   "https://termcp.example/stream",
			want:       transportAuthorized,
		},
		{
			name:       "oauth token different Agent",
			owner:      transportOwner{principal: "agent-a", credential: "token:oauth-old", oauth: true},
			actorID:    "agent-b",
			credential: "token:oauth-new",
			oauth:      true,
			want:       transportOwnerMismatch,
		},
		{
			name:       "oauth token different grant",
			owner:      transportOwner{principal: "agent-a", credential: "token:oauth-old", oauth: true, grantID: "grant-a", oauthClient: "client-a", resource: "https://termcp.example/stream"},
			actorID:    "agent-a",
			credential: "token:oauth-new",
			oauth:      true,
			grant:      "grant-b",
			client:     "client-a",
			resource:   "https://termcp.example/stream",
			want:       transportOwnerMismatch,
		},
		{
			name:       "oauth token different client",
			owner:      transportOwner{principal: "agent-a", credential: "token:oauth-old", oauth: true, grantID: "grant-a", oauthClient: "client-a", resource: "https://termcp.example/stream"},
			actorID:    "agent-a",
			credential: "token:oauth-new",
			oauth:      true,
			grant:      "grant-a",
			client:     "client-b",
			resource:   "https://termcp.example/stream",
			want:       transportOwnerMismatch,
		},
		{
			name:       "oauth token different resource",
			owner:      transportOwner{principal: "agent-a", credential: "token:oauth-old", oauth: true, grantID: "grant-a", oauthClient: "client-a", resource: "https://termcp.example/stream"},
			actorID:    "agent-a",
			credential: "token:oauth-new",
			oauth:      true,
			grant:      "grant-a",
			client:     "client-a",
			resource:   "https://termcp.example/sse",
			want:       transportOwnerMismatch,
		},
		{
			name:       "static Token rotation",
			owner:      transportOwner{principal: "agent-a", credential: "token:old"},
			actorID:    "agent-a",
			credential: "token:new",
			want:       transportOwnerMismatch,
		},
		{
			name:       "static credential exact match",
			owner:      transportOwner{principal: "agent-a", credential: "token:old"},
			actorID:    "agent-a",
			credential: "token:old",
			want:       transportAuthorized,
		},
		{
			name:       "OAuth token cannot adopt static session",
			owner:      transportOwner{principal: "agent-a", credential: "token:static", oauth: false},
			actorID:    "agent-a",
			credential: "token:oauth-new",
			oauth:      true,
			want:       transportOwnerMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{connections: newConnRegistry()}
			server.transportOwners.Store("session-a", test.owner)
			ctx := t.Context()
			if test.oauth {
				ctx = auth.WithVerifiedOAuthAuthority(ctx, test.grant, test.client, test.resource)
			}
			got := server.authorizeTransport(ctx, "session-a", auth.Actor{ID: test.actorID}, test.credential, test.oauth)
			if got != test.want {
				t.Fatalf("authorizeTransport = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAuthorizeTransportDistinguishesMissingFromMismatch(t *testing.T) {
	server := &Server{connections: newConnRegistry()}
	got := server.authorizeTransport(t.Context(), "missing", auth.Actor{ID: "agent-a"}, "token:a", false)
	if got != transportSessionMissing {
		t.Fatalf("missing session = %v", got)
	}
}

func waitForConnection(t *testing.T, s *Server, match func(Connection) bool) Connection {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, connection := range s.Connections() {
			if match(connection) {
				return connection
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for MCP connection; live=%+v", s.Connections())
	return Connection{}
}

func waitForCredentialAbsent(t *testing.T, s *Server, credentialID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, connection := range s.Connections() {
			if connection.CredentialID == credentialID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("revoked credential %q still has live MCP connections: %+v", credentialID, s.Connections())
}
