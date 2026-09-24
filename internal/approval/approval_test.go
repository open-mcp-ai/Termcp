package approval_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
)

func newQ(t *testing.T, need int, timeout time.Duration) *approval.Queue {
	t.Helper()
	return approval.NewQueue("sess-1", approval.Options{Need: need, Timeout: timeout})
}

// A fresh request starts pending and undecided.
func TestSubmitStartsPending(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id, err := q.SubmitShellInput("shell-1", "mcp", "ls -la", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	got, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != approval.Pending {
		t.Errorf("state = %q, want pending", got.State)
	}
	if got.Need != 1 || got.NeedsMore() != 1 {
		t.Errorf("need=%d needsMore=%d, want 1/1", got.Need, got.NeedsMore())
	}
	if got.Text != "ls -la" || got.ShellID != "shell-1" || got.Source != "mcp" {
		t.Errorf("payload not preserved: %+v", got)
	}
}

// One reviewer is enough, and the decision is recorded as the local reviewer
// rather than as a name someone typed: under a single deployment token the
// server cannot verify who clicked, so a self-declared name would look like
// attribution without being any.
func TestOneApprovalIsEnough(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "rm -rf /tmp/x", nil)

	got, err := q.Approve(id)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.State != approval.Approved {
		t.Fatalf("state = %q, want approved", got.State)
	}
	if len(got.Approvals) != 1 || got.Approvals[0] != approval.LocalReviewer {
		t.Errorf("approvals = %v, want [%s]", got.Approvals, approval.LocalReviewer)
	}
}

// A decision on an already-decided request is refused rather than silently
// re-applied.
func TestSecondDecisionIsRefused(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "x", nil)
	if _, err := q.Approve(id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(id); !errors.Is(err, approval.ErrNotPending) {
		t.Errorf("second approve err = %v, want ErrNotPending", err)
	}
	if _, err := q.Reject(id, "no"); !errors.Is(err, approval.ErrNotPending) {
		t.Errorf("reject after approve err = %v, want ErrNotPending", err)
	}
}

// One rejection is final, even with the threshold nearly met.
func TestRejectIsFinalAndBeatsPendingApprovals(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "shutdown -h now", nil)

	got, err := q.Reject(id, "not during business hours")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if got.State != approval.Rejected {
		t.Errorf("state = %q, want rejected", got.State)
	}
	if got.Reason != "not during business hours" {
		t.Errorf("reason not recorded: %q", got.Reason)
	}

	// A late approval must not be able to resurrect it.
	if _, err := q.Approve(id); !errors.Is(err, approval.ErrNotPending) {
		t.Errorf("approve after reject: err = %v, want ErrNotPending", err)
	}
	if got, _ := q.Get(id); got.State != approval.Rejected {
		t.Errorf("state = %q after late approve, want rejected", got.State)
	}
}

// Expiry is fail-closed: the request ends Expired, never Approved.
func TestExpiryIsFailClosed(t *testing.T) {
	q := newQ(t, 1, 30*time.Millisecond)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "echo hi", nil)

	got, err := q.Wait(id)
	if !errors.Is(err, approval.ErrTimeout) {
		t.Errorf("Wait err = %v, want ErrTimeout", err)
	}
	if got.State != approval.Expired {
		t.Errorf("state = %q, want expired", got.State)
	}
	if len(got.Approvals) != 0 {
		t.Errorf("expired request must carry no approvals, got %v", got.Approvals)
	}
}

// Wait must observe an approval promptly, not after the timeout.
func TestWaitUnblocksOnApproval(t *testing.T) {
	q := newQ(t, 1, 10*time.Second)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "echo hi", nil)

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = q.Approve(id)
	}()

	start := time.Now()
	got, err := q.Wait(id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.State != approval.Approved {
		t.Errorf("state = %q, want approved", got.State)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Wait took %v; it should wake on the decision, not the deadline", elapsed)
	}
}

