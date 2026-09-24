package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// startInteractiveSession opens a session suitable for driving input, skipping
// on platforms where the test shell cannot be scripted reliably.
func startInteractiveSession(t *testing.T) *Session {
	t.Helper()
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Terminate(true, 0) })
	return s
}

// Approval mode is off by default: an existing deployment must be unaffected.
func TestApprovalIsOffByDefault(t *testing.T) {
	s := startInteractiveSession(t)
	if s.ApprovalEnabled() {
		t.Error("a new session must not gate input")
	}
	if s.ApprovalQueue() != nil {
		t.Error("no queue should exist until approval mode is enabled")
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("primary shell missing")
	}
	if cs.RequiresApproval() {
		t.Error("a shell in an ungated session must not require approval")
	}
}

// Enabling covers every shell in the session, which is the scope the switch has.
func TestEnableApprovalCoversTheSession(t *testing.T) {
	s := startInteractiveSession(t)
	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatalf("EnableApproval: %v", err)
	}
	if !s.ApprovalEnabled() {
		t.Fatal("session should gate input after enabling")
	}
	cs := s.PrimaryShell()
	if !cs.RequiresApproval() {
		t.Error("every shell in a gated session must require approval")
	}
	if q := cs.ApprovalQueue(); q == nil {
		t.Error("the shell must expose the session queue")
	}
}

// A threshold below 1 is refused rather than silently clamped: the caller asked
// for a gate that approves nothing, which is a configuration error, not a policy.
func TestEnableApprovalRejectsBadThreshold(t *testing.T) {
	s := startInteractiveSession(t)
	if err := s.EnableApproval(0, time.Minute); err == nil {
		t.Error("need=0 must be refused")
	}
	if s.ApprovalEnabled() {
		t.Error("a refused EnableApproval must not turn the gate on")
	}
	if err := s.EnableApproval(1, -time.Second); err == nil {
		t.Error("a negative timeout must be refused")
	}
}

// Disabling cancels what is pending. A request submitted under the gate must not
// become executable because the gate was switched off.
func TestDisableApprovalCancelsPending(t *testing.T) {
	s := startInteractiveSession(t)
	if err := s.EnableApproval(2, time.Minute); err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	q := s.ApprovalQueue()

	id, err := cs.SubmitForApproval("mcp", "rm -rf /tmp/x", nil)
	if err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	if got, _ := q.Get(id); got.State != approval.Pending {
		t.Fatalf("state = %q, want pending", got.State)
	}

	s.DisableApproval()

	if s.ApprovalEnabled() {
		t.Error("session should not gate input after disabling")
	}
	if got, _ := q.Get(id); got.State != approval.Cancelled {
		t.Errorf("state = %q after disable, want cancelled", got.State)
	}
	// And a shell can no longer submit.
	if _, err := cs.SubmitForApproval("mcp", "anything", nil); err == nil {
		t.Error("submitting while approval mode is off must fail")
	}
}

// Re-enabling replaces the queue and cancels the old pending set: a request
// approved under one threshold must not be replayed under another.
func TestReEnableApprovalCancelsPreviousQueue(t *testing.T) {
	s := startInteractiveSession(t)
	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	oldQueue := s.ApprovalQueue()
	id, err := cs.SubmitForApproval("mcp", "old", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if got, _ := oldQueue.Get(id); got.State != approval.Cancelled {
		t.Errorf("state = %q on the replaced queue, want cancelled", got.State)
	}
	if q := s.ApprovalQueue(); q == nil {
		t.Error("re-enabling must install a fresh queue")
	}
}

// The change sink fires for submissions and decisions, so the UI and waiting
// agents see the whole lifecycle.
func TestApprovalChangeSinkObservesLifecycle(t *testing.T) {
	s := startInteractiveSession(t)

	type event struct {
		sessionID string
		state     approval.State
	}
	var got []event
	s.SetApprovalChangeHandler(func(sessionID string, req approval.Request) {
		got = append(got, event{sessionID, req.State})
	})

	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	id, err := cs.SubmitForApproval("mcp", "echo hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovalQueue().Approve(id); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("events = %v, want pending then approved", got)
	}
	if got[0].state != approval.Pending || got[1].state != approval.Approved {
		t.Errorf("states = %v/%v, want pending/approved", got[0].state, got[1].state)
	}
	for _, e := range got {
		if e.sessionID != s.ID {
			t.Errorf("event session = %q, want %q", e.sessionID, s.ID)
		}
	}
}

// A handler installed before approval mode is enabled must still be used: main
// installs the sink at session creation, long before a gate exists.
func TestChangeSinkInstalledBeforeEnablingIsUsed(t *testing.T) {
	s := startInteractiveSession(t)
	fired := 0
	s.SetApprovalChangeHandler(func(string, approval.Request) { fired++ })

	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrimaryShell().SubmitForApproval("mcp", "x", nil); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("sink fired %d times, want 1 (it must survive the enable)", fired)
	}
}

