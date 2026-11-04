package extensions

import (
	"strings"
	"testing"
	"time"
)

// The on-demand half of the extension layer (issue #183). The contract these
// tests pin:
//
//   - nothing runs until something asks, and asking is `open ext <name>`
//   - every ask gets its OWN channel: no caller is ever handed a channel somebody
//     else may still be driving, and no two callers share a cursor
//   - the CONNECTION belongs to the install, not to the caller: it outlives the
//     channels, so finishing does not cost the next caller a dial
//   - a caller that has finished can hand its channel back at once
//   - what nobody takes is closed at the window, and the handle that named it says
//     so instead of reading as a typo

// lazyExt is an on-demand extension with the given idle window. It declares no
// forward, which is what makes it on demand.
func lazyExt(t *testing.T, r *Reconciler, name, idle string) *Extension {
	t.Helper()
	ext := Extension{Mode: "pipe"}
	ext.Name = name
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Args = nil
	ext.IdleTimeout = idle
	mustSave(t, r.store, &ext)
	return &ext
}

// TestOnDemandExtensionStaysDownUntilAsked is the whole point of the change: a
// template that declares no forward costs nothing — no dial, no channel, no
// connection — until a caller asks for it.
func TestOnDemandExtensionStaysDownUntilAsked(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "quiet", "10m")

	// Several walks, which is what used to be enough to dial it.
	for i := 0; i < 3; i++ {
		r.Sync(t.Context(), "")
		time.Sleep(120 * time.Millisecond)
	}
	st, ok := r.Status("quiet")
	if !ok {
		t.Fatal("the extension should be listed")
	}
	if st.Status != StatusIdle {
		t.Errorf("status = %q, want %q (nothing has asked for it)", st.Status, StatusIdle)
	}
	if st.Instances != 0 {
		t.Errorf("an extension nobody asked for should hold no instances, got %d", st.Instances)
	}
	if n := len(r.sessMgr.ListAll()); n != 0 {
		t.Errorf("an extension nobody asked for should hold no connection, got %d sessions", n)
	}
	if !st.Lazy {
		t.Error("a template that declares no forward should report itself as on demand")
	}
}

// TestOnDemandExtensionStartsOnAsk pins the ask itself: the handles come back from
// the call that asked, without a second round trip.
func TestOnDemandExtensionStartsOnAsk(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "asked", "10m")

	st, err := r.Acquire("asked", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusAvailable {
		t.Fatalf("an answered ask should be available, got %+v", st)
	}
	if st.SessionID == "" || st.ShellID == "" {
		t.Fatalf("an answered ask should carry both handles, got %+v", st)
	}
	if st.Instances != 1 {
		t.Errorf("one ask should hold one instance, got %d", st.Instances)
	}
}

