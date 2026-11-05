package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/pkg/api"
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

	// 3. DELETE without a protocol session is refused.
	req = httptest.NewRequest(http.MethodDelete, "/ext/testext/stream", nil)
	req.SetPathValue("name", "testext")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on DELETE without session, got %d", w.Code)
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

	t.Run("missing shell.output is checked before writing", func(t *testing.T) {
		test(t, v2Policy("ext.get", "shell.input", "shell.key"), "shell.output", 0)
	})

	t.Run("ask ext.get is not orphaned", func(t *testing.T) {
		p := v2Policy("ext.get", "shell.input", "shell.key", "shell.output")
		p.Actions["ext.get"] = authorization.Grant{Effect: authorization.EffectAsk}
		test(t, p, "requires approval", 0)
	})
}

func TestExtMCPStreamSeparatesAgentSessionsAndReplies(t *testing.T) {
	h := newV2Harness(t, v2Policy("ext.get", "shell.input", "shell.key", "shell.output"))
	installLineEchoExtension(t, h, true)
	var second struct {
		Token string `json:"token"`
	}
	h.adminRequest("POST", "/auth/principals", map[string]any{
		"name":        "second-extension-agent",
		"group_id":    h.groupID,
		"issue_token": true,
	}, &second)
	if second.Token == "" || second.Token == h.token {
		t.Fatal("second Agent credential was not issued")
	}

	code, firstSession, body, err := postExtensionMCP(h.url, h.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err != nil || code != http.StatusOK || firstSession == "" || !strings.Contains(body, `"id":1`) {
		t.Fatalf("first initialize = %d session=%q body=%q err=%v", code, firstSession, body, err)
	}
	code, secondSession, body, err := postExtensionMCP(h.url, second.Token, "", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`)
	if err != nil || code != http.StatusOK || secondSession == "" || secondSession == firstSession || !strings.Contains(body, `"id":2`) {
		t.Fatalf("second initialize = %d session=%q body=%q err=%v", code, secondSession, body, err)
	}

	type response struct {
		code       int
		body       string
		err        error
		credential string
	}
	responses := make(chan response, 2)
	var wg sync.WaitGroup
	for _, request := range []struct {
		token, session, frame string
	}{
		{h.token, firstSession, `{"jsonrpc":"2.0","id":101,"method":"ping"}`},
		{second.Token, secondSession, `{"jsonrpc":"2.0","id":202,"method":"ping"}`},
	} {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, body, err := postExtensionMCP(h.url, request.token, request.session, request.frame)
			responses <- response{code: code, body: body, err: err, credential: request.token}
		}()
	}
	wg.Wait()
	close(responses)
	seen := map[string]bool{}
	for response := range responses {
		if response.err != nil || response.code != http.StatusOK {
			t.Fatalf("concurrent extension call = %d %q: %v", response.code, response.body, response.err)
		}
		switch response.credential {
		case h.token:
			if strings.Contains(response.body, `"id":202`) || !strings.Contains(response.body, `"id":101`) {
				t.Fatalf("first Agent consumed another session's reply: %s", response.body)
			}
		case second.Token:
			if strings.Contains(response.body, `"id":101`) || !strings.Contains(response.body, `"id":202`) {
				t.Fatalf("second Agent consumed another session's reply: %s", response.body)
			}
		}
		seen[response.credential] = true
	}
	if len(seen) != 2 {
		t.Fatal("both Agent replies were not observed")
	}

	sameSession := make(chan response, 6)
	for id := 401; id <= 406; id++ {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, id)
			code, _, body, err := postExtensionMCP(h.url, h.token, firstSession, frame)
			sameSession <- response{code: code, body: body, err: err, credential: strconv.Itoa(id)}
		}()
	}
	wg.Wait()
	close(sameSession)
	for response := range sameSession {
		want := `"id":` + response.credential
		if response.err != nil || response.code != http.StatusOK || !strings.Contains(response.body, want) {
			t.Fatalf("same-session serialized call = %d %q: %v", response.code, response.body, response.err)
		}
	}

	code, _, body, err = postExtensionMCP(h.url, second.Token, firstSession, `{"jsonrpc":"2.0","id":303,"method":"ping"}`)
	if err != nil || code != http.StatusForbidden {
		t.Fatalf("cross-credential session use = %d %q: %v", code, body, err)
	}
	if code, err = deleteExtensionMCP(h.url, h.token, firstSession); err != nil || code != http.StatusNoContent {
		t.Fatalf("DELETE owner session = %d: %v", code, err)
	}
	if code, _, _, err = postExtensionMCP(h.url, h.token, firstSession, `{"jsonrpc":"2.0","id":404,"method":"ping"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST after DELETE = %d: %v", code, err)
	}
	if code, err = deleteExtensionMCP(h.url, second.Token, secondSession); err != nil || code != http.StatusNoContent {
		t.Fatalf("DELETE second session = %d: %v", code, err)
	}
}

func TestExtMCPRejectsHostScopeBeforeAcquire(t *testing.T) {
	policy := v2Policy("ext.get", "shell.input", "shell.key", "shell.output")
	policy.HostMode = "selected"
	policy.Hosts = []string{"another-host"}
	h := newV2Harness(t, policy)
	installLineEchoExtension(t, h, false)
	code, _, body, err := postExtensionMCP(h.url, h.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err != nil || code != http.StatusForbidden || !strings.Contains(body, "outside this Agent's scope") {
		t.Fatalf("host scope response = %d %q: %v", code, body, err)
	}
	got, ok := h.server.extensions.Status("echo")
	if !ok || got.ShellID != "" || got.Instances != 0 {
		t.Fatalf("host-scope denial started extension: %+v", got)
	}
}

func installLineEchoExtension(t *testing.T, h *v2Harness, resident bool) {
	t.Helper()
	command, args := "cat", []string{}
	if runtime.GOOS == "windows" {
		command = "powershell.exe"
		args = []string{"-NoLogo", "-NoProfile", "-Command", `while (($line = [Console]::In.ReadLine()) -ne $null) { [Console]::Out.WriteLine($line) }`}
	}
	store := extensions.NewStore(t.TempDir())
	if err := store.Save(&extensions.Extension{
		Name:     "echo",
		Target:   "internal",
		Mode:     string(api.ModePipe),
		Command:  command,
		Args:     args,
		Resident: resident,
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := extensions.NewReconciler(store, h.server.sessMgr, h.server.msgMgr, h.vault, forward.NewForwardManager())
	h.server.SetExtensionReconciler(reconciler)
	if err := reconciler.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reconciler.Stop)
}

func postExtensionMCP(baseURL, token, session, frame string) (int, string, string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/ext/echo/stream", strings.NewReader(frame))
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set(extMCPSessionHeader, session)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, resp.Header.Get(extMCPSessionHeader), "", err
	}
	return resp.StatusCode, resp.Header.Get(extMCPSessionHeader), string(body), nil
}

func deleteExtensionMCP(baseURL, token, session string) (int, error) {
	req, err := http.NewRequest(http.MethodDelete, baseURL+"/ext/echo/stream", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(extMCPSessionHeader, session)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
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
