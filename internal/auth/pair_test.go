package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kindHandler echoes the verified trust domain back to the test.
func kindHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, ok := KindFromContext(r.Context())
		if !ok {
			w.Write([]byte("no-kind"))
			return
		}
		w.Write([]byte(string(kind)))
	})
}

func pairRequest(t *testing.T, h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func pairVerifiers(t *testing.T) (operator, mcp *Verifier) {
	t.Helper()
	operator, err := NewVerifier("operator-token", "")
	if err != nil {
		t.Fatal(err)
	}
	mcp, err = NewVerifier("mcp-token", "")
	if err != nil {
		t.Fatal(err)
	}
	return operator, mcp
}

func TestMiddlewarePair_MCPTokenReachesOnlyMCPPaths(t *testing.T) {
	operator, mcp := pairVerifiers(t)
	h := MiddlewarePair(operator, mcp, kindHandler())

	for _, path := range []string{"/sse", "/message", "/stream"} {
		rec := pairRequest(t, h, path, "mcp-token")
		if rec.Code != http.StatusOK || rec.Body.String() != "agent" {
			t.Errorf("MCP token on %s: got %d %q, want 200 agent", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/api/connections", "/", "/api/ui/ws"} {
		rec := pairRequest(t, h, path, "mcp-token")
		if rec.Code != http.StatusForbidden {
			t.Errorf("MCP token on %s: got %d, want 403", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "MCP token") {
			t.Errorf("403 on %s should name the token's proper surface: %s", path, rec.Body.String())
		}
	}
}

func TestMiddlewarePair_OperatorTokenWorksEverywhere(t *testing.T) {
	operator, mcp := pairVerifiers(t)
	h := MiddlewarePair(operator, mcp, kindHandler())

	for _, path := range []string{"/sse", "/stream", "/api/connections", "/", "/api/ui/ws"} {
		rec := pairRequest(t, h, path, "operator-token")
		if rec.Code != http.StatusOK || rec.Body.String() != "human" {
			t.Errorf("operator token on %s: got %d %q, want 200 human", path, rec.Code, rec.Body.String())
		}
	}
}

func TestMiddlewarePair_UnknownTokenGets401Not403(t *testing.T) {
	operator, mcp := pairVerifiers(t)
	h := MiddlewarePair(operator, mcp, kindHandler())

	rec := pairRequest(t, h, "/api/connections", "random-garbage")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("garbage token: got %d, want 401 (403 would confirm token validity)", rec.Code)
	}
}

func TestMiddlewarePair_NilMCPVerifierIsLegacyBehavior(t *testing.T) {
	operator, _ := pairVerifiers(t)
	h := MiddlewarePair(operator, nil, kindHandler())

	// One token, every surface — exactly the pre-split contract.
	for _, path := range []string{"/sse", "/api/connections", "/"} {
		rec := pairRequest(t, h, path, "operator-token")
		if rec.Code != http.StatusOK || rec.Body.String() != "human" {
			t.Errorf("legacy single token on %s: got %d %q", path, rec.Code, rec.Body.String())
		}
	}
}

func TestMiddlewarePair_OnlyMCPVerifierConfigured(t *testing.T) {
	_, mcp := pairVerifiers(t)
	h := MiddlewarePair(nil, mcp, kindHandler())

	rec := pairRequest(t, h, "/sse", "mcp-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("MCP token on /sse: got %d", rec.Code)
	}
	// The operator surface stays closed: there is no operator credential.
	rec = pairRequest(t, h, "/api/connections", "mcp-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MCP token on operator surface: got %d, want 403", rec.Code)
	}
	rec = pairRequest(t, h, "/api/connections", "operator-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("operator token with no operator verifier: got %d, want 401", rec.Code)
	}
}

func TestMiddlewarePair_MCPTokenViaBasicGetsNoCookie(t *testing.T) {
	operator, mcp := pairVerifiers(t)
	h := MiddlewarePair(operator, mcp, kindHandler())

	// The cookie exists for the browser (Web UI / WebSocket — operator
	// surface), so an agent-kind credential must never mint one.
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	req.SetBasicAuth("agent", "mcp-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Basic with MCP token on /sse: got %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			t.Error("MCP token minted a browser cookie")
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("human", "operator-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			found = true
		}
	}
	if !found {
		t.Error("operator Basic did not mint the browser cookie")
	}
}

func TestKindFromContextWithoutAuth(t *testing.T) {
	if _, ok := KindFromContext(context.Background()); ok {
		t.Fatal("kind present on a request the middleware never saw")
	}
}

func TestMiddlewareFull_ScopedToken(t *testing.T) {
	operator, mcp := pairVerifiers(t)
	tokens := OpenTokenStore(t.TempDir())
	token, _, err := tokens.Create("agent", []string{"shell.input"}, nil, false, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	h := MiddlewareFull(operator, mcp, tokens, kindHandler())

	// A scoped token authenticates on any surface; what it may do there is
	// the admission layer's question, not the middleware's.
	for _, path := range []string{"/stream", "/api/connections", "/"} {
		rec := pairRequest(t, h, path, token)
		if rec.Code != http.StatusOK || rec.Body.String() != "agent" {
			t.Errorf("scoped token on %s: got %d %q, want 200 agent", path, rec.Code, rec.Body.String())
		}
	}

	// Revocation takes effect on the very next request.
	recs, _ := tokens.List()
	if err := tokens.Revoke(recs[0].ID); err != nil {
		t.Fatal(err)
	}
	rec := pairRequest(t, h, "/stream", token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: got %d, want 401", rec.Code)
	}
}

func TestMiddlewareFull_PrincipalCarriesScopes(t *testing.T) {
	tokens := OpenTokenStore(t.TempDir())
	token, rec, err := tokens.Create("agent", []string{"shell.input"}, []string{"sess-9"}, true, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	var saw *Principal
	h := MiddlewareFull(nil, nil, tokens, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw, _ = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rr := pairRequest(t, h, "/api/anything", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("scoped token: got %d", rr.Code)
	}
	if saw == nil || saw.ID != rec.ID || saw.Kind != KindAgent {
		t.Fatalf("principal: %+v", saw)
	}
	if len(saw.Tools) != 1 || saw.Tools[0] != "shell.input" || !saw.ApprovalRequired {
		t.Fatalf("scopes lost in transit: %+v", saw)
	}
	if saw.Sessions[0] != "sess-9" {
		t.Fatalf("session scope lost: %+v", saw)
	}
}

func TestMiddlewareFull_StaticCredentialsStillWork(t *testing.T) {
	operator, mcp := pairVerifiers(t)
	tokens := OpenTokenStore(t.TempDir())
	h := MiddlewareFull(operator, mcp, tokens, kindHandler())

	if rec := pairRequest(t, h, "/api/connections", "operator-token"); rec.Body.String() != "human" {
		t.Errorf("operator: %d %q", rec.Code, rec.Body.String())
	}
	if rec := pairRequest(t, h, "/stream", "mcp-token"); rec.Body.String() != "agent" {
		t.Errorf("mcp: %d %q", rec.Code, rec.Body.String())
	}
	if rec := pairRequest(t, h, "/api/connections", "mcp-token"); rec.Code != http.StatusForbidden {
		t.Errorf("mcp on REST: %d, want 403", rec.Code)
	}
}
