package notify_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/notify"
)

type mockSender struct {
	mu            sync.Mutex
	resourceCalls []string
	samplingCalls []string
}

func (m *mockSender) SendResourceNotification(ctx context.Context, shellID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resourceCalls = append(m.resourceCalls, shellID)
	return nil
}

func (m *mockSender) SendSamplingNotification(ctx context.Context, shellID string, target any, event notify.Event, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samplingCalls = append(m.samplingCalls, shellID+":"+string(event))
	return nil
}

func (m *mockSender) counts() (resCount, sampCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.resourceCalls), len(m.samplingCalls)
}

// waitFor polls until cond holds, or fails the test at the deadline. These tests
// assert on WHEN a timer fires, so they cannot sleep a fixed duration and then
// read a count: a loaded CI runner (or a coarse Windows timer tick) makes the
// event land late, and a fixed sleep turns that into a flake. Polling waits for
// the event and fails only if it never happens.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out after %v waiting for %s", timeout, what)
	}
}

func TestNotifyManager_RegisterAndList(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	rule1, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if rule1.ID == "" {
		t.Fatal("expected non-empty rule ID")
	}

	rule2, err := mgr.Register("sess-1", "shell-2", notify.ChannelSampling, notify.EventExit, 0, nil)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}

	all := mgr.List("")
	if len(all) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(all))
	}

	filtered := mgr.List("shell-1")
	if len(filtered) != 1 || filtered[0].ID != rule1.ID {
		t.Fatalf("expected 1 rule for shell-1, got %d", len(filtered))
	}

	if !mgr.Unregister(rule1.ID) {
		t.Fatalf("expected Unregister to return true")
	}
	if mgr.Unregister("non-existent") {
		t.Fatalf("expected false for unknown rule")
	}
	if len(mgr.List("")) != 1 {
		t.Fatalf("expected 1 rule left after unregister")
	}
	_ = rule2
}

func TestNotifyManager_ClearShellAndSession(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	mgr.Register("sess-1", "shell-2", notify.ChannelSampling, notify.EventExit, 0, nil)
	mgr.Register("sess-2", "shell-3", notify.ChannelResource, notify.EventSilence, 2, nil)

	mgr.ClearShell("shell-1")
	if len(mgr.List("shell-1")) != 0 {
		t.Fatal("expected shell-1 rules to be cleared")
	}
	if len(mgr.List("")) != 2 {
		t.Fatalf("expected 2 rules remaining, got %d", len(mgr.List("")))
	}

	mgr.ClearSession("sess-1")
	if len(mgr.List("")) != 1 {
		t.Fatalf("expected 1 rule remaining after ClearSession(sess-1), got %d", len(mgr.List("")))
	}
	if mgr.List("")[0].ShellID != "shell-3" {
		t.Fatalf("expected shell-3 rule to remain, got %s", mgr.List("")[0].ShellID)
	}
}

func TestNotifyManager_ExitOneShotAndAutoCleanup(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender, notify.WithCooldown(10*time.Millisecond))

	_, err := mgr.Register("sess-1", "shell-1", notify.ChannelSampling, notify.EventExit, 0, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	// Also register a silence rule on the same shell to test that OnExit clears ALL rules on that shell
	_, err = mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventSilence, 5, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	exitCode := 0
	mgr.OnExit("shell-1", &exitCode)

	// The exit dispatch runs on its own goroutine; wait for the send.
	waitFor(t, time.Second, "the exit notification", func() bool {
		_, samp := sender.counts()
		return samp == 1
	})

	// All rules for shell-1 must be auto-cleaned up
	if len(mgr.List("shell-1")) != 0 {
		t.Fatalf("expected all rules on shell-1 to be cleaned up, got %d", len(mgr.List("shell-1")))
	}
}