// Wait on an already-decided request returns immediately with the decision.
func TestWaitReturnsImmediatelyWhenAlreadyDecided(t *testing.T) {
	q := newQ(t, 1, 10*time.Second)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "echo hi", nil)
	if _, err := q.Approve(id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := q.Wait(id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.State != approval.Approved {
		t.Errorf("state = %q, want approved", got.State)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Wait on a decided request took %v; it must not block", elapsed)
	}
}

// Cancelling is fail-closed and idempotent.
func TestCancelAndCancelAllAreFailClosed(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id1, _ := q.SubmitShellInput("shell-1", "mcp", "one", nil)
	id2, _ := q.SubmitShellInput("shell-2", "rest", "two", nil)

	q.Cancel(id1, "approval mode disabled")
	if got, _ := q.Get(id1); got.State != approval.Cancelled {
		t.Errorf("state = %q, want cancelled", got.State)
	}
	// A cancelled request can no longer be approved.
	if _, err := q.Approve(id1); !errors.Is(err, approval.ErrNotPending) {
		t.Errorf("approve after cancel: err = %v, want ErrNotPending", err)
	}

	q.CancelAll("session ended")
	if got, _ := q.Get(id2); got.State != approval.Cancelled {
		t.Errorf("state = %q, want cancelled", got.State)
	}
	// Cancel is a no-op on an already-terminal request.
	q.Cancel(id1, "again")
	if got, _ := q.Get(id1); got.State != approval.Cancelled {
		t.Errorf("state = %q after second cancel, want cancelled", got.State)
	}
}

// A closed queue refuses new work and drops what is pending.
func TestCloseRefusesNewSubmissionsAndCancelsPending(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "one", nil)

	q.Close("session released")
	if got, _ := q.Get(id); got.State != approval.Cancelled {
		t.Errorf("pending request state = %q, want cancelled", got.State)
	}
	if _, err := q.SubmitShellInput("shell-1", "mcp", "two", nil); err == nil {
		t.Error("Submit on a closed queue must fail")
	}
}

// An empty submission is refused: it would spend an approver's attention on
// nothing.
func TestEmptySubmissionIsRefused(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	if _, err := q.SubmitShellInput("shell-1", "mcp", "", nil); err == nil {
		t.Error("empty text with no keys must be refused")
	}
	if _, err := q.SubmitShellInput("shell-1", "mcp", "", []string{}); err == nil {
		t.Error("empty text with an empty key list must be refused")
	}
	// Keys alone are a valid submission: ctrl+c has no text.
	if _, err := q.SubmitShellInput("shell-1", "mcp", "", []string{"ctrl+c"}); err != nil {
		t.Errorf("a key-only submission is legitimate: %v", err)
	}
}

// Need below 1 is clamped up: a queue that approves nothing is a broken gate.
func TestNeedIsClampedToOne(t *testing.T) {
	q := approval.NewQueue("s", approval.Options{Need: 0})
	if q.Need() != 1 {
		t.Errorf("Need = %d, want 1", q.Need())
	}
	q2 := approval.NewQueue("s", approval.Options{Need: -5})
	if q2.Need() != 1 {
		t.Errorf("Need = %d, want 1", q2.Need())
	}
}

// A request that already meets its threshold at submit time would deadlock Wait
// if it stayed pending. The invariant is "Pending implies NeedsMore() > 0".
func TestPendingAlwaysNeedsMore(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "x", nil)
	got, _ := q.Get(id)
	if got.State == approval.Pending && got.NeedsMore() == 0 {
		t.Error("a pending request with NeedsMore()==0 can never be resolved; Wait would block forever")
	}
}

// Snapshots must not alias the queue's internal slices: a caller appending to
// Approvals must not corrupt the request.
func TestSnapshotsDoNotAliasInternalState(t *testing.T) {
	q := newQ(t, 3, time.Minute)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "x", []string{"enter"})
	if _, err := q.Approve(id); err != nil {
		t.Fatal(err)
	}

	snap, _ := q.Get(id)
	snap.Approvals = append(snap.Approvals, "mallory")
	snap.Keys = append(snap.Keys, "ctrl+c")

	fresh, _ := q.Get(id)
	if len(fresh.Approvals) != 1 {
		t.Errorf("approvals = %v; a caller's append leaked into the queue", fresh.Approvals)
	}
	if len(fresh.Keys) != 1 {
		t.Errorf("keys = %v; a caller's append leaked into the queue", fresh.Keys)
	}
}