// Cancelling a session's approval mode releases a waiter instead of leaving it
// blocked until the timeout.
func TestDisableApprovalReleasesWaiter(t *testing.T) {
	s := startInteractiveSession(t)
	if err := s.EnableApproval(1, time.Hour); err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	q := s.ApprovalQueue()
	id, err := cs.SubmitForApproval("mcp", "long wait", nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan approval.Request, 1)
	go func() {
		req, _ := q.Wait(id)
		done <- req
	}()

	time.Sleep(50 * time.Millisecond)
	s.DisableApproval()

	select {
	case req := <-done:
		if req.State != approval.Cancelled {
			t.Errorf("state = %q, want cancelled", req.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after approval mode was disabled")
	}
}

// Approved input must actually reach the shell, and only after approval.
func TestApprovedInputReachesTheShell(t *testing.T) {
	s := startInteractiveSession(t)
	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	q := s.ApprovalQueue()

	marker := "approval_exec_marker"
	id, err := cs.SubmitForApproval("mcp", marker+"\n", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Before approval, the marker must not appear: the bytes were never written.
	time.Sleep(300 * time.Millisecond)
	if out := drainOutput(s); strings.Contains(out, marker) {
		t.Fatalf("input reached the shell before approval:\n%s", out)
	}

	if _, err := q.Approve(id); err != nil {
		t.Fatal(err)
	}
	if err := cs.ExecuteApprovedInput(marker+"\n", false, InputFromAI); err != nil {
		t.Fatalf("ExecuteApprovedInput: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out += drainOutput(s)
		if strings.Contains(out, marker) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("approved input never reached the shell; output was:\n%s", out)
}

// drainOutput reads whatever the session has buffered without blocking long.
func drainOutput(s *Session) string {
	out := ""
	for {
		chunk, err := s.ReadOutput(context.Background(), 200*time.Millisecond, true, 0, 0)
		out += chunk
		if err != nil || chunk == "" {
			return out
		}
	}
}

// Approval transitions must land in the shell's byte log, so a replay can tell
// reviewed input from input that never passed a gate.
func TestApprovalTransitionsAreRecordedInTheLog(t *testing.T) {
	// The audit marks are written to log.jsonl, which only exists when a store is
	// attached, so this test goes through the Manager rather than New: Delete is
	// what finalizes the session and closes the log file. Going through New
	// directly leaves the handle open and t.TempDir's RemoveAll then fails on
	// Windows with "used by another process" — only once the suite runs in
	// parallel with other tests, which is why it passed in isolation.
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		// Delete finalizes the session and closes its log file, so t.TempDir's
		// RemoveAll afterwards cannot race an open handle.
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	q := s.ApprovalQueue()

	// A request that is only submitted leaves the "requested" mark.
	submitted, err := cs.SubmitForApproval("mcp", "echo audited", nil)
	if err != nil {
		t.Fatal(err)
	}
	marks := readMarks(t, s, cs.ID)
	if !hasStatus(marks, api.LogApprovalRequest) {
		t.Errorf("no %q mark after submit; marks = %v", api.LogApprovalRequest, marks)
	}
	if hasStatus(marks, api.LogApprovalGranted) {
		t.Error("a pending request must not record a granted mark")
	}

	// Approving records the granted mark, which is what marks bytes as reviewed.
	if _, err := q.Approve(submitted); err != nil {
		t.Fatal(err)
	}
	marks = readMarks(t, s, cs.ID)
	if !hasStatus(marks, api.LogApprovalGranted) {
		t.Errorf("no %q mark after approval; marks = %v", api.LogApprovalGranted, marks)
	}

	// A rejection must not add a granted mark: no bytes follow it.
	before := countStatus(marks, api.LogApprovalGranted)
	rejected, err := cs.SubmitForApproval("mcp", "echo rejected", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Reject(rejected, "no"); err != nil {
		t.Fatal(err)
	}
	marks = readMarks(t, s, cs.ID)
	if got := countStatus(marks, api.LogApprovalGranted); got != before {
		t.Errorf("a rejection added a granted mark (%d -> %d)", before, got)
	}
}

func hasStatus(marks []api.LogMark, want api.LogStatus) bool {
	return countStatus(marks, want) > 0
}

func countStatus(marks []api.LogMark, want api.LogStatus) int {
	n := 0
	for _, m := range marks {
		if m.Status == want {
			n++
		}
	}
	return n
}

// readMarks reads the shell's marks, failing the test on a store error.
func readMarks(t *testing.T, s *Session, shellID string) []api.LogMark {
	t.Helper()
	marks, err := s.msgMgr.Marks(s.ID, shellID)
	if err != nil {
		t.Fatalf("Marks: %v", err)
	}
	return marks
}