func TestNotifyManager_OutputDualEdgeAndCooldown(t *testing.T) {
	sender := &mockSender{}
	// Cooldown 40ms, trailing 60ms
	mgr := notify.NewManager(sender,
		notify.WithCooldown(40*time.Millisecond),
		notify.WithTrailingDelay(60*time.Millisecond),
	)

	_, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// 1st output: the immediate notification fires. The dispatch runs on its own
	// goroutine, so wait for the send rather than assuming it already ran.
	mgr.OnOutput("shell-1")
	waitFor(t, time.Second, "the immediate leading-edge notification", func() bool {
		res, _ := sender.counts()
		return res == 1
	})

	// 2nd output within cooldown: the immediate send must be dropped by the
	// cooldown gate, and the trailing timer reset to +60ms from now.
	//
	// This asserts an ABSENCE, so it has to let the cooldown genuinely elapse:
	// there is no event to poll for. 10ms + 20ms = 30ms against a 40ms cooldown
	// leaves a 10ms margin while staying inside the window.
	time.Sleep(10 * time.Millisecond)
	mgr.OnOutput("shell-1")
	time.Sleep(20 * time.Millisecond)
	if res, _ := sender.counts(); res != 1 {
		t.Fatalf("cooldown should have prevented a second immediate notification, got %d", res)
	}

	// The trailing edge fires once output stops (60ms after the 2nd output). Poll
	// for it: on a loaded runner the timer lands later than a fixed 80ms sleep,
	// which is exactly the flake this replaced.
	waitFor(t, 3*time.Second, "the trailing-edge notification", func() bool {
		res, _ := sender.counts()
		return res == 2
	})
}

func TestNotifyManager_SilenceOneShot(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender,
		notify.WithCooldown(10*time.Millisecond),
	)

	rule, err := mgr.Register("sess-1", "shell-1", notify.ChannelSampling, notify.EventSilence, 1, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	mgr.OnOutput("shell-1")

	// Within 500ms the silence must not have triggered. This asserts an ABSENCE,
	// so it does need to wait out the part of the window it is testing: 400ms of a
	// 1s timer leaves 600ms of margin, so a slow runner only makes the window
	// safer, never flakier.
	time.Sleep(400 * time.Millisecond)
	_, samp := sender.counts()
	if samp != 0 {
		t.Fatalf("silence should not have triggered yet, got %d", samp)
	}

	// The 1s silence timer then fires. Poll instead of sleeping a fixed 800ms, so a
	// late timer fails only if it never arrives.
	waitFor(t, 3*time.Second, "the silence notification", func() bool {
		_, samp := sender.counts()
		return samp == 1
	})

	// One-shot silence rule should be automatically unregistered. The unregister
	// is deferred in dispatchWithCooldown, so it lands just after the send.
	waitFor(t, time.Second, "the silence rule to auto-unregister", func() bool {
		return len(mgr.List("shell-1")) == 0
	})
	_ = rule
}

func TestNotifyManager_OutputBurstCollapsesToOneLeadingAndOneTrailing(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender,
		notify.WithCooldown(50*time.Millisecond),
		notify.WithTrailingDelay(80*time.Millisecond),
	)
	if _, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// A fast producer: 200 chunks in a tight loop. The leading edge must be
	// throttled at the source and the trailing timer must collapse to one.
	for i := 0; i < 200; i++ {
		mgr.OnOutput("shell-1")
	}

	// The leading edge is dispatched on its own goroutine; wait for it rather than
	// sleeping a fixed 30ms that a contended runner can overrun.
	waitFor(t, time.Second, "the single leading notification", func() bool {
		res, _ := sender.counts()
		return res >= 1
	})

	// Wait past the trailing delay; the trailing edge fires once.
	waitFor(t, 3*time.Second, "the trailing-edge notification", func() bool {
		res, _ := sender.counts()
		return res == 2
	})
	// A burst must collapse to exactly one trailing edge: a short settle afterwards
	// must not produce a third send.
	time.Sleep(150 * time.Millisecond)
	if res, _ := sender.counts(); res != 2 {
		t.Fatalf("burst should yield 1 leading + 1 trailing, got %d", res)
	}
}

func TestNotifyManager_SilenceSecondsOnlyForSilenceEvent(t *testing.T) {
	mgr := notify.NewManager(&mockSender{})
	if _, err := mgr.Register("s", "sh-out", notify.ChannelResource, notify.EventOutput, 9, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register("s", "sh-exit", notify.ChannelResource, notify.EventExit, 9, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register("s", "sh-sil", notify.ChannelResource, notify.EventSilence, 9, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range mgr.List("") {
		if r.Event == notify.EventSilence {
			if r.SilenceSec != 9 {
				t.Fatalf("silence rule should keep silence_seconds=9, got %d", r.SilenceSec)
			}
			continue
		}
		if r.SilenceSec != 0 {
			t.Fatalf("%s rule should not expose silence_seconds, got %d", r.Event, r.SilenceSec)
		}
	}
}
