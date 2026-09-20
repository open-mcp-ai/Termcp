package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// startTerminatedSession starts an internal session, prints a marker line into
// it (so the retained output is non-empty), terminates it with force, and waits
// until the registry reports it exited. The DEAD session keeps its output
// readable (via shell_output) but has a closed transport, which is exactly the
// state the file/forward guards must refuse.
func startTerminatedSession(t *testing.T, s *Server) string {
	t.Helper()
	startReq := makeRequest(map[string]any{"ssh_config": "internal"})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	sm := parseResult(t, startRes)
	sid := sm["session_id"].(string)
	shellID := sm["shell_id"].(string)

	testRunLine(t, s, shellID, "echo DEAD-READ-MARKER")
	testReadOutputUntil(t, s, shellID, "DEAD-READ-MARKER", 3*time.Second)

	termReq := makeRequest(map[string]any{"session_id": sid, "force": true})
	if _, err := s.handleTerminateSession(context.Background(), termReq); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		infoReq := makeRequest(map[string]any{"session_id": sid})
		infoRes, _ := s.handleGetSessionInfo(context.Background(), infoReq)
		if !infoRes.IsError && strings.Contains(infoRes.Content[0].(mcpgo.TextContent).Text, `"status":"exited"`) {
			return sid
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("session did not turn DEAD in time")
	return ""
}

// TestDeadSessionFileAndForwardRejected pins the documented contract: on an
// exited (DEAD) session the file and forward tools return session_not_running —
// never a low-level SFTP error, and never a zombie forward.
func TestDeadSessionFileAndForwardRejected(t *testing.T) {
	s := newTestServer(t)
	sid := startTerminatedSession(t, s)

	// file_read → session_not_running (not "SFTP: use of closed connection").
	readReq := makeRequest(map[string]any{"session_id": sid, "remote_path": "/etc/hostname"})
	readRes, err := s.handleFileRead(context.Background(), readReq)
	if err != nil {
		t.Fatal(err)
	}
	var errBody struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if !readRes.IsError {
		t.Fatal("file_read on DEAD session must error")
	}
	if err := json.Unmarshal([]byte(readRes.Content[0].(mcpgo.TextContent).Text), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.ErrorCode != "session_not_running" {
		t.Fatalf("file_read on DEAD session: error_code = %q, want session_not_running (%s)", errBody.ErrorCode, errBody.Error)
	}

	// file_urls is a file tool too: same guard, same code.
	urlsReq := makeRequest(map[string]any{"session_id": sid, "remote_path": "/etc/hostname"})
	urlsRes, err := s.handleGetFileURLs(context.Background(), urlsReq)
	if err != nil {
		t.Fatal(err)
	}
	if !urlsRes.IsError {
		t.Fatal("file_urls on DEAD session must error")
	}
	if err := json.Unmarshal([]byte(urlsRes.Content[0].(mcpgo.TextContent).Text), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.ErrorCode != "session_not_running" {
		t.Fatalf("file_urls on DEAD session: error_code = %q, want session_not_running", errBody.ErrorCode)
	}

	// forward(action=local) → session_not_running, no listener created.
	fwdReq := makeRequest(map[string]any{
		"action":      "local",
		"session_id":  sid,
		"remote_host": "localhost",
		"remote_port": float64(80),
		"local_port":  float64(0),
	})
	fwdRes, err := s.handleForwardOps(context.Background(), fwdReq)
	if err != nil {
		t.Fatal(err)
	}
	if !fwdRes.IsError {
		t.Fatalf("forward on DEAD session must error, got %s", fwdRes.Content[0].(mcpgo.TextContent).Text)
	}
	if err := json.Unmarshal([]byte(fwdRes.Content[0].(mcpgo.TextContent).Text), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.ErrorCode != "session_not_running" {
		t.Fatalf("forward on DEAD session: error_code = %q, want session_not_running", errBody.ErrorCode)
	}
	if s.forwardMgr != nil && len(s.forwardMgr.List()) != 0 {
		t.Fatalf("forward on DEAD session must not leave a listener, got %v", s.forwardMgr.List())
	}

	// shell_open on a DEAD session is refused the same way.
	openReq := makeRequest(map[string]any{"session_id": sid})
	openRes, err := s.handleStartSubShell(context.Background(), openReq)
	if err != nil {
		t.Fatal(err)
	}
	if !openRes.IsError {
		t.Fatal("shell_open on DEAD session must error")
	}
	if err := json.Unmarshal([]byte(openRes.Content[0].(mcpgo.TextContent).Text), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.ErrorCode != "session_not_running" {
		t.Fatalf("shell_open on DEAD session: error_code = %q, want session_not_running", errBody.ErrorCode)
	}

	// shell_output still reads the retained output of the closed session.
	outReq := makeRequest(map[string]any{"shell_id": sid, "tail_lines": float64(5)})
	outRes, err := s.handleReadOutput(context.Background(), outReq)
	if err != nil {
		t.Fatal(err)
	}
	if outRes.IsError {
		t.Fatalf("shell_output on DEAD session must keep working: %s", outRes.Content[0].(mcpgo.TextContent).Text)
	}
	var out struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(outRes.Content[0].(mcpgo.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "DEAD-READ-MARKER") {
		t.Fatalf("shell_output on DEAD session returned unexpected content: %q", out.Output)
	}
}

// TestForwardsCascadeOnSessionDead verifies the session→forward lifecycle
// wiring: when a session goes DEAD (here via session_terminate), every forward
// opened for it is closed automatically instead of lingering as a dead
// listener until the session is deleted.
func TestForwardsCascadeOnSessionDead(t *testing.T) {
	srv := startTestSSH(t)
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	cleanupTestRuntime(t, sessMgr, srv)
	fm := forward.NewForwardManager()
	s := New(sessMgr, msgMgr, sshconfig.NewStore(dir), fm, "test")

	startReq := makeRequest(map[string]any{"ssh_config": "internal"})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	sm := parseResult(t, startRes)
	sid := sm["session_id"].(string)

	fwdReq := makeRequest(map[string]any{
		"action":      "local",
		"session_id":  sid,
		"remote_host": "localhost",
		"remote_port": float64(80),
		"local_port":  float64(0),
	})
	if res, err := s.handleForwardOps(context.Background(), fwdReq); err != nil || res.IsError {
		t.Fatalf("forward on live session failed: %v %v", err, res)
	}
	if got := len(fm.List()); got != 1 {
		t.Fatalf("expected 1 live forward, got %d", got)
	}

	termReq := makeRequest(map[string]any{"session_id": sid, "force": true})
	if _, err := s.handleTerminateSession(context.Background(), termReq); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(fm.List()) != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(fm.List()); got != 0 {
		t.Fatalf("forwards must be closed when the session goes DEAD, still %d forward(s)", got)
	}
}
