package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/approval"
)

// startGatedSession opens a session and turns approval mode on, returning the
// session id and shell id.
func startGatedSession(t *testing.T, s *Server, need int, timeout time.Duration) (string, string) {
	t.Helper()
	startReq := makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)
	shellID := m["shell_id"].(string)

	live := s.sessMgr.Get(sessionID)
	if live == nil {
		t.Fatal("session missing from the manager")
	}
	if err := live.EnableApproval(need, timeout); err != nil {
		t.Fatalf("EnableApproval: %v", err)
	}
	return sessionID, shellID
}

// Under review mode shell_input must not write and must not queue either: the
// command line is not complete until an ending key arrives.
func TestHandleSendInputHoldsUnderReview(t *testing.T) {
	s := newTestServer(t)
	_, shellID := startGatedSession(t, s, 1, time.Minute)

	result, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     "echo HELLO",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, result)
	if m["review_pending"] != true {
		t.Errorf("review_pending = %v, want true", m["review_pending"])
	}
	if m["approved"] != false {
		t.Errorf("approved = %v, want false", m["approved"])
	}
	// No id: the agent cannot decide the request, so an id would only invite a
	// retry loop.
	if _, has := m["pending_id"]; has {
		t.Error("a held input must not hand back a pending_id")
	}
	// The text is staged, not queued: nothing is reviewable until it ends.
	q := s.sessMgr.Get(sessionIDOf(t, s, shellID)).ApprovalQueue()
	if q == nil {
		t.Fatal("expected a queue")
	}
	if reqs := q.List(); len(reqs) != 0 {
		t.Errorf("staged text must not be queued yet, got %d request(s)", len(reqs))
	}
}

// shell_key(enter) commits the staged text as one reviewable command line.
func TestHandlePressKeyCommitsStagedText(t *testing.T) {
	s := newTestServer(t)
	sid, shellID := startGatedSession(t, s, 1, time.Minute)

	if _, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     "echo ONE_LINE",
	})); err != nil {
		t.Fatal(err)
	}
	keyResult, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"key":      "enter",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if m := parseResult(t, keyResult); m["review_pending"] != true {
		t.Errorf("review_pending = %v, want true", m["review_pending"])
	}

	q := s.sessMgr.Get(sid).ApprovalQueue()
	reqs := q.List()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 queued command line, got %d", len(reqs))
	}
	// The text and the newline are one unit, so a reviewer cannot approve the
	// text and reject the newline.
	if reqs[0].Text != "echo ONE_LINE" {
		t.Errorf("queued text = %q, want %q", reqs[0].Text, "echo ONE_LINE")
	}
	if len(reqs[0].Keys) != 1 || reqs[0].Keys[0] != "enter" {
		t.Errorf("queued keys = %v, want [enter]", reqs[0].Keys)
	}
}

// Multi-line staging: two shell_input calls before the enter are one command.
func TestStagedTextAccumulatesUntilEnter(t *testing.T) {
	s := newTestServer(t)
	sid, shellID := startGatedSession(t, s, 1, time.Minute)

	for _, part := range []string{"echo ", "TWO", "_PARTS"} {
		if _, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
			"shell_id": shellID,
			"text":     part,
		})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"key":      "enter",
	})); err != nil {
		t.Fatal(err)
	}

	reqs := s.sessMgr.Get(sid).ApprovalQueue().List()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if reqs[0].Text != "echo TWO_PARTS" {
		t.Errorf("accumulated text = %q, want %q", reqs[0].Text, "echo TWO_PARTS")
	}
}

// A bare key with nothing staged is still reviewable: ctrl+c interrupts a
// running process, which a reviewer must see.
func TestBareKeyIsItsOwnRequest(t *testing.T) {
	s := newTestServer(t)
	sid, shellID := startGatedSession(t, s, 1, time.Minute)

	if _, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"key":      "ctrl+c",
	})); err != nil {
		t.Fatal(err)
	}
	reqs := s.sessMgr.Get(sid).ApprovalQueue().List()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if reqs[0].Text != "" {
		t.Errorf("a bare key request must carry no text, got %q", reqs[0].Text)
	}
	if len(reqs[0].Keys) != 1 || reqs[0].Keys[0] != "ctrl+c" {
		t.Errorf("keys = %v, want [ctrl+c]", reqs[0].Keys)
	}
}

// repeat folds into the key list, so the reviewer sees exactly what will run.
func TestBareKeyRepeatIsReviewable(t *testing.T) {
	s := newTestServer(t)
	sid, shellID := startGatedSession(t, s, 1, time.Minute)

	if _, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"key":      "enter",
		"repeat":   3,
	})); err != nil {
		t.Fatal(err)
	}
	reqs := s.sessMgr.Get(sid).ApprovalQueue().List()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if len(reqs[0].Keys) != 3 {
		t.Errorf("keys = %v, want 3 entries", reqs[0].Keys)
	}
}

