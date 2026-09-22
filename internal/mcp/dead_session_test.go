package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// waitForExited polls the session registry until the session reports status
// exited (DEAD), or the timeout elapses.
func waitForExited(t *testing.T, s *Server, sid string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		infoRes, _ := s.handleGetSessionInfo(context.Background(), makeRequest(map[string]any{"session_id": sid}))
		if !infoRes.IsError && strings.Contains(infoRes.Content[0].(mcpgo.TextContent).Text, `"status":"exited"`) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// assertErrorCode fails unless res is an error result whose error_code is want.
func assertErrorCode(t *testing.T, res *mcpgo.CallToolResult, want, what string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("%s: expected an error result, got %s", what, res.Content[0].(mcpgo.TextContent).Text)
	}
	var body struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &body); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if body.ErrorCode != want {
		t.Fatalf("%s: error_code = %q, want %q (%s)", what, body.ErrorCode, want, body.Error)
	}
}

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

	// The internal profile starts a login shell, whose startup (prompt themes,
	// rc files) can still be in flight when the first line is typed; input sent
	// before the shell is ready is dropped. Retry until the marker appears so this
	// asserts the DEAD-read contract, not shell startup speed.
	deadline := time.Now().Add(30 * time.Second)
	var output string
	for time.Now().Before(deadline) {
		testRunLine(t, s, shellID, "echo DEAD-READ-MARKER")
		output = testReadOutputUntil(t, s, shellID, "DEAD-READ-MARKER", 3*time.Second)
		if strings.Contains(output, "DEAD-READ-MARKER") {
			break
		}
	}
	if !strings.Contains(output, "DEAD-READ-MARKER") {
		t.Fatalf("session never echoed the marker line, got %q", output)
	}

	termReq := makeRequest(map[string]any{"session_id": sid, "force": true})
	if _, err := s.handleTerminateSession(context.Background(), termReq); err != nil {
		t.Fatal(err)
	}
	if !waitForExited(t, s, sid, 5*time.Second) {
		t.Fatal("session did not turn DEAD in time")
	}
	return sid
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
	assertErrorCode(t, readRes, "session_not_running", "file_read on DEAD session")

	// file_urls is a file tool too: same guard, same code.
	urlsReq := makeRequest(map[string]any{"session_id": sid, "remote_path": "/etc/hostname"})
	urlsRes, err := s.handleGetFileURLs(context.Background(), urlsReq)
	if err != nil {
		t.Fatal(err)
	}
	assertErrorCode(t, urlsRes, "session_not_running", "file_urls on DEAD session")

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
	assertErrorCode(t, fwdRes, "session_not_running", "forward on DEAD session")
	if s.forwardMgr != nil && len(s.forwardMgr.List()) != 0 {
		t.Fatalf("forward on DEAD session must not leave a listener, got %v", s.forwardMgr.List())
	}

	// shell_open on a DEAD session is refused the same way.
	openReq := makeRequest(map[string]any{"session_id": sid})
	openRes, err := s.handleStartSubShell(context.Background(), openReq)
	if err != nil {
		t.Fatal(err)
	}
	assertErrorCode(t, openRes, "session_not_running", "shell_open on DEAD session")

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

// TestCleanExitPipeSessionIsReadOnly pins the DEAD contract for the case that
// used to slip through: a pipe session whose command exits on its own. The
// container flips to exited without closing its SSH client (only terminate,
// disconnect, and Delete do), so the refusal below can only come from the
// session-status guard — never from a dead transport. That live-transport
// precondition is asserted, so a transport-liveness guard cannot pass this test
// by closing the connection earlier. The forward path shares the same guard
// (sshClientForSession → requireRunningSession) and stays covered by
// TestDeadSessionFileAndForwardRejected.
func TestCleanExitPipeSessionIsReadOnly(t *testing.T) {
	s := newTestServer(t)

	startRes, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    "echo",
		"mode":       "pipe",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	sid := parseResult(t, startRes)["session_id"].(string)

	if !waitForExited(t, s, sid, 5*time.Second) {
		t.Fatal("cleanly-exited pipe session did not turn DEAD in time")
	}
	if sess := s.sessMgr.Get(sid); sess == nil || sess.SSHClient() == nil {
		t.Fatal("precondition failed: a cleanly-exited session keeps its SSH transport until Delete")
	}

	// A file that exists on every platform: if the guard ever regresses, this
	// read succeeds instead of failing for an unrelated reason.
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	readRes, err := s.handleFileRead(context.Background(), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": path,
	}))
	if err != nil {
		t.Fatal(err)
	}
	assertErrorCode(t, readRes, "session_not_running", "file_read on a cleanly-exited session")
}
