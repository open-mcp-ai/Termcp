package notify_test

import (
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/notify"
)

// The approval wake-up reuses the output-event dispatch path, so an agent
// registered with shell_notify learns that a decision landed without polling.
// These tests pin that wiring: it is the only reason an agent can wait instead
// of spinning on approval(action=get).
func TestOnApprovalChangeWakesOutputRules(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	if _, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil); err != nil {
		t.Fatal(err)
	}

	mgr.OnApprovalChange("shell-1", "approval approved")

	// Dispatch runs on its own goroutine, so poll rather than sleep a fixed time.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, _ := sender.counts(); res > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("OnApprovalChange did not dispatch to a registered output rule")
}

// A shell with no rules must be a no-op, not a panic: a decision can land after
// the agent has gone away.
func TestOnApprovalChangeWithoutRulesIsSafe(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	mgr.OnApprovalChange("shell-with-no-rules", "approval pending")
	mgr.OnApprovalChange("", "approval pending") // an already-detached shell

	if res, samp := sender.counts(); res != 0 || samp != 0 {
		t.Errorf("expected no dispatch, got resource=%d sampling=%d", res, samp)
	}
}

// exit and silence rules describe the process lifecycle, which an approval
// transition does not touch. Waking them would fire an "exited" notification
// for a command that was merely approved.
func TestOnApprovalChangeDoesNotWakeExitOrSilenceRules(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	if _, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventExit, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventSilence, 1, nil); err != nil {
		t.Fatal(err)
	}

	mgr.OnApprovalChange("shell-1", "approval approved")
	time.Sleep(400 * time.Millisecond)

	if res, samp := sender.counts(); res != 0 || samp != 0 {
		t.Errorf("an approval must not fire exit/silence rules; got resource=%d sampling=%d", res, samp)
	}
}

// An unregistered shell stops receiving approval wake-ups, so a closed agent
// does not accumulate dispatches.
func TestOnApprovalChangeStopsAfterUnregister(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	rule, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr.OnApprovalChange("shell-1", "approval pending")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, _ := sender.counts(); res > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mgr.Unregister(rule.ID)
	before, _ := sender.counts()

	// The cooldown gate is 1s, so wait past it before checking that nothing new
	// arrives: otherwise a suppressed dispatch would look like a correct one.
	time.Sleep(1200 * time.Millisecond)
	mgr.OnApprovalChange("shell-1", "approval approved")
	time.Sleep(400 * time.Millisecond)

	after, _ := sender.counts()
	if after > before {
		t.Errorf("dispatches grew from %d to %d after unregister", before, after)
	}
}

// Concurrent approval transitions must not race the rule bookkeeping.
func TestOnApprovalChangeConcurrent(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)
	if _, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.OnApprovalChange("shell-1", "approval pending")
		}()
	}
	wg.Wait()
	// Nothing to assert beyond surviving the race: the cooldown gate intentionally
	// collapses bursts, so the count is not deterministic.
}