// An ungated session writes straight through, unchanged.
func TestHandleSendInputWritesWhenUngated(t *testing.T) {
	s := newTestServer(t)
	startReq := makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	shellID := parseResult(t, startResult)["shell_id"].(string)

	result, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     "echo DIRECT",
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := parseResult(t, result)
	if body["review_pending"] == true {
		t.Error("an ungated write must not be reported as pending review")
	}
	if _, has := body["pending_id"]; has {
		t.Error("an ungated write must not return a pending_id")
	}
}

// Turning review off must discard staged text: it was never reviewable, and
// leaving it would let it reappear under the next policy.
func TestDisablingReviewDiscardsStagedText(t *testing.T) {
	s := newTestServer(t)
	sid, shellID := startGatedSession(t, s, 1, time.Minute)

	if _, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     "echo NEVER_SENT",
	})); err != nil {
		t.Fatal(err)
	}
	live := s.sessMgr.Get(sid)
	live.DisableApproval()
	if err := live.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}

	// The next enter must not resurrect the discarded text.
	if _, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"key":      "enter",
	})); err != nil {
		t.Fatal(err)
	}
	reqs := live.ApprovalQueue().List()
	for _, r := range reqs {
		if strings.Contains(r.Text, "NEVER_SENT") {
			t.Errorf("discarded text reappeared after re-enabling: %q", r.Text)
		}
	}
}

// The approval tool is gone: an agent must not be able to decide its own
// requests, or review mode is decorative.
func TestApprovalToolIsNotRegistered(t *testing.T) {
	s := newTestServer(t)
	for _, tool := range s.mcpServer.ListTools() {
		if tool.Tool.Name == "approval" {
			t.Fatal("the approval tool must not exist: an agent that can approve itself is not gated")
		}
	}
}

// sessionIDOf finds the session that owns a shell.
func sessionIDOf(t *testing.T, s *Server, shellID string) string {
	t.Helper()
	for _, info := range s.sessMgr.ListAll() {
		if sess := s.sessMgr.Get(info.ID); sess != nil && sess.GetChildShell(shellID) != nil {
			return info.ID
		}
	}
	t.Fatalf("no session owns shell %s", shellID)
	return ""
}

// reviewPendingOperationResult is the reply a held file or forward operation
// gets. It must carry no pending id for the same reason the command-line reply
// does not: the agent cannot decide its own request, so an id only invites a
// retry loop.
func TestHeldOperationReturnsNoPendingID(t *testing.T) {
	res := reviewPendingOperationResult("write 1.2 KB to /etc/hosts")
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &body); err != nil {
		t.Fatal(err)
	}
	if body["review_pending"] != true {
		t.Errorf("review_pending = %v, want true", body["review_pending"])
	}
	if body["approved"] != false {
		t.Errorf("approved = %v, want false", body["approved"])
	}
	if _, has := body["pending_id"]; has {
		t.Error("a held operation must not hand back a pending_id")
	}
	// The message has to name what is waiting: the agent's next step depends on
	// knowing the operation was understood, not silently dropped.
	if msg, _ := body["message"].(string); !strings.Contains(msg, "/etc/hosts") {
		t.Errorf("message does not describe the held operation: %q", msg)
	}
}

