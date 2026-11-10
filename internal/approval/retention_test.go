package approval

import (
	"testing"
	"time"
)

// A SNAPSHOT'S COST IS WHAT IT CONTAINS, so decided history is bounded. Without
// the bound, one transition at a 10 000-request backlog copied the whole map:
// 2.9 ms and 3.7 MB, per transition, growing with every request the session ever
// decided.
func TestDecidedHistoryIsBounded(t *testing.T) {
	q := NewQueue("sess-bound", Options{Timeout: time.Hour})
	for i := 0; i < 3*maxRetainedTerminal; i++ {
		id, err := q.Submit(Submission{ShellID: "sh", Text: "ls", Summary: "run ls"})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if _, err := q.Approve(id); err != nil {
			t.Fatalf("approve %d: %v", i, err)
		}
	}
	if got := len(q.List()); got > maxRetainedTerminal {
		t.Fatalf("List returned %d decided requests, want at most %d", got, maxRetainedTerminal)
	}
	// The NEWEST survive: a reviewer reads what just happened, not the first entry
	// of the session.
	id, err := q.Submit(Submission{ShellID: "sh", Text: "pwd", Summary: "run pwd"})
	if err != nil {
		t.Fatalf("submit newest: %v", err)
	}
	if _, err := q.Get(id); err != nil {
		t.Fatalf("the newest request is not readable: %v", err)
	}
}

// PENDING REQUESTS ARE THE WORK LIST and are never evicted, however much decided
// history piles up behind them: a dropped pending request is a command nobody can
// approve and a waiter nobody can wake.
func TestEvictionNeverDropsAPendingRequest(t *testing.T) {
	q := NewQueue("sess-pending", Options{Timeout: time.Hour})
	waiting, err := q.Submit(Submission{ShellID: "sh", Text: "sleep 1", Summary: "run sleep"})
	if err != nil {
		t.Fatalf("submit pending: %v", err)
	}
	for i := 0; i < 2*maxRetainedTerminal; i++ {
		id, err := q.Submit(Submission{ShellID: "sh", Text: "ls", Summary: "run ls"})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if _, err := q.Approve(id); err != nil {
			t.Fatalf("approve %d: %v", i, err)
		}
	}
	got, err := q.Get(waiting)
	if err != nil {
		t.Fatalf("the pending request was evicted: %v", err)
	}
	if got.State != Pending {
		t.Fatalf("pending request is %s", got.State)
	}
	// And it is still decidable: the shell index must still list it.
	found := false
	for _, r := range q.ListForShell("sh") {
		if r.ID == waiting {
			found = true
		}
	}
	if !found {
		t.Fatal("the pending request fell out of the shell index")
	}
}

// An evicted id must not linger in the shell index: a caller iterating that index
// would then chase a request that no longer exists.
func TestEvictedRequestsLeaveTheShellIndex(t *testing.T) {
	q := NewQueue("sess-index", Options{Timeout: time.Hour})
	for i := 0; i < 2*maxRetainedTerminal; i++ {
		id, err := q.Submit(Submission{ShellID: "sh", Text: "ls", Summary: "run ls"})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if _, err := q.Approve(id); err != nil {
			t.Fatalf("approve %d: %v", i, err)
		}
	}
	list := q.ListForShell("sh")
	if len(list) > maxRetainedTerminal {
		t.Fatalf("the shell index kept %d requests, want at most %d", len(list), maxRetainedTerminal)
	}
}
