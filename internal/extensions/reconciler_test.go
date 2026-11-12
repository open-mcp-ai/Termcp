package extensions

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// testShellCommand is a command that exists on every CI host and blocks until
// its stdin closes, so a probe finds it still running after the grace window.
func testShellCommand() string {
	return "cat"
}

// immediateExitCommand is a command the host actually has that exits at once
// with a nonzero code, so a probe observes the exit inside its grace window and
// reports a startup failure rather than a dial failure.
func immediateExitCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-Command", "exit 3"}
	}
	return "bash", []string{"-c", "echo boom; exit 3"}
}

// startReconciler stands up the whole runtime an extension needs: a loopback SSH
// server, a session manager, both stores, and a running reconciler. Registering
// cleanup after t.TempDir (LIFO) keeps the exit watchers ahead of the directory
// removal.
func startReconciler(t *testing.T) *Reconciler {
	t.Helper()
	srv := sshserver.New()
	// The in-process MCP bridge is what an extension with a url runs on its
	// channel (production registers it at startup, see main.go), so every test
	// that drives the extension layer gets the same server an instance has.
	mcpbridge.RegisterBuiltin(srv, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})
	r := NewReconciler(NewStore(dir), sessMgr, msgMgr, sshconfig.NewStore(dir), forward.NewForwardManager())
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop() })
	return r
}

// wantExtension asks for an on-demand extension, which is what any caller does
// before it can drive one: since issue #183 the reconciler does not dial a
// template that declares no forward until something asks, and asking is
// `open ext <name>` (Reconciler.Acquire) or the extension tool's action=use.
//
// The wait helpers deliberately do NOT call it. A test that expects an extension
// up says so in one visible line, and a test that expects it to stay down — or to
// go down and stay down until asked again — says nothing.
func wantExtension(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	if _, err := r.Acquire(name, 0); err != nil {
		t.Fatalf("asking for extension %q: %v", name, err)
	}
}