// A reviewer decides on the summary, so it has to name the operation and its
// target. These are the cases where a vague summary would make the gate
// decorative: "write to a file" is not a decision anyone can make.
func TestOperationSummaryNamesTheOperationAndItsTarget(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"file_write", map[string]any{"remote_path": "/etc/hosts", "data": strings.Repeat("x", 2048)}, []string{"/etc/hosts", "2.0 KB"}},
		{"file_write", map[string]any{"remote_path": "/tmp/a", "local_path": "/local/b"}, []string{"/tmp/a", "/local/b"}},
		{"file_delete", map[string]any{"remote_path": "/etc/hosts"}, []string{"delete", "/etc/hosts"}},
		{"file_rename", map[string]any{"from_path": "/a", "to_path": "/b"}, []string{"/a", "/b"}},
		{"file_mkdir", map[string]any{"remote_path": "/srv/new"}, []string{"/srv/new"}},
		{"file_perm", map[string]any{"remote_path": "/x", "action": "chmod", "mode": float64(493)}, []string{"chmod", "0755", "/x"}},
		{"file_perm", map[string]any{"remote_path": "/x", "action": "chown", "uid": float64(0), "gid": float64(0)}, []string{"chown", "/x"}},
		{"file_link", map[string]any{"action": "symlink", "link_path": "/l", "target": "/t"}, []string{"/l", "/t"}},
		{"file_fs", map[string]any{"remote_path": "/big", "size": float64(0)}, []string{"truncate", "/big", "0"}},
		{"forward", map[string]any{"action": "local", "local_port": float64(8080), "remote_host": "db", "remote_port": float64(5432)}, []string{":8080", "db:5432"}},
		{"forward", map[string]any{"action": "dynamic", "local_port": float64(1080)}, []string{"SOCKS5", "1080"}},
		{"forward", map[string]any{"action": "close", "forward_id": "f-1"}, []string{"close", "f-1"}},
	}
	for _, tc := range cases {
		got := summarizeOperation(tc.tool, tc.args)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s(%v) = %q, missing %q", tc.tool, tc.args, got, want)
			}
		}
	}
	// A delete summary must say delete, not just name the path: the verb is the
	// part a reviewer is deciding on.
	if got := summarizeOperation("file_delete", map[string]any{"remote_path": "/x"}); !strings.HasPrefix(got, "delete") {
		t.Errorf("delete summary = %q, want it to lead with the verb", got)
	}
}

// Reads must NOT be gated. Putting a directory listing behind a decision trains
// a reviewer to click Accept without reading, which is how a gate stops being a
// gate — and the failure is invisible, because everything still "works".
func TestReadOnlyToolsAreNotGated(t *testing.T) {
	gated := map[approval.Kind]bool{}
	for _, g := range gatedTools {
		gated[g.kind] = true
	}
	for _, tool := range []string{"file_read", "file_stat", "file_urls", "file_getwd", "shell_output", "shell_list"} {
		for _, g := range gatedTools {
			if g.tool == tool {
				t.Errorf("%s is a read and must not be gated", tool)
			}
		}
	}
	// The gate has to cover both kinds of change, or the hole this test exists
	// for is back: review that covers the terminal but not the filesystem.
	if !gated[approval.KindFileWrite] {
		t.Error("file writes must be gated: an irreversible change is exactly what review is for")
	}
	if !gated[approval.KindFileDelete] {
		t.Error("file deletes must be gated")
	}
	if !gated[approval.KindForwardOpen] {
		t.Error("opening a port forward must be gated: it exposes the host")
	}
}

