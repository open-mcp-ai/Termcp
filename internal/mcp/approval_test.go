package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/capability"
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
	// Nothing is queued yet, so there is no approval to correlate.
	if _, has := m["pending_id"]; has {
		t.Error("a held input must not hand back a pending_id")
	}
	if _, has := m["approval_id"]; has {
		t.Error("staged input must not hand back an approval_id before the ending key")
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
	m := parseResult(t, keyResult)
	if m["review_pending"] != true {
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
	if m["approval_id"] != reqs[0].ID {
		t.Errorf("approval_id = %v, want %q", m["approval_id"], reqs[0].ID)
	}
	if m["session_id"] != sid {
		t.Errorf("session_id = %v, want %q", m["session_id"], sid)
	}
	if m["expires_at"] != float64(reqs[0].ExpiresAt) {
		t.Errorf("expires_at = %v, want %d", m["expires_at"], reqs[0].ExpiresAt)
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
// gets. The stable approval ID is correlation metadata, not a decision token.
func TestHeldOperationReturnsApprovalCorrelation(t *testing.T) {
	request := approval.Request{ID: "approval-1", SessionID: "session-1", ExpiresAt: 1793300000000}
	res := reviewPendingOperationResult("write 1.2 KB to /etc/hosts", &request)
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
	if body["approval_id"] != request.ID || body["session_id"] != request.SessionID {
		t.Errorf("approval correlation = %+v", body)
	}
	if body["expires_at"] != float64(request.ExpiresAt) {
		t.Errorf("expires_at = %v, want %d", body["expires_at"], request.ExpiresAt)
	}
	// The message has to name what is waiting: the agent's next step depends on
	// knowing the operation was understood, not silently dropped.
	if msg, _ := body["message"].(string); !strings.Contains(msg, "/etc/hosts") {
		t.Errorf("message does not describe the held operation: %q", msg)
	}
}

func TestReviewPendingOuterSlotCarriesCorrelationHeaders(t *testing.T) {
	res := jsonResult(map[string]any{
		"ok":             true,
		"approved":       false,
		"review_pending": true,
		"message":        "Submitted for review: write a file",
		"approval_id":    "approval-1",
		"session_id":     "session-1",
		"expires_at":     int64(1790129916410),
	})
	held := reviewPendingFrom(res)
	if held == nil {
		t.Fatal("reviewPendingFrom() = nil")
	}
	slot := held.slot("write session-1 file /tmp/x --data hi")
	if slot.Code != 202 {
		t.Fatalf("slot code = %d, want 202", slot.Code)
	}
	if slot.Headers["Approval-ID"] != "approval-1" ||
		slot.Headers["Session-ID"] != "session-1" ||
		slot.Headers["Expires-At"] != "1790129916410" {
		t.Fatalf("slot headers = %+v", slot.Headers)
	}
	if slot.Body != "Submitted for review: write a file" {
		t.Fatalf("slot body = %q", slot.Body)
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
		// NO file_write/local_path CASE ANY MORE. The summary used to fold the source path
		// into the line a reviewer reads; the flag is gone, so there is no second path to
		// name. Kept as a note rather than a silent deletion because a reviewer's summary
		// is exactly where a stale argument would look like a real one.
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
	for _, c := range capability.All() {
		if c.Kind != "" {
			gated[c.Kind] = true
		}
	}
	for _, tool := range []string{"file_read", "file_stat", "file_urls", "file_getwd", "shell_output", "shell_list"} {
		c, ok := capability.ForTool(tool)
		if !ok {
			t.Fatalf("%s has no capability row", tool)
		}
		if c.Kind != "" {
			t.Errorf("%s is a read and must not be gated", tool)
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
	res, err := s.call(context.Background(), "file_write", map[string]any{
		"session_id":  sessionID,
		"remote_path": target,
		"data":        "approved-content",
	})
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
	if body["approval_id"] != req.ID || body["session_id"] != req.SessionID {
		t.Errorf("approval correlation = %+v, want id %q session %q", body, req.ID, req.SessionID)
	}
	if body["expires_at"] != float64(req.ExpiresAt) {
		t.Errorf("expires_at = %v, want %d", body["expires_at"], req.ExpiresAt)
	}
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
	f, err := chk2.ReadFile(target, 0, 0, "text", "", "")
	if err != nil {
		t.Fatalf("the approved write did not happen: %v", err)
	}
	if f == nil {
		t.Fatal("the approved write produced no readable file")
	}
	// Data is a *string (nil means "no inline body"), so deref rather than
	// formatting the pointer: %v on a *string prints the address.
	if f.Data == nil {
		t.Fatal("the approved write produced a file with no inline body")
	}
	if got := *f.Data; !strings.Contains(got, "approved-content") {
		t.Errorf("file content = %q, want it to contain %q", got, "approved-content")
	}
	_ = chk2.RemoveFile(target)
}

// A rejection must leave the host untouched. A gate whose "no" still performed
// the operation would be worse than no gate at all.
func TestTERMOuterReviewPendingCarriesCorrelation(t *testing.T) {
	s := newTestServer(t)
	sessionID, _ := startGatedSession(t, s, 1, time.Minute)
	target := filepath.Join(t.TempDir(), "termcp-term-review-correlation.txt")
	text := termText(t, s, map[string]any{
		"cmd": "write " + sessionID + " file " + target + " --data hi",
	})
	if !strings.HasPrefix(text, "TERM/1 202 Accepted") {
		t.Fatalf("term review reply = %q", text)
	}
	approvalID, ok := headerValue(t, text, "Approval-ID")
	if !ok || approvalID == "" {
		t.Fatalf("Approval-ID missing from term reply:\n%s", text)
	}
	if session, ok := headerValue(t, text, "Session-ID"); !ok || session != sessionID {
		t.Fatalf("Session-ID = %q, want %q:\n%s", session, sessionID, text)
	}
	if _, ok := headerValue(t, text, "Expires-At"); !ok {
		t.Fatalf("Expires-At missing from term reply:\n%s", text)
	}
	if _, err := s.sessMgr.Get(sessionID).ApprovalQueue().Get(approvalID); err != nil {
		t.Fatalf("Approval-ID %q is not in the queue: %v", approvalID, err)
	}
}

func TestFileWriteIsNotExecutedOnRejection(t *testing.T) {
	s := newTestServer(t)
	sessionID, _ := startGatedSession(t, s, 1, time.Minute)

	target := filepath.Join(t.TempDir(), "termcp-approval-rejected.txt")
	cli, _ := s.sftpClient(sessionID)
	_ = cli.RemoveFile(target)
	_ = cli.Close()

	if _, err := s.call(context.Background(), "file_write", map[string]any{
		"session_id":  sessionID,
		"remote_path": target,
		"data":        "must-not-appear",
	}); err != nil {
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

// A session scoped to file operations must let a port forward run straight
// through (issue #75). This is the direction that matters and the one nobody
// writes a test for: a gate that checks "is review mode on" instead of "is this
// kind held" holds the tunnel anyway, which looks identical from the UI (the lock
// is on) and only shows up as an agent blocked from something it was allowed.
func TestScopedReviewLetsUnscopedOperationsThrough(t *testing.T) {
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
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	live := s.sessMgr.Get(sessionID)
	if live == nil {
		t.Fatal("session missing from the manager")
	}
	// Review the file operations only.
	if err := live.EnableApprovalScoped(1, 0, []approval.Kind{
		approval.KindFileWrite, approval.KindFileDelete,
	}); err != nil {
		t.Fatalf("EnableApprovalScoped: %v", err)
	}

	// A forward is not in the scope, so the gate must decline to hold it and the
	// handler must run. `forward(action=list)` needs no tunnel and returns OK, so
	// it is a safe probe for "the gate let this through".
	res, held := s.gateOperation(context.Background(), makeRequest(map[string]any{
		"action":     "list",
		"session_id": sessionID,
	}), "forward", sessionID)
	if held {
		t.Fatal("a session scoped to file operations must not hold a port forward")
	}
	if res != nil {
		t.Fatalf("the gate returned a result for an operation it did not hold: %v", res)
	}

	// A file write IS in the scope, so it must be held.
	if _, held := s.gateOperation(context.Background(), makeRequest(map[string]any{
		"session_id":  sessionID,
		"remote_path": "/tmp/gated",
		"data":        "x",
	}), "file_write", sessionID); !held {
		t.Fatal("a file write is in the scope and must be held")
	}

	// And with no scopes, the forward is held again — the default is unchanged.
	if err := live.EnableApproval(1, 0); err != nil {
		t.Fatal(err)
	}
	if _, held := s.gateOperation(context.Background(), makeRequest(map[string]any{
		"action":     "list",
		"session_id": sessionID,
	}), "forward", sessionID); !held {
		t.Fatal("with no scopes the forward must be held again")
	}
}

// Opening and closing a tunnel are separate decisions, so a session can hold one
// without the other. Checking only "forward" would hold both.
func TestForwardOpenAndCloseAreSeparatelyScopeable(t *testing.T) {
	s := newTestServer(t)
	startReq := makeRequest(map[string]any{
		"command": testShell(), "args": testInteractiveShellArgs(),
		"mode": "pty", "ssh_config": "internal",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := parseResult(t, startResult)["session_id"].(string)
	live := s.sessMgr.Get(sessionID)
	if err := live.EnableApprovalScoped(1, 0, []approval.Kind{approval.KindForwardClose}); err != nil {
		t.Fatal(err)
	}

	open := makeRequest(map[string]any{"action": "local", "session_id": sessionID, "remote_port": 80})
	if _, held := s.gateOperation(context.Background(), open, "forward", sessionID); held {
		t.Error("opening a tunnel is not in scope but was held")
	}
	closeReq := makeRequest(map[string]any{"action": "close", "session_id": sessionID, "forward_id": "x"})
	if _, held := s.gateOperation(context.Background(), closeReq, "forward", sessionID); !held {
		t.Error("closing a tunnel is in scope and must be held")
	}
}