// waitForStatus polls until want holds or the deadline passes. Extensions are
// asynchronous by design — the reconciler proves availability off the caller's
// thread — so the tests wait on the outcome rather than assuming it.
func waitForStatus(t *testing.T, r *Reconciler, name, want string) Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		st, ok := r.Status(name)
		if ok && st.Status == want {
			return st
		}
		if time.Now().After(deadline) {
			st, _ := r.Status(name)
			dumpReconciler(t, r, name)
			t.Fatalf("extension %q never reached %s (last: %+v)", name, want, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustSave(t *testing.T, store *Store, ext *Extension) {
	t.Helper()
	if err := store.Save(ext); err != nil {
		t.Fatal(err)
	}
}

// dumpReconciler prints everything about an extension and the sessions the
// manager holds, for diagnosing a reconciler test that stopped making progress.
func dumpReconciler(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	st, _ := r.Status(name)
	t.Logf("extension %q: %+v", name, st)
	r.mu.Lock()
	for n, s := range r.states {
		s.mu.Lock()
		t.Logf("state %q: status=%q session=%q shell=%q failures=%d", n, s.status, s.sessionID, s.shellID, len(s.failures))
		s.mu.Unlock()
	}
	r.mu.Unlock()
	for _, s := range r.sessMgr.ListAll() {
		t.Logf("session %s: name=%q status=%q ssh=%q", s.ID, s.Name, s.Status, s.SSHConfig)
	}
}

// waitForFailureReason polls until an extension has recorded a failure whose
// reason contains substr. The initial status of any extension is unavailable, so
// waiting on the status alone cannot prove the reconciler ran — only a recorded
// failure can — and the reason is what distinguishes "the process said why" from
// "something else happened".
func waitForFailureReason(t *testing.T, r *Reconciler, name, substr string) Status {
	t.Helper()
	// 20s, and the two constants below are why 8s was not enough.
	//
	// probeGrace is 700ms: a process that takes longer than that to die is
	// reported AVAILABLE first, and its death is only noticed by the next walk,
	// which is loopInterval (5s) away. On a cold Windows runner powershell.exe
	// spends 1-2s just starting, so the sequence is grace miss (0.7s), cold start
	// (up to 2s), next walk (up to 5s), plus a channel reopen — already 8s with no
	// margin, which is exactly where this failed: "never recorded a failure
	// containing exit code (last: {Status:available ... Failures:[]})".
	//
	// The deadline is not the thing being tested; it is the backstop. Two walks
	// leave room for a loaded runner without hiding a real regression, which would
	// never record a failure at all.
	deadline := time.Now().Add(20 * time.Second)
	for {
		// The walk is what notices a process that ended; the tick that drives it is
		// seconds away, so nudge it rather than making the test pay for the interval.
		r.Sync(t.Context(), "")
		if st, ok := r.Status(name); ok {
			for _, f := range st.Failures {
				if strings.Contains(f.Reason, substr) {
					return st
				}
			}
		}
		if time.Now().After(deadline) {
			st, _ := r.Status(name)
			t.Fatalf("extension %q never recorded a failure containing %q (last: %+v)", name, substr, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForFailure polls until an extension has recorded a failure in stage. The
// initial status of any extension is unavailable, so waiting on the status alone
// cannot prove the reconciler ran — only a recorded failure can.
func waitForFailure(t *testing.T, r *Reconciler, name, stage string) Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		r.Sync(t.Context(), "")
		st, ok := r.Status(name)
		if ok {
			for _, f := range st.Failures {
				if f.Stage == stage {
					return st
				}
			}
		}
		if time.Now().After(deadline) {
			st, _ := r.Status(name)
			t.Fatalf("extension %q never recorded a %s failure (last: %+v)", name, stage, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReconcilerBringsUpSkillExtension pins the simplest full path: an enabled
// skill extension is connected by the reconciler, ends up available, and the
// session it created carries the restriction the template declared.
func TestReconcilerBringsUpSkillExtension(t *testing.T) {
	r := startReconciler(t)
	// The whitelist is declared, not implied: a template says what it may do, and a
	// hand-written one that omits allow is unrestricted rather than silently narrowed.
	ext := Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/skills/docs/"}
	ext.Allow = []string{capability.FileRead, capability.FileStat}
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "docs")
	st := waitForStatus(t, r, "docs", StatusAvailable)
	if st.SessionID == "" {
		t.Fatal("available extension reports no session")
	}
	if st.ShellID != "" {
		t.Errorf("a shell-less extension should report no primary shell, got %q", st.ShellID)
	}
	sess := r.sessMgr.Get(st.SessionID)
	if sess == nil {
		t.Fatal("reported session not found in the manager")
	}
	rest := sess.Restriction()
	if !rest.AllowsTool("file_read") {
		t.Error("the session should allow file_read")
	}
	if rest.AllowsTool("file_write") {
		t.Error("the session should not allow file_write")
	}
	if !rest.AllowsPath("/srv/skills/docs/SKILL.md") {
		t.Error("the session should reach into its declared path")
	}
	if rest.AllowsPath("/etc/passwd") {
		t.Error("the session should not reach outside its declared path")
	}
}

// TestReconcilerReportsStartupFailure pins that a process that cannot stay
// running is reported as unavailable with the reason it printed, rather than
// being silently retried forever or reported as up.
//
// Which stage records it is timing the test does not get to decide: a command
// that takes longer to die than the probe's grace window is caught by the next
// walk instead ("session"), one that dies inside the window by the probe
// ("start"). Both carry the process's own exit line, which is the part that tells
// an author what to fix, so both stages are accepted here.
func TestReconcilerReportsStartupFailure(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "broken"
	ext.Target = "internal"
	ext.Command, ext.Args = immediateExitCommand()
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "broken")
	st := waitForFailureReason(t, r, "broken", "exit code")
	last := st.Failures[len(st.Failures)-1]
	if last.Stage != "start" && last.Stage != "session" {
		t.Errorf("failure stage = %q, want start or session", last.Stage)
	}
	if st.ShellID != "" {
		t.Error("a failed startup should not leave a channel handle behind")
	}
	// NOTHING RETRIES AN ON-DEMAND EXTENSION BUT A CALLER (issue #183). A backoff
	// timer would be a dial nobody asked for, so none is scheduled — and the
	// connection goes with the instance, because there is nothing left to carry.
	if st.NextRetry != 0 {
		t.Errorf("an on-demand extension should schedule no retry of its own, got %d", st.NextRetry)
	}
	// THE CONNECTION STAYS (issue #183): it is the install's, not the failed
	// instance's, and tearing it down would make the next ask pay for a dial to
	// learn the same thing about the same template.
	if n := len(r.sessMgr.ListAll()); n > 1 {
		t.Errorf("a failed instance should leave at most the connection behind, got %d sessions", n)
	}
	// Asking again is what tries again, and it fails again for the same reason: the
	// template, not the attempt, is what is wrong.
	// The proof that it tried again is a NEW instance that also ended: the failure
	// list deduplicates a repeated reason, so counting entries would prove nothing.
	firstGone := st.ClosedShellID
	wantExtension(t, r, "broken")
	// The new instance ends too, and the WALK is what notices it; the deadline is a
	// backstop for a loaded runner, not the thing being measured.
	retryDeadline := time.Now().Add(20 * time.Second)
	for {
		r.Sync(t.Context(), "")
		again, _ := r.Status("broken")
		if again.ClosedShellID != "" && again.ClosedShellID != firstGone {
			break
		}
		if time.Now().After(retryDeadline) {
			t.Fatalf("asking again should try again, got %+v", again)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReconcilerMissingTarget pins the unrecoverable case: a target profile that
// does not exist is reported once, and no session is ever created for it.
func TestReconcilerMissingTarget(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "nowhere"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "nowhere")
	st := waitForFailure(t, r, "nowhere", "target")
	if st.NextRetry != 0 {
		t.Error("a missing target is not transient; no retry should be scheduled")
	}
	if len(r.sessMgr.ListAll()) != 0 {
		t.Errorf("no session should have been created, got %d", len(r.sessMgr.ListAll()))
	}
	// Tell it again, twice, and prove a spent, unbuildable template is stepped over
	// rather than re-probed: the failure line stays the one already recorded, and
	// nothing dials or builds a session for it.
	for i := 0; i < 2; i++ {
		r.Sync(t.Context(), "")
		time.Sleep(150 * time.Millisecond)
	}
	st, _ = r.Status("nowhere")
	if len(st.Failures) != 1 {
		t.Errorf("the walk should not re-probe a template it cannot build, got %d: %+v", len(st.Failures), st.Failures)
	}
	if st.NextRetry != 0 {
		t.Error("the budget stays spent while the template stays unbuildable")
	}
}

// TestReconcilerTargetAppearsLater pins the recovery path for a template that
// could not build: the profile it points at is created by hand, and the
// extension comes up without a nudge. This is why a target failure spends the
// budget instead of climbing the backoff ladder — nothing the timer did was
// going to create the profile.
func TestReconcilerTargetAppearsLater(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "late-target"
	ext.Target = "arrives"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "late-target")
	waitForFailure(t, r, "late-target", "target")

	if err := r.SSHConfigs().Save("arrives", []byte("kind = \"internal\"\n")); err != nil {
		t.Fatal(err)
	}

	wantExtension(t, r, "late-target")
	st := waitForStatus(t, r, "late-target", StatusAvailable)
	if st.SessionID == "" {
		t.Error("the extension should run once its profile exists")
	}
}

// TestReconcilerDisabled pins that a disabled extension is left entirely alone:
// no session, no attempt, and the status says why nothing is happening.
func TestReconcilerDisabled(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "off"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.SetEnabled(false)
	mustSave(t, r.store, &ext)

	st := waitForStatus(t, r, "off", StatusDisabled)
	if st.SessionID != "" {
		t.Error("a disabled extension should have no session")
	}
	if len(r.sessMgr.ListAll()) != 0 {
		t.Errorf("a disabled extension should create no session, got %d", len(r.sessMgr.ListAll()))
	}
}

// TestReconcilerRetry pins that Retry short-circuits the backoff: fixing the
// underlying problem and asking again brings the extension up without waiting
// for the timer.
func TestReconcilerRetry(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "late"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)
	wantExtension(t, r, "late")
	waitForFailure(t, r, "late", "target")

	ext.Target = "internal"
	mustSave(t, r.store, &ext)
	if err := r.Retry("late"); err != nil {
		t.Fatal(err)
	}
	wantExtension(t, r, "late")
	waitForStatus(t, r, "late", StatusAvailable)
}

// TestReconcilerRetryDisabledRefused pins that retrying a disabled extension is
// an error rather than a silent no-op, since the caller asked for something the
// config forbids.
func TestReconcilerRetryDisabledRefused(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "off"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.SetEnabled(false)
	mustSave(t, r.store, &ext)
	waitForStatus(t, r, "off", StatusDisabled)

	err := r.Retry("off")
	if err == nil {
		t.Fatal("retrying a disabled extension should fail")
	}
}

// TestReconcilerSessionDeathMarksUnavailable pins the recovery contract: a
// session that dies on its own takes its extension to unavailable with a
// recorded reason, and the reconciler then brings it back.
func TestReconcilerSessionDeathMarksUnavailable(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "live"
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Args = nil
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "live")
	st := waitForStatus(t, r, "live", StatusAvailable)
	sess := r.sessMgr.Get(st.SessionID)
	if sess == nil {
		dumpReconciler(t, r, "live")
		t.Fatal("session missing for a live extension")
	}

	r.sessMgr.Terminate(sess.ID, true, 0)
	st = waitForStatus(t, r, "live", StatusUnavailable)
	found := false
	for _, f := range st.Failures {
		if f.Stage == "session" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the extension's death should be recorded, got %+v", st.Failures)
	}

	// The backoff is short enough that the loop restores it on its own, on a
	// connection rebuilt underneath it.
	wantExtension(t, r, "live")
	st = waitForStatus(t, r, "live", StatusAvailable)
	if st.SessionID == sess.ID {
		t.Error("the extension should be served by a new session after the old one died")
	}
}

// TestReconcilerForwardEstablished pins that a declared forward is set up with
// the extension and its bound address is reported, including the port chosen
// when the template asked for an ephemeral one.
func TestReconcilerForwardEstablished(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "tunnel"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Forwards = []ForwardDecl{{
		DirectionKind:       ForwardDynamic,
		LocalHost:  "127.0.0.1",
		LocalPort:  0,
		RemoteHost: "localhost",
		RemotePort: 0,
	}}
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "tunnel")
	st := waitForStatus(t, r, "tunnel", StatusAvailable)
	if len(st.Forwards) != 1 {
		t.Fatalf("want 1 established forward, got %+v", st.Forwards)
	}
	fw := st.Forwards[0]
	if fw.ListenAddr == "" {
		t.Error("a dynamic forward should report the port it actually bound")
	}
	if !strings.Contains(fw.ListenAddr, "127.0.0.1:") {
		t.Errorf("listen address %q should be the declared local host", fw.ListenAddr)
	}
	if fw.ForwardID == "" || r.forwardMgr == nil {
		// forwardMgr is present in these tests; a missing id means nothing bound.
		if fw.ForwardID == "" {
			t.Error("the established forward should carry its forward id")
		}
	}
	if r.forwardMgr != nil {
		live := false
		for _, f := range r.forwardMgr.ListBySession(st.SessionID) {
			if f.ForwardID == fw.ForwardID {
				live = true
			}
		}
		if !live {
			t.Error("the declared forward is not registered with the forward manager")
		}
	}
}

// TestReconcilerEditRebuildsTheChannel pins the edit contract under sharing: a
// template edit rebuilds what the extension itself holds — its channel — while the
// connection it shares with its host's other extensions stays up. The session is
// the group's, and rebuilding it to change one member's command would redial the
// host for everyone.
func TestReconcilerEditRebuildsTheChannel(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "edited"
	ext.Target = "internal"
	ext.Command = testShellCommand()
	ext.Args = nil
	mustSave(t, r.store, &ext)
	wantExtension(t, r, "edited")
	st := waitForStatus(t, r, "edited", StatusAvailable)
	firstShell := st.ShellID
	if firstShell == "" {
		t.Fatal("an mcp extension should report the channel it runs on")
	}

	ext.Command = "sort"
	mustSave(t, r.store, &ext)

	// The edit closed the instance built from the old command, so the caller asks
	// again and gets one built from the new one (issue #183).
	wantExtension(t, r, "edited")
	deadline := time.Now().Add(8 * time.Second)
	for {
		st, _ = r.Status("edited")
		if st.Status == StatusAvailable && st.ShellID != "" && st.ShellID != firstShell {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("channel was not replaced after the edit (still %q)", st.ShellID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The new instance runs the new command. It is NOT promised the old connection:
	// an on-demand extension holds nothing while nobody is using it, so the edit
	// closes the old instance and the connection with it (issue #183).
	if st.SessionID == "" {
		t.Error("the rebuilt instance should report the session it runs on")
	}
}

// TestReconcilerSharesOneSessionPerHost pins the singleton: two extensions that
// dial the same profile with the same capability set are served by one connection,
// each through its own channel. The point of the grouping is that one extension
// per tool stays affordable — a second MCP server on a host costs a channel, not a
// dial.
func TestReconcilerSharesOneSessionPerHost(t *testing.T) {
	r := startReconciler(t)
	for _, name := range []string{"first", "second"} {
		ext := Extension{Mode: "pipe"}
		ext.Name = name
		ext.Target = "internal"
		ext.Command = testShellCommand()
		ext.Args = nil
		mustSave(t, r.store, &ext)
	}

	wantExtension(t, r, "first")
	first := waitForStatus(t, r, "first", StatusAvailable)
	wantExtension(t, r, "second")
	second := waitForStatus(t, r, "second", StatusAvailable)

	if first.SessionID != second.SessionID {
		t.Fatalf("extensions on one host should share a session: %q vs %q", first.SessionID, second.SessionID)
	}
	if first.ShellID == second.ShellID || first.ShellID == "" || second.ShellID == "" {
		t.Fatalf("each extension should drive its own channel: %q vs %q", first.ShellID, second.ShellID)
	}
	if n := len(r.sessMgr.ListAll()); n != 1 {
		t.Errorf("two extensions on one host should hold one session, got %d", n)
	}

	// Both channels are in the one session, so either extension's handle resolves
	// to the same connection.
	sess := r.sessMgr.Get(first.SessionID)
	if sess == nil {
		t.Fatal("shared session not found in the manager")
	}
	if sess.GetChildShell(first.ShellID) == nil || sess.GetChildShell(second.ShellID) == nil {
		t.Error("both extensions' channels should live in the shared session")
	}

	// Killing the shared connection takes both extensions down, and both come back
	// on a new one: the singleton is rebuilt, not left half-served.
	r.sessMgr.Terminate(sess.ID, true, 0)
	waitForStatus(t, r, "first", StatusUnavailable)
	waitForStatus(t, r, "second", StatusUnavailable)
	wantExtension(t, r, "first")
	first = waitForStatus(t, r, "first", StatusAvailable)
	wantExtension(t, r, "second")
	second = waitForStatus(t, r, "second", StatusAvailable)
	if first.SessionID == sess.ID {
		t.Error("the extension should not still report the dead session")
	}
	if first.SessionID != second.SessionID {
		t.Errorf("both extensions should come back on the same rebuilt session: %q vs %q (first=%+v second=%+v)",
			first.SessionID, second.SessionID, first, second)
	}
	if n := len(r.sessMgr.ListAll()); n != 1 {
		t.Errorf("the rebuilt group should hold one session, got %d", n)
	}
}

// TestReconcilerSeparatesByCapability pins the boundary of the sharing: a session
// enforces one whitelist, so two extensions that were granted different
// capabilities are served by different connections. Sharing them would hand each
// of them the other's reach — the one thing the layer promises it cannot do.
func TestReconcilerSeparatesByCapability(t *testing.T) {
	r := startReconciler(t)
	first := Extension{}
	first.Name = "docs-a"
	first.Target = "internal"
	first.Paths = []string{"/srv/skills/a/"}
	mustSave(t, r.store, &first)

	second := Extension{}
	second.Name = "docs-b"
	second.Target = "internal"
	second.Paths = []string{"/srv/skills/b/"}
	mustSave(t, r.store, &second)

	wantExtension(t, r, "docs-a")
	a := waitForStatus(t, r, "docs-a", StatusAvailable)
	wantExtension(t, r, "docs-b")
	b := waitForStatus(t, r, "docs-b", StatusAvailable)
	if a.SessionID == b.SessionID {
		t.Fatal("extensions with different path windows must not share a session")
	}
	if n := len(r.sessMgr.ListAll()); n != 2 {
		t.Errorf("want one session per capability set, got %d", n)
	}

	// The window each one declares is the window its session enforces: neither
	// session can reach into the other's tree.
	for _, tc := range []struct {
		sid    string
		mine   string
		theirs string
	}{
		{a.SessionID, "/srv/skills/a/SKILL.md", "/srv/skills/b/SKILL.md"},
		{b.SessionID, "/srv/skills/b/SKILL.md", "/srv/skills/a/SKILL.md"},
	} {
		sess := r.sessMgr.Get(tc.sid)
		if sess == nil {
			t.Fatalf("session %q not found", tc.sid)
		}
		rest := sess.Restriction()
		if !rest.AllowsPath(tc.mine) {
			t.Errorf("session %q should reach %q", tc.sid, tc.mine)
		}
		if rest.AllowsPath(tc.theirs) {
			t.Errorf("session %q must not reach the other extension's path %q", tc.sid, tc.theirs)
		}
	}
}

// TestReconcilerSessionIsNamespaced pins that the sessions the reconciler creates
// carry the namespace their text and archives are hidden by, and that they are
// named after the group so a leftover is identifiable.
func TestReconcilerSessionIsNamespaced(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "hidden"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "hidden")
	st := waitForStatus(t, r, "hidden", StatusAvailable)
	sess := r.sessMgr.Get(st.SessionID)
	if sess == nil {
		t.Fatal("reported session not found in the manager")
	}
	info := sess.Info()
	if !ns.IsExtensions(info.Namespace) {
		t.Errorf("namespace = %q, want an extension namespace", info.Namespace)
	}
	if !ns.Hidden(info.Namespace) {
		t.Error("a session in a namespace must be hidden from the ordinary surfaces")
	}
	if info.Name != ns.ForExtensions("internal") {
		t.Errorf("session name = %q, want the group it belongs to", info.Name)
	}
}

// TestReconcilerSweepsRestoredSessions pins the restart path: a session a
// previous process kept for an extension comes back from its manifest as a
// placeholder nothing can be driven through, so the reconciler drops it instead of
// leaving an entry that can never be reopened.
func TestReconcilerSweepsRestoredSessions(t *testing.T) {
	r := startReconciler(t)
	// The session stands in for one a previous run left behind: same namespace,
	// created outside this reconciler's groups.
	cfg := session.Config{Name: "ext:internal", SSHConfig: "internal", NoShell: true, Namespace: ns.ForExtensions("internal")}
	leftover, err := r.sessMgr.Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A second reconciler over the same manager is what a restart looks like from
	// the manager's side.
	restarted := NewReconciler(NewStore(t.TempDir()), r.sessMgr, r.msgMgr, r.sshConfigs, r.forwardMgr)
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for r.sessMgr.Get(leftover.ID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("a restored extension session should be swept on start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconcilerDeleteTearsDown pins that removing an extension from the store
// closes the session it kept warm, so an extension that is gone is gone.
func TestReconcilerDeleteTearsDown(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "gone"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)
	wantExtension(t, r, "gone")
	st := waitForStatus(t, r, "gone", StatusAvailable)
	sid := st.SessionID

	if err := r.store.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, ok := r.Status("gone"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted extension's state was never dropped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	sess := r.sessMgr.Get(sid)
	if sess == nil {
		return // deleted outright, which also satisfies "closed"
	}
	// markDead publishes the exited status after flushing the session's output
	// pipes, so it lands slightly after Terminate returns.
	shutdownDeadline := time.Now().Add(3 * time.Second)
	for sess.Info().Status == api.SessionRunning {
		if time.Now().After(shutdownDeadline) {
			t.Fatal("a deleted extension's session should not still be running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconcilerStoreChangeTriggers pins the wiring: saving an extension through
// the store reaches the reconciler without an explicit poke.
func TestReconcilerStoreChangeTriggers(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{}
	ext.Name = "wired"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)
	// No Trigger call: the store's own change callback is the path production uses.
	wantExtension(t, r, "wired")
	waitForStatus(t, r, "wired", StatusAvailable)
}

// TestReconcilerStatusUnknownName pins the lookup contract: a name the store
// does not hold is reported as absent, not as an empty status.
func TestReconcilerStatusUnknownName(t *testing.T) {
	r := startReconciler(t)
	if _, ok := r.Status("never-saved"); ok {
		t.Error("an unsaved extension should not report a status")
	}
	if err := r.Retry("never-saved"); !errors.Is(err, ErrNotFound) {
		t.Errorf("retry of a missing extension = %v, want ErrNotFound", err)
	}
}

// shortLivedCommand runs for longer than the probe's grace window and then exits
// on its own: the shape of a process that starts fine and dies later.
func shortLivedCommand() (string, []string) {
	// Long enough that the reconciler reports the extension available BEFORE the
	// process exits. With 2s, a slow CI runner could spend that whole window
	// starting the process, so "died after being reported available" never
	// happened — the test then waited for a transition it had already missed and
	// failed with a stale `available` status. The test is about noticing a death,
	// not about being fast.
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-Command", "Start-Sleep -Seconds 6"}
	}
	return "bash", []string{"-c", "sleep 6"}
}

// TestReconcilerReportsAnInstanceThatEnded pins what an on-demand extension does
// with a process that exits after it was reported available: the instance is gone,
// the exit line it left behind is recorded as the reason, and nothing is reopened
// on its own — the next caller asks, and gets an instance built from the template
// again (issue #183).
//
// The reason is the product here. A handle whose process exited is dead either
// way, so what the daemon owes the caller is not a repair but the diagnosis: "the
// process exited with code 7" is the difference between a wrong template and a
// wrong host.
func TestReconcilerReportsAnInstanceThatEnded(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "short-lived"
	ext.Target = "internal"
	ext.Command, ext.Args = shortLivedCommand()
	mustSave(t, r.store, &ext)

	wantExtension(t, r, "short-lived")
	st := waitForStatus(t, r, "short-lived", StatusAvailable)
	firstShell := st.ShellID
	if firstShell == "" {
		t.Fatal("an mcp extension should report the channel it runs on")
	}

	// The process exits on its own; nothing notifies the reconciler, so the walk is
	// what notices (nudged here rather than waited for, to keep the test quick).
	//
	// 25s, not 12s: the countdown does not start until the process has been reported
	// available, and it then has to outlive shortLivedCommand (6s) plus however long
	// a CI runner takes to start PowerShell and notice the exit.
	deadline := time.Now().Add(25 * time.Second)
	for {
		r.Sync(t.Context(), "")
		cur, _ := r.Status("short-lived")
		if cur.Instances == 0 {
			if cur.Status == StatusAvailable {
				t.Errorf("an extension with no instances should not read as available: %+v", cur)
			}
			found := false
			for _, f := range cur.Failures {
				if f.Stage == "session" {
					found = true
				}
			}
			if !found {
				t.Errorf("the process's death should be recorded as a session failure, got %+v", cur.Failures)
			}
			if cur.ClosedShellID != firstShell {
				t.Errorf("the instance that ended should be named in the reply, got %q want %q", cur.ClosedShellID, firstShell)
			}
			// Asking again is the repair, and it gets a channel of its own.
			wantExtension(t, r, "short-lived")
			again := waitForStatus(t, r, "short-lived", StatusAvailable)
			if again.ShellID == "" || again.ShellID == firstShell {
				t.Errorf("a new ask should get a new channel, got %q (first was %q)", again.ShellID, firstShell)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ended process was never noticed (last: %+v)", cur)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
