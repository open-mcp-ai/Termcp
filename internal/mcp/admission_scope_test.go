package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

// startPlainSession opens an internal-profile session without review mode.
func startPlainSession(t *testing.T, s *Server) (string, string) {
	t.Helper()
	startReq := makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	})
	res, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	return m["session_id"].(string), m["shell_id"].(string)
}

func TestScopedTokenToolsWhitelist(t *testing.T) {
	s := newTestServer(t)
	sessionID, shellID := startPlainSession(t, s)

	// A token scoped to reads must not write.
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "t-read", Kind: auth.KindAgent, Tools: []string{"shell.output"},
	})
	res, held := s.checkAdmission(ctx, "shell_input", makeRequest(map[string]any{
		"shell_id": shellID, "text": "echo hi",
	}))
	if held {
		t.Fatal("a refused call must not be held for review")
	}
	if res == nil || !res.IsError {
		t.Fatalf("shell_input outside the tools whitelist: got %+v", res)
	}

	// The same call with the write capability in scope passes admission.
	ctxW := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "t-write", Kind: auth.KindAgent, Tools: []string{"shell.input", "shell.key"},
	})
	res, _ = s.checkAdmission(ctxW, "shell_input", makeRequest(map[string]any{
		"shell_id": shellID, "text": "echo hi",
	}))
	if res != nil {
		t.Fatalf("in-scope call refused: %s", toolResultText(res))
	}
	_ = sessionID
}

func TestScopedTokenSessionsWhitelist(t *testing.T) {
	s := newTestServer(t)
	sessionID, shellID := startPlainSession(t, s)

	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "t-other", Kind: auth.KindAgent, Sessions: []string{"somebody-else"},
	})
	res, _ := s.checkAdmission(ctx, "shell_output", makeRequest(map[string]any{"session_id": sessionID}))
	if res == nil || !res.IsError {
		t.Fatalf("session outside the whitelist: got %+v", res)
	}

	// The profile name that established the session matches too.
	ctxP := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "t-internal", Kind: auth.KindAgent, Sessions: []string{"internal"},
	})
	res, _ = s.checkAdmission(ctxP, "shell_output", makeRequest(map[string]any{"session_id": sessionID}))
	if res != nil {
		t.Fatalf("profile-name session scope refused: %s", toolResultText(res))
	}
	_ = shellID
}

func TestScopedTokenApprovalRequiredForcesReview(t *testing.T) {
	s := newTestServer(t)
	sessionID, _ := startPlainSession(t, s)

	sess := s.sessMgr.Get(sessionID)
	if sess.ApprovalEnabled() {
		t.Fatal("session unexpectedly starts gated")
	}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "t-review", Kind: auth.KindAgent, ApprovalRequired: true,
	})
	res, held := s.checkAdmission(ctx, "file_mkdir", makeRequest(map[string]any{
		"session_id": sessionID, "remote_path": "/tmp/termcp-scope-test",
	}))
	if !held {
		t.Fatalf("approval_required write was not held: %+v", res)
	}
	if !sess.ApprovalEnabled() {
		t.Fatal("review mode was not engaged by an approval_required token")
	}
	// The held operation landed in the session's queue for a human.
	q := sess.ApprovalQueue()
	if q == nil || len(q.List()) != 1 {
		t.Fatalf("queue: %+v", q)
	}
	s.sessMgr.Terminate(sessionID, true, 50*time.Millisecond)
}
