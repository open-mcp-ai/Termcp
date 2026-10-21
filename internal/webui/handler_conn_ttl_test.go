package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/vault"
)

const ttlImportDoc = `[[connections]]
name = "injected"
kind = "remote"
host = "sidecar.example"
user = "op"
password = "one-job-secret"
`

func importRequest(t *testing.T, query string) (*httptest.ResponseRecorder, *sshconfig.Store) {
	t.Helper()
	st := sshconfig.NewStore(t.TempDir())
	h := &Handler{SSH: st}
	mux := http.NewServeMux()
	h.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/connections/batch"+query, strings.NewReader(ttlImportDoc))
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, req)
	return out, st
}

func TestImportTemporaryWithTTLExpires(t *testing.T) {
	out, st := importRequest(t, "?temporary=true&ttl_seconds=1")
	if out.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", out.Code, out.Body.String())
	}
	if !st.IsTemporary("injected") {
		t.Fatal("imported profile is not temporary")
	}
	// Poll, never sleep a fixed span: CI runners stall unpredictably.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !st.IsTemporary("injected") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("profile outlived its TTL")
}

func TestImportTTLRejectsNonTemporary(t *testing.T) {
	out, _ := importRequest(t, "?ttl_seconds=60")
	if out.Code != http.StatusBadRequest {
		t.Fatalf("ttl without temporary: got %d, want 400", out.Code)
	}
}

func TestImportTTLValidatesRange(t *testing.T) {
	for _, bad := range []string{"0", "-5", "abc", "99999999"} {
		out, _ := importRequest(t, "?temporary=true&ttl_seconds="+bad)
		if out.Code != http.StatusBadRequest {
			t.Errorf("ttl_seconds=%s: got %d, want 400", bad, out.Code)
		}
	}
}

// A scoped token needs profile.create to inject (the sidecar shape); without
// it the import route refuses before parsing a byte.
func TestImportRequiresProfileCreateScope(t *testing.T) {
	st := sshconfig.NewStore(t.TempDir())
	h := &Handler{SSH: st}
	mux := http.NewServeMux()
	h.Register(mux)

	post := func(t *testing.T, p *auth.Principal) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/connections/batch?temporary=true", strings.NewReader(ttlImportDoc))
		if p != nil {
			req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		}
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		return out.Code
	}

	// No principal (loopback, no auth middleware): the operator path, allowed.
	if code := post(t, nil); code != http.StatusCreated {
		t.Fatalf("operator import: %d", code)
	}
	// Scoped agent token without profile.create: refused.
	code := post(t, &auth.Principal{ID: "t", Kind: auth.KindAgent, Tools: []string{"shell.output"}})
	if code != http.StatusForbidden {
		t.Fatalf("import without profile.create: %d, want 403", code)
	}
	// The sidecar token shape: profile.create only.
	code = post(t, &auth.Principal{ID: "t2", Kind: auth.KindAgent, Tools: []string{"profile.create"}})
	if code != http.StatusCreated {
		t.Fatalf("import with profile.create: %d, want 201", code)
	}
}

// The review gaps: reads of profiles need profile.list, deletes need
// profile.delete, and the operator principal must keep passing every gate it
// always passed (regression guard).
func TestScopedTokenProfileReadWriteGates(t *testing.T) {
	st := sshconfig.NewStore(t.TempDir())
	h := &Handler{SSH: st}
	mux := http.NewServeMux()
	h.Register(mux)

	seed := "kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n"
	put := httptest.NewRecorder()
	mux.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/api/connections/prod", strings.NewReader(seed)))
	if put.Code != http.StatusNoContent {
		t.Fatalf("seed PUT: %d %s", put.Code, put.Body.String())
	}

	call := func(method, path, body string, p *auth.Principal) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if p != nil {
			req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		}
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		return out.Code
	}

	createOnly := &auth.Principal{ID: "sidecar", Kind: auth.KindAgent, Tools: []string{"profile.create"}}
	readOnly := &auth.Principal{ID: "reader", Kind: auth.KindAgent, Tools: []string{"profile.list"}}

	// The sidecar token: injects, but cannot read, list, export or delete.
	if code := call(http.MethodGet, "/api/connections/prod", "", createOnly); code != http.StatusForbidden {
		t.Errorf("create-only GET single: %d, want 403", code)
	}
	if code := call(http.MethodGet, "/api/connections", "", createOnly); code != http.StatusForbidden {
		t.Errorf("create-only GET list: %d, want 403", code)
	}
	if code := call(http.MethodGet, "/api/connections/batch", "", createOnly); code != http.StatusForbidden {
		t.Errorf("create-only GET export: %d, want 403", code)
	}
	if code := call(http.MethodDelete, "/api/connections/prod", "", createOnly); code != http.StatusForbidden {
		t.Errorf("create-only DELETE: %d, want 403", code)
	}

	// A read token reads but cannot write.
	if code := call(http.MethodGet, "/api/connections/prod", "", readOnly); code != http.StatusOK {
		t.Errorf("reader GET single: %d, want 200", code)
	}
	if code := call(http.MethodPut, "/api/connections/other", seed, readOnly); code != http.StatusForbidden {
		t.Errorf("reader PUT: %d, want 403", code)
	}

	// The operator principal (no Tools list) keeps passing every route it
	// always passed — this is the case the review flagged as untested.
	op := &auth.Principal{ID: "operator", Kind: auth.KindOperator}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/connections/prod", http.StatusOK},
		{http.MethodGet, "/api/connections", http.StatusOK},
		{http.MethodGet, "/api/connections/batch", http.StatusOK},
		{http.MethodPut, "/api/connections/prod", http.StatusNoContent},
		{http.MethodDelete, "/api/connections/prod", http.StatusNoContent},
	} {
		if code := call(tc.method, tc.path, seed, op); code != tc.want {
			t.Errorf("operator %s %s: %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
}

func TestVaultRoutesRefuseAgentPrincipal(t *testing.T) {
	v := vault.New(t.TempDir())
	if _, err := v.Init("vault gate test"); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	h := &Handler{Vault: v}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vault", h.handleVaultStatus)
	mux.HandleFunc("POST /api/vault/unlock", h.handleVaultUnlock)
	mux.HandleFunc("POST /api/vault/lock", h.handleVaultLock)

	agent := &auth.Principal{ID: "t", Kind: auth.KindAgent, Tools: []string{"profile.create"}}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/vault"},
		{http.MethodPost, "/api/vault/unlock"},
		{http.MethodPost, "/api/vault/lock"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"passphrase":"x"}`))
		req = req.WithContext(auth.WithPrincipal(req.Context(), agent))
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		if out.Code != http.StatusForbidden {
			t.Errorf("agent %s %s: %d, want 403", tc.method, tc.path, out.Code)
		}
	}
}
