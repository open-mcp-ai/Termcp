package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

func tokenTestHandler(t *testing.T) (*httptest.Server, *auth.TokenStore) {
	t.Helper()
	tokens := auth.OpenTokenStore(t.TempDir())
	h := &Handler{Tokens: tokens}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tokens", h.handleListTokens)
	mux.HandleFunc("POST /api/tokens", h.handleCreateToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", h.handleRevokeToken)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tokens
}

func TestTokenLifecycleOverREST(t *testing.T) {
	srv, tokens := tokenTestHandler(t)

	// Create: plaintext comes back once.
	resp, err := http.Post(srv.URL+"/api/tokens", "application/json",
		strings.NewReader(`{"kind":"agent","tools":["shell.output"],"ttl_seconds":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var created struct {
		Token  string `json:"token"`
		Record struct {
			ID    string   `json:"id"`
			Kind  string   `json:"kind"`
			Tools []string `json:"tools"`
		} `json:"record"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "tcptok_") || created.Record.ID == "" {
		t.Fatalf("create response: %+v", created)
	}
	if _, err := tokens.Resolve(created.Token); err != nil {
		t.Fatalf("the minted token must resolve: %v", err)
	}

	// List carries metadata but never the plaintext nor the verifier.
	resp2, err := http.Get(srv.URL + "/api/tokens")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 1<<20)
	n, _ := resp2.Body.Read(body)
	resp2.Body.Close()
	if strings.Contains(string(body[:n]), created.Token) {
		t.Fatal("list leaked a token")
	}

	// Revoke: 204, then the token no longer resolves.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/tokens/"+created.Record.ID, nil)
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp3.StatusCode)
	}
	if _, err := tokens.Resolve(created.Token); err == nil {
		t.Fatal("revoked token still resolves")
	}

	// Unknown id: 404.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/tokens/nope", nil)
	resp4, _ := http.DefaultClient.Do(req)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d, want 404", resp4.StatusCode)
	}
}

func TestTokenRoutesRefuseAgentPrincipal(t *testing.T) {
	tokens := auth.OpenTokenStore(t.TempDir())
	h := &Handler{Tokens: tokens}
	mux := http.NewServeMux()
	// Wrap the routes in a middleware that pins an agent principal: a scoped
	// token must not mint or kill tokens.
	mux.HandleFunc("POST /api/tokens", h.handleCreateToken)
	mux.HandleFunc("GET /api/tokens", h.handleListTokens)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), &auth.Principal{ID: "t-x", Kind: auth.KindAgent})
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(agent)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/tokens", "application/json", strings.NewReader(`{"kind":"agent"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent minting a token: got %d, want 403", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/api/tokens")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent listing tokens: got %d, want 403", resp.StatusCode)
	}
}