// A gated session must hold a file write, and approving it must actually
// perform the write.
//
// This is the end-to-end proof that review covers more than the terminal. Both
// halves matter and fail differently: if the write is not held, review has a
// hole that reads as protection; if the approval does not execute, the reviewer
// believes they let something through that never happened.
func TestFileWriteIsHeldAndExecutedOnApproval(t *testing.T) {
	s := newTestServer(t)
	sessionID, _ := startGatedSession(t, s, 1, time.Minute)

	// The file must not exist before: the assertion is on the remote host, not
	// on a queue entry, because "held" has to mean "nothing happened".
	dir, bad := s.sftpClient(sessionID)
	if bad != nil {
		t.Fatalf("sftp: %v", bad)
	}
	target := filepath.Join(t.TempDir(), "termcp-approval-probe.txt")
	_ = dir.RemoveFile(target)
	_ = dir.Close()

	// The write is submitted through the tool, the way an agent would.
	res, err := s.handleFileWrite(context.Background(), makeRequest(map[string]any{
		"session_id":  sessionID,
		"remote_path": target,
		"data":        "approved-content",
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := parseResult(t, res)
	if body["review_pending"] != true {
		t.Fatalf("a gated file write was not held: %v", body)
	}
	if _, has := body["pending_id"]; has {
		t.Error("the held reply must not carry a pending_id")
	}

	// Nothing may have been written yet.
	chk, _ := s.sftpClient(sessionID)
	if _, statErr := chk.StatFile(target); statErr == nil {
		_ = chk.RemoveFile(target)
		_ = chk.Close()
		t.Fatal("the file exists before approval: the write was not really held")
	}
	_ = chk.Close()

	// The queue must describe it in a way a human can decide on.
	q := s.sessMgr.Get(sessionID).ApprovalQueue()
	if q == nil {
		t.Fatal("no approval queue")
	}
	pend := q.List()
	if len(pend) != 1 {
		t.Fatalf("queued %d requests, want 1", len(pend))
	}
	req := pend[0]
	if req.Kind != approval.KindFileWrite {
		t.Errorf("kind = %q, want %q", req.Kind, approval.KindFileWrite)
	}
	if !strings.Contains(req.Summary, target) {
		t.Errorf("summary %q does not name the target, so a reviewer cannot decide", req.Summary)
	}
	if req.Text != "" {
		t.Errorf("a file write carries no command text, got %q", req.Text)
	}

	// Approving must execute it. This replays through the same handler that
	// submitted it, which is the property that keeps one implementation.
	if _, err := q.Approve(req.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.ExecuteApprovedOperation(req); err != nil {
		t.Fatalf("executing the approved write: %v", err)
	}

	chk2, _ := s.sftpClient(sessionID)
	defer chk2.Close()
	f, err := chk2.ReadFile(target, 0, 0, "text", "")
	if err != nil {
		t.Fatalf("the approved write did not happen: %v", err)
	}
	if f == nil {
		t.Fatal("the approved write produced no readable file")
	}
	got := fmt.Sprintf("%v", f.Data)
	if !strings.Contains(got, "approved-content") {
		t.Errorf("file content = %q, want it to contain %q", got, "approved-content")
	}
	_ = chk2.RemoveFile(target)
}

// A rejection must leave the host untouched. A gate whose "no" still performed
// the operation would be worse than no gate at all.
func TestFileWriteIsNotExecutedOnRejection(t *testing.T) {
	s := newTestServer(t)
	sessionID, _ := startGatedSession(t, s, 1, time.Minute)

	target := filepath.Join(t.TempDir(), "termcp-approval-rejected.txt")
	cli, _ := s.sftpClient(sessionID)
	_ = cli.RemoveFile(target)
	_ = cli.Close()

	if _, err := s.handleFileWrite(context.Background(), makeRequest(map[string]any{
		"session_id":  sessionID,
		"remote_path": target,
		"data":        "must-not-appear",
	})); err != nil {
		t.Fatal(err)
	}

	q := s.sessMgr.Get(sessionID).ApprovalQueue()
	req := q.List()[0]
	decided, err := q.Reject(req.ID, "no")
	if err != nil {
		t.Fatal(err)
	}
	if decided.State != approval.Rejected {
		t.Fatalf("state = %q, want rejected", decided.State)
	}
	// Even a caller that ignored the rejection and replayed it must not write:
	// the executor is only reached through the decision endpoint, and this
	// asserts the file is absent after the decision.
	chk, _ := s.sftpClient(sessionID)
	defer chk.Close()
	if _, statErr := chk.StatFile(target); statErr == nil {
		_ = chk.RemoveFile(target)
		t.Fatal("a rejected write still created the file")
	}
}

// A failed approved operation must explain itself in one readable sentence.
//
// A failed tool result carries a JSON envelope ({"error_code":…,"error":…}) so
// agents can branch on the kind without parsing prose. But this path shows the
// failure to the HUMAN who just approved the operation, and the envelope leaked
// into that message verbatim:
//
//	approved but execution failed: {"error_code":"operation_failed","error":"remove /tmp/x: file does not exist"}
//
// The one sentence that matters was wrapped in JSON. The envelope is still
// produced for agents; only the human-facing rendering unwraps it.
func TestApprovedOperationFailureReadsAsASentence(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "the standard envelope",
			text: `{"error_code":"operation_failed","error":"remove /tmp/x: file does not exist"}`,
			want: "remove /tmp/x: file does not exist",
		},
		{
			name: "an envelope with surrounding whitespace",
			text: `  {"error_code":"shell_not_found","error":"Shell 'abc' not found"}  `,
			want: "Shell 'abc' not found",
		},
		{
			name: "plain prose is passed through",
			text: "something went wrong",
			want: "something went wrong",
		},
		{
			name: "JSON that is not an error envelope is not swallowed",
			text: `{"download_url":"http://x/y"}`,
			want: `{"download_url":"http://x/y"}`,
		},
		{
			name: "malformed JSON is not swallowed",
			text: `{"error_code":`,
			want: `{"error_code":`,
		},
	}
	for _, tc := range cases {
		got := unwrapToolError(tc.text)
		if got != tc.want {
			t.Errorf("%s: unwrapToolError(%q) = %q, want %q", tc.name, tc.text, got, tc.want)
		}
	}

	// And the message the human sees must not contain the envelope's keys.
	res := toolError(CodeOperationFailed, "remove %s: file does not exist", "/tmp/x")
	msg := toolResultText(res)
	for _, gone := range []string{"error_code", "{\"", "\":"} {
		if strings.Contains(msg, gone) {
			t.Errorf("the human-facing message still looks like JSON (%q contains %q)", msg, gone)
		}
	}
	if !strings.Contains(msg, "file does not exist") {
		t.Errorf("the message lost the reason: %q", msg)
	}
}