// The change hook fires for submission and every transition, and runs without
// the queue lock so a hook may call back in.
func TestOnChangeFiresAndCanCallBack(t *testing.T) {
	var mu sync.Mutex
	var states []approval.State
	var q *approval.Queue
	q = approval.NewQueue("sess-1", approval.Options{
		Need:    1,
		Timeout: time.Minute,
		OnChange: func(r approval.Request) {
			mu.Lock()
			states = append(states, r.State)
			mu.Unlock()
			// Calling back in would deadlock if the hook ran under the lock.
			_ = q.List()
		},
	})

	id, err := q.SubmitShellInput("shell-1", "mcp", "x", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := q.Approve(id); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []approval.State{approval.Pending, approval.Approved}
	if len(states) != len(want) {
		t.Fatalf("hook states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Errorf("hook state[%d] = %q, want %q", i, states[i], want[i])
		}
	}
}

// List puts actionable entries first so the API worklist leads with them.
func TestListOrdersPendingFirst(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	done, _ := q.SubmitShellInput("shell-1", "mcp", "done", nil)
	if _, err := q.Approve(done); err != nil {
		t.Fatal(err)
	}
	pending, _ := q.SubmitShellInput("shell-1", "mcp", "pending", nil)

	list := q.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	if list[0].ID != pending || list[0].State != approval.Pending {
		t.Errorf("List[0] = %s/%s, want the pending request first", list[0].ID, list[0].State)
	}
}

// Requests are addressable per shell, oldest first.
func TestListForShell(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	a, _ := q.SubmitShellInput("shell-1", "mcp", "a", nil)
	b, _ := q.SubmitShellInput("shell-1", "mcp", "b", nil)
	c, _ := q.SubmitShellInput("shell-2", "mcp", "c", nil)

	got := q.ListForShell("shell-1")
	if len(got) != 2 || got[0].ID != a || got[1].ID != b {
		t.Errorf("ListForShell(shell-1) = %v, want [a b]", ids(got))
	}
	if got := q.ListForShell("shell-2"); len(got) != 1 || got[0].ID != c {
		t.Errorf("ListForShell(shell-2) = %v, want [c]", ids(got))
	}
	if got := q.ListForShell("nope"); len(got) != 0 {
		t.Errorf("ListForShell(unknown) = %v, want empty", ids(got))
	}
}

// Concurrent decisions must resolve the request exactly once, with no lost
// update: several reviewers can click at the same moment.
func TestConcurrentApprovalsResolveOnce(t *testing.T) {
	q := newQ(t, 1, 10*time.Second)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "x", nil)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var approved int
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.Approve(id); err == nil {
				mu.Lock()
				approved++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if approved != 1 {
		t.Errorf("exactly one concurrent approve may succeed, got %d", approved)
	}
	got, _ := q.Get(id)
	if got.State != approval.Approved {
		t.Fatalf("state = %q, want approved", got.State)
	}
	if len(got.Approvals) != 1 {
		t.Errorf("approvals = %v, want exactly one entry", got.Approvals)
	}
}

// Every error path must leave the request un-approved: this is the single
// property the feature exists for.
func TestNoErrorPathEverApproves(t *testing.T) {
	q := newQ(t, 1, 20*time.Millisecond)
	id, _ := q.SubmitShellInput("shell-1", "mcp", "x", nil)

	// Only the first decision lands; the rest are refused and must not turn the
	// request into a second approval.
	if _, err := q.Approve(id); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if _, err := q.Approve(id); !errors.Is(err, approval.ErrNotPending) {
		t.Errorf("repeat approve err = %v, want ErrNotPending", err)
	}
	if got, _ := q.Get(id); got.State != approval.Approved {
		t.Errorf("state = %q, want the single approval to stand", got.State)
	}
}

func TestUnknownIDIsReported(t *testing.T) {
	q := newQ(t, 1, time.Minute)
	if _, err := q.Get("nope"); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := q.Approve("nope"); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("Approve err = %v, want ErrNotFound", err)
	}
	if _, err := q.Reject("nope", ""); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("Reject err = %v, want ErrNotFound", err)
	}
	if _, err := q.Wait("nope"); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("Wait err = %v, want ErrNotFound", err)
	}
}

func ids(rs []approval.Request) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