// TestASilentChannelIsNeverHandedOver pins what replaced reuse: a channel that has
// gone silent is CLOSED at the window, not parked for the next caller. Handing it
// over would mean trusting that its process is still in a state worth driving, and
// the fork it saves is the cheap half of the bargain.
func TestASilentChannelIsNeverHandedOver(t *testing.T) {
	r := startReconciler(t)
	// A short window so the test does not wait ten minutes for silence.
	lazyExt(t, r, "silent", "50ms")

	first, err := r.Acquire("silent", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Let the window pass without touching it, and give the walk a turn to reap.
	time.Sleep(120 * time.Millisecond)
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.Sync(t.Context(), "")
		if cur, _ := r.Status("silent"); cur.Instances == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the silent channel was never closed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	second, err := r.Acquire("silent", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.ShellID == first.ShellID {
		t.Errorf("a silent channel must not be handed to the next caller (both %q)", second.ShellID)
	}
	if second.Instances != 1 {
		t.Errorf("the closed one should not still be counted, got %d", second.Instances)
	}
	// The connection is the install's, so the second ask forks again instead of
	// dialling again.
	if second.SessionID != first.SessionID {
		t.Errorf("the second ask should ride the same connection: %q vs %q", first.SessionID, second.SessionID)
	}
}

// TestBusyInstanceIsNotHandedToTheNextCaller is the rule that keeps two agents
// from reading each other's output: a channel in use belongs to whoever is driving
// it, so a second caller gets its own process on the same connection.
func TestBusyInstanceIsNotHandedToTheNextCaller(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "busy", "10m")

	first, err := r.Acquire("busy", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Acquire("busy", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.ShellID == first.ShellID {
		t.Fatalf("a used instance must not be handed to a second caller (both got %q)", second.ShellID)
	}
	if second.Instances != 2 {
		t.Errorf("two callers should hold two instances, got %d", second.Instances)
	}
	// One connection, two processes: a second instance costs a channel, not a dial.
	if n := len(r.sessMgr.ListAll()); n != 1 {
		t.Errorf("two instances of one extension should share the connection, got %d sessions", n)
	}
	if second.SessionID != first.SessionID {
		t.Errorf("both instances should be on one connection: %q vs %q", first.SessionID, second.SessionID)
	}
}

// TestReleaseHandsTheInstanceBack pins the caller's own half of the idle window:
// finishing is something the caller knows, and saying so stops the process now
// instead of at the end of the window.
func TestReleaseHandsTheInstanceBack(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "done", "10m")

	st, err := r.Acquire("done", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Release("done", st.ShellID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("releasing the handle should release one instance, got %d", n)
	}
	after, _ := r.Status("done")
	if after.Instances != 0 {
		t.Errorf("a released instance should be gone, got %d", after.Instances)
	}
	if after.Status != StatusIdle {
		t.Errorf("an extension with nothing left should read as idle, got %q", after.Status)
	}
	// Releasing it twice is an error rather than a quiet success: a caller that
	// believes it gave something back and did not will not look again.
	if _, err := r.Release("done", st.ShellID); err == nil {
		t.Error("releasing an instance that is not running should be refused")
	}
}

// TestConnectionOutlivesItsChannels pins the half of the change that makes the
// cheap thing cheap: finishing with an extension closes a channel, not the
// connection, so the next caller forks instead of dialling. A connection is seconds
// to build; a channel is a fork.
func TestConnectionOutlivesItsChannels(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "kept", "10m")

	first, err := r.Acquire("kept", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Release("kept", first.ShellID); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Status("kept")
	if after.Instances != 0 {
		t.Fatalf("the released channel should be gone, got %d", after.Instances)
	}
	// The connection is still there, and still named: nothing was using it for a
	// moment, and that is not a reason to redial.
	if after.SessionID == "" {
		t.Error("releasing a channel should leave the connection named")
	}
	if n := len(r.sessMgr.ListAll()); n != 1 {
		t.Errorf("the connection should outlive its channels, got %d sessions", n)
	}

	second, err := r.Acquire("kept", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Errorf("the next ask should reuse the connection: %q vs %q", second.SessionID, first.SessionID)
	}
	if second.ShellID == first.ShellID {
		t.Error("the next ask should still get its own channel")
	}
}

// TestTheWindowIsATimerNotAWalkTick pins the promise the option makes. A caller
// that asks for a window gets that window: the channel is closed when its silence
// ends, not whenever the next walk happens to look. The reconciler's loop is
// loopInterval (five seconds) wide, so a channel closing well inside that could
// only have been closed by its own deadline.
func TestTheWindowIsATimerNotAWalkTick(t *testing.T) {
	r := startReconciler(t)
	const window = 150 * time.Millisecond
	lazyExt(t, r, "timed", "150ms")

	st, err := r.Acquire("timed", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for {
		// NOTHING HERE TRIGGERS THE WALK. If the deadline is only honoured by the
		// sweep, this test cannot pass: the first walk is seconds away.
		cur, _ := r.Status("timed")
		if cur.Instances == 0 {
			elapsed := time.Since(start)
			if elapsed < window {
				t.Errorf("the channel was closed after %s, before its %s window ended", elapsed, window)
			}
			if elapsed > window+2*time.Second {
				t.Errorf("the channel outlived its window by %s", elapsed-window)
			}
			if cur.ClosedShellID != st.ShellID {
				t.Errorf("the closed channel should be named: got %q want %q", cur.ClosedShellID, st.ShellID)
			}
			if cur.Status != StatusIdle {
				t.Errorf("a window that ran out should leave the extension idle, got %q", cur.Status)
			}
			return
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("the window never ran out: %+v", cur)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestIdleInstanceIsClosedAtTheWindow pins the backstop for the caller that forgot:
// the window runs out, the process goes, and the handle that named it answers with
// what happened rather than reading as a typo.
func TestIdleInstanceIsClosedAtTheWindow(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "forgotten", "80ms")

	st, err := r.Acquire("forgotten", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.Sync(t.Context(), "")
		cur, _ := r.Status("forgotten")
		if cur.Instances == 0 {
			if cur.Status != StatusIdle {
				t.Errorf("a closed instance should leave the extension idle, got %q", cur.Status)
			}
			if cur.ClosedShellID != st.ShellID {
				t.Errorf("the closed instance should be named: got %q want %q", cur.ClosedShellID, st.ShellID)
			}
			if cur.ClosedReason == "" {
				t.Error("the reply should say why the instance went")
			}
			// And the handle that named it explains itself.
			name, reason, at, ok := r.ExplainClosed("", st.ShellID)
			if !ok {
				t.Fatal("the closed handle should be explainable")
			}
			if name != "forgotten" || reason == "" || at.IsZero() {
				t.Errorf("explanation = %q %q %v", name, reason, at)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("an idle instance was never closed: %+v", cur)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestResidentExtensionIsNotOnDemand pins the boundary: a template that declares a
// forward is a listener, and a listener has no caller whose absence could mean
// "idle" — so it is not on demand, and its connection is not a caller's to release.
func TestResidentExtensionIsNotOnDemand(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "resident"
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Args = nil
	ext.Forwards = []ForwardDecl{{Kind: ForwardDynamic}}
	mustSave(t, r.store, &ext)

	// No ask: the walk brings it up because the template asked for a listener.
	st := waitForStatus(t, r, "resident", StatusAvailable)
	if st.Lazy {
		t.Error("a template with a forward should not report itself as on demand")
	}
	if _, err := r.Release("resident", st.ShellID); err == nil {
		t.Error("a resident extension's connection is not the caller's to release")
	}
}

// TestIdleTimeoutMustBeADuration pins the refusal: a window nobody can parse is
// reported rather than silently replaced by the default, because a caller that
// wrote "10 minutes" meant something and running with ten minutes instead of what
// it wrote is the kind of help that hides a typo.
func TestIdleTimeoutMustBeADuration(t *testing.T) {
	// "0" is NOT in this list: it is the word for "no idle collection" (issue #227),
	// the same one the daemon's --idle-timeout uses. It is covered by
	// TestExtensionIdleTimeoutZeroDisablesCollectionPinsIssue227.
	for _, raw := range []string{"10 minutes", "-5m"} {
		ext := Extension{Mode: "pipe"}
		ext.Name = "bad"
		ext.Target = "internal"
		ext.Command = testShellCommand()
		ext.IdleTimeout = raw
		ext.Normalize()
		if err := ext.Validate(); err == nil {
			t.Errorf("idle_timeout %q should be refused", raw)
		}
	}
	ext := Extension{Mode: "pipe"}
	ext.Name = "ok"
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Normalize()
	got, err := ext.IdleTimeoutDuration()
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultIdleTimeout {
		t.Errorf("an absent idle_timeout should mean the default, got %s", got)
	}
}

// TestABrokenProgramIsNotOfferedAsAvailable pins the fresh path's grace. `open ext`
// used to mark the extension available the instant the fork returned, so a template
// whose program does not exist answered 200 with a handle to a process that had
// already exited — and the caller learned it on the next read, or sent its first
// JSON-RPC frame into an empty stream (dogfood on #183).
func TestABrokenProgramIsNotOfferedAsAvailable(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "bad"
	ext.Target = "internal"
	ext.Command = "nosuchprogram-xyz"
	ext.Args = []string{"serve"}
	mustSave(t, r.store, &ext)

	st, err := r.Acquire("bad", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status == StatusAvailable {
		t.Fatalf("a program that does not exist was reported available with shell %q", st.ShellID)
	}
	if st.ShellID != "" {
		t.Errorf("no handle should be handed out for a dead process, got %q", st.ShellID)
	}
	reason := ""
	for _, f := range st.Failures {
		reason += f.Reason + " "
	}
	if !strings.Contains(reason, "executable file not found") && !strings.Contains(reason, "no such file") && !strings.Contains(reason, "command not found") {
		t.Errorf("the failure should carry the program's own message, got %q", reason)
	}
}

// TestTouchExtendsTheWindow pins the half of the idle window that makes it usable:
// a channel being driven is not a channel nobody asked about. The window is 400ms
// and the ask happens at 250ms, so the original deadline passes at 400ms — the
// channel must still be there at 500ms, and must still close once silence is real.
func TestTouchExtendsTheWindow(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "touchy", "400ms")
	st, err := r.Acquire("touchy", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.ShellID == "" {
		t.Fatal("no channel")
	}
	time.Sleep(250 * time.Millisecond)
	r.Touch(st.ShellID, "")
	time.Sleep(250 * time.Millisecond)
	if cur, _ := r.Status("touchy"); cur.Instances == 0 {
		t.Fatalf("the window was not extended by use: %+v", cur)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		cur, _ := r.Status("touchy")
		if cur.Instances == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never closed after silence: %+v", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDrivingAChannelResetsTheReportedSilence pins the other half of the field: a
// caller asking "how long have I got" reads idle_for_ms, so a channel it is driving
// must reset that clock — otherwise the number says the instance is idle while the
// instance is being used (dogfood on #183).
func TestDrivingAChannelResetsTheReportedSilence(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "clocked", "10m")
	st, err := r.Acquire("clocked", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.ShellID == "" {
		t.Fatal("no channel")
	}
	time.Sleep(300 * time.Millisecond)
	before, _ := r.Status("clocked")
	r.Touch(st.ShellID, "")
	after, _ := r.Status("clocked")
	if before.IdleForMs < 250 {
		t.Fatalf("the clock did not move before the touch: %d ms", before.IdleForMs)
	}
	if after.IdleForMs > 150 {
		t.Errorf("driving the channel left the reported silence at %d ms (was %d)", after.IdleForMs, before.IdleForMs)
	}
}

// TestStatusReportsTheNewestInstancePinsIssue230 pins that repeated asks for an
// on-demand extension report the newest instance in Status/snapshot rather than
// sticking to the first one (issue #230).
func TestStatusReportsTheNewestInstancePinsIssue230(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "multichannel", "10m")

	first, err := r.Acquire("multichannel", 10*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := r.Acquire("multichannel", 10*time.Second)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	if first.ShellID == second.ShellID {
		t.Fatal("every ask must get its own channel, got the same shell_id twice")
	}

	current, ok := r.Status("multichannel")
	if !ok {
		t.Fatal("extension disappeared from status")
	}

	if current.ShellID != second.ShellID {
		t.Fatalf("Status().ShellID = %q, want newest instance %q (issue #230: old code returned the first instance %q)",
			current.ShellID, second.ShellID, first.ShellID)
	}
	if current.Instances != 2 {
		t.Fatalf("Instances = %d, want 2", current.Instances)
	}
}

// TestExtensionIdleTimeoutZeroDisablesCollectionPinsIssue227 pins that
// idle_timeout = "0" is accepted by configuration validation and prevents instances
// from ever being collected for silence (issue #227).
func TestExtensionIdleTimeoutZeroDisablesCollectionPinsIssue227(t *testing.T) {
	r := startReconciler(t)

	// 1. Negative is still rejected.
	neg := &Extension{Name: "neg", Target: "internal", IdleTimeout: "-5s"}
	if _, err := neg.IdleTimeoutDuration(); err == nil {
		t.Fatal("negative idle_timeout should be rejected")
	}

	// 2. Zero is accepted as "no idle collection".
	zero := &Extension{Name: "forever", Target: "internal", IdleTimeout: "0"}
	dur, err := zero.IdleTimeoutDuration()
	if err != nil {
		t.Fatalf("idle_timeout=0 should be accepted, got: %v", err)
	}
	if dur != 0 {
		t.Fatalf("expected duration 0, got %v", dur)
	}

	// 3. Runtime verification: instances are NEVER reaped when limit <= 0.
	lazyExt(t, r, "forever", "0")
	inst, err := r.Acquire("forever", 10*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	st := r.state("forever")
	// Sweep with limit = 0: must return 0 reaped.
	if reaped := r.reapDue("forever", st, 0); reaped != 0 {
		t.Fatalf("reapDue with limit=0 reaped %d instances, want 0", reaped)
	}

	current, ok := r.Status("forever")
	if !ok || current.Instances != 1 || current.ShellID != inst.ShellID {
		t.Fatalf("instance was reaped despite idle_timeout=0: %+v", current)
	}
}
