package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
)

// TestExtMCPHandlerValidation pins the HTTP-level contracts of /ext/{name}/stream (issue #234).
func TestExtMCPHandlerValidation(t *testing.T) {
	s := &Server{}
	handler := s.ExtMCPHandler()

	// 1. GET is refused with 405 and teaches POST
	req := httptest.NewRequest(http.MethodGet, "/ext/testext/stream", nil)
	req.SetPathValue("name", "testext")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "POST your JSON-RPC frames") {
		t.Errorf("refusal should teach POST, got: %q", w.Body.String())
	}

	// 2. Non-JSON body is refused with 400
	req = httptest.NewRequest(http.MethodPost, "/ext/testext/stream", bytes.NewBufferString("not-json"))
	req.SetPathValue("name", "testext")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on non-JSON, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not JSON") {
		t.Errorf("refusal should mention not JSON, got: %q", w.Body.String())
	}

	// 3. DELETE reports 204
	req = httptest.NewRequest(http.MethodDelete, "/ext/testext/stream", nil)
	req.SetPathValue("name", "testext")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on DELETE, got %d", w.Code)
	}
}

func TestExtMCPAuthorizesAgentBeforeStartingExtension(t *testing.T) {
	test := func(t *testing.T, policy auth.AgentPolicy, want string, wantPending int) {
		t.Helper()
		h := newV2Harness(t, policy)
		h.server.SetExtensionReconciler(extensions.NewReconciler(
			extensions.NewStore(t.TempDir()), h.server.sessMgr, h.server.msgMgr, h.vault, forward.NewForwardManager(),
		))
		ctx := agentPolicyContext(t, h)
		req := httptest.NewRequest(http.MethodPost, "/ext/example/stream",
			bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.SetPathValue("name", "example")
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		h.server.ExtMCPHandler().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("status/body = %d %q, want 403 containing %q", w.Code, w.Body.String(), want)
		}
		if got := len(h.server.ListPolicyApprovals()); got != wantPending {
			t.Fatalf("pending approvals = %d, want %d", got, wantPending)
		}
	}

	t.Run("missing ext.get", func(t *testing.T) {
		test(t, auth.EmptyAgentPolicy(), "operation \"ext.get\" is not granted", 0)
	})

	t.Run("ask ext.get is not orphaned", func(t *testing.T) {
		p := v2Policy("ext.get")
		p.Actions["ext.get"] = authorization.Grant{Effect: authorization.EffectAsk}
		test(t, p, "ext.get requires approval", 0)
	})
}

func agentPolicyContext(t *testing.T, h *v2Harness) context.Context {
	t.Helper()
	remainder := strings.TrimPrefix(h.token, "tmcp_")
	credentialID, _, ok := strings.Cut(remainder, ".")
	if !ok || credentialID == "" {
		t.Fatalf("unexpected Agent token shape")
	}
	ctx, err := h.account.ApprovalContextForCredential(t.Context(), h.agentID, credentialID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestExtensionMCPInnerAuthorizationIsBoundToAcquiredShell(t *testing.T) {
	ctx := withExtensionMCPExecution(context.Background(), "ext:internal", "shell-1")
	if got := callerNamespace(ctx); got != "ext:internal" {
		t.Fatalf("caller namespace = %q, want the acquired extension namespace", got)
	}
	if !extensionMCPToolAuthorized(ctx, "shell_input", makeRequest(map[string]any{"shell_id": "shell-1"})) {
		t.Fatal("the acquired extension shell must be admitted")
	}
	if !extensionMCPToolAuthorized(ctx, "shell_key", makeRequest(map[string]any{"shell_id": "shell-1"})) {
		t.Fatal("the acquired extension shell must accept the ending key")
	}
	if extensionMCPToolAuthorized(ctx, "shell_input", makeRequest(map[string]any{"shell_id": "shell-2"})) {
		t.Fatal("the ext.get bypass escaped to another shell")
	}
	if extensionMCPToolAuthorized(ctx, "shell_output", makeRequest(map[string]any{"shell_id": "shell-1"})) {
		t.Fatal("the bypass admitted a tool outside the extension frame write path")
	}
}
