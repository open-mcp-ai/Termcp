package extensions

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// TestApplyArgsRefusesMalformedCapabilityFields pins the fail-closed rule for the
// fields where a silent default is a security decision.
//
// The array readers used to fall back to "no change" on a value of the wrong type,
// which for an array field meant CLEARING it — and an empty allow list is the
// unrestricted default, so `allow: [1,2]` did not narrow the template, it dropped
// the whitelist entirely.
func TestApplyArgsRefusesMalformedCapabilityFields(t *testing.T) {
	cases := []map[string]any{
		{"allow": []any{1, 2}},
		{"allow": "file_read"},
		{"paths": []any{"/srv/", 7}},
		{"args": 5},
		{"enabled": "true"},
		{"forwards": []any{"local"}},
		{"forwards": "local"},
	}
	for _, args := range cases {
		ext := Extension{}
		ext.Name = "probe"
		ext.Target = "internal"
		if err := ApplyArgs(&ext, args, true); err == nil {
			t.Errorf("%v: malformed input must be refused, not coerced (allow=%v paths=%v)", args, ext.Allow, ext.Paths)
		}
	}

	// The well-formed shapes still work, including the documented empty array.
	ext := Extension{}
	ext.Name = "probe"
	ext.Target = "internal"
	ext.Allow = []string{"file_read"}
	if err := ApplyArgs(&ext, map[string]any{"allow": []any{}}, true); err != nil {
		t.Fatalf("clearing an array must stay legal: %v", err)
	}
	if len(ext.Allow) != 0 {
		t.Errorf("an empty array should clear the field, got %v", ext.Allow)
	}
	if err := ApplyArgs(&ext, map[string]any{"allow": []any{"file_read", "file_stat"}}, true); err != nil {
		t.Fatalf("ApplyArgs: %v", err)
	}
	if len(ext.Allow) != 2 {
		t.Errorf("allow = %v, want two entries", ext.Allow)
	}
}

// TestApplyArgsDropsOnExit pins that the removed field is tolerated but not
// stored. A template that still carries on_exit — hand-written, or written by an
// older build — keeps loading, and the value is dropped rather than kept as
// configuration nothing reads.
func TestApplyArgsDropsOnExit(t *testing.T) {
	ext := Extension{}
	ext.Name = "probe"
	ext.Target = "internal"
	// A real declaration, so this test stays about on_exit being dropped rather than
	// about a capability-less template being refused (see Validate).
	ext.Mode = "pty"
	if err := ApplyArgs(&ext, map[string]any{"on_exit": "close"}, true); err != nil {
		t.Fatalf("ApplyArgs: %v", err)
	}
	if err := ext.Validate(); err != nil {
		t.Errorf("a template carrying on_exit must still validate: %v", err)
	}
}

// TestStateChangeDetectsBothReadings pins the two readings themselves: "this
// extension moved to another connection" and "this is a different template" are
// independent facts about one edit.
func TestStateChangeDetectsBothReadings(t *testing.T) {
	st := &extState{status: StatusUnavailable}
	ext := Extension{}
	ext.Name = "probe"
	ext.Target = "internal"

	// First sight: the group is set and the template is recorded.
	st.setGroup(groupKey(&ext))
	if !st.changed(&ext) {
		t.Fatal("the first walk should see the template as new")
	}

	// Nothing changed: neither reading fires.
	if st.setGroup(groupKey(&ext)) || st.changed(&ext) {
		t.Fatal("an unchanged template must not read as a change")
	}

	// The group moves AND the template differs. Both readings must be true on
	// this walk, so the fingerprint is updated here rather than on the next one.
	ext.Paths = []string{"/srv/"}
	moved := st.setGroup(groupKey(&ext))
	differed := st.changed(&ext)
	if !moved || !differed {
		t.Fatalf("moving groups and editing a template are independent: moved=%v differed=%v", moved, differed)
	}
	if st.changed(&ext) {
		t.Error("the fingerprint must have been recorded on the walk that saw the change")
	}
}

// TestEnsureForwardsKeepsWhatCameUpBeforeAFailure pins the retry path for a
// template that declares several tunnels.
//
// A list where a later entry fails must record the entries that DID come up.
// Recording nothing left those listeners live but unrecorded, so the next attempt
// rebuilt them onto a port it already held — the extension then failed forever on
// its own success.
//
// The failure is produced honestly: the test holds a port, so the second tunnel's
// bind fails while the first (ephemeral) one succeeds. Both declarations pass
// Validate, which is the point — the failure is a runtime condition, not a typo.
func TestEnsureForwardsKeepsWhatCameUpBeforeAFailure(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	busy, _ := strconv.Atoi(strings.TrimPrefix(held.Addr().String(), "127.0.0.1:"))
	if busy == 0 {
		t.Fatalf("cannot read the held port from %q", held.Addr().String())
	}

	r := startQuietReconciler(t)
	ext := Extension{}
	ext.Name = "partial"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Forwards = []ForwardDecl{
		// This one binds an ephemeral port and succeeds.
		{DirectionKind: ForwardDynamic, LocalPort: 0},
		// This one asks for the port the test is holding, so it fails.
		{DirectionKind: ForwardDynamic, LocalHost: "127.0.0.1", LocalPort: busy},
	}
	mustSave(t, r.store, &ext)

	// The attempt runs on its own goroutine, so the failure is waited for rather
	// than driven: the save above already told the reconciler which template moved.
	st := r.state(ext.Name)
	deadline := time.Now().Add(8 * time.Second)
	for {
		r.Sync(t.Context(), ext.Name)
		snap := st.snapshot()
		if len(snap.Failures) > 0 {
			if len(snap.Forwards) == 0 {
				t.Fatalf("the tunnel that DID bind must be recorded, or the retry rebuilds it onto its own port: %+v", snap)
			}
			if snap.Forwards[0].ForwardID == "" {
				t.Errorf("the recorded forward should carry its id, got %+v", snap.Forwards[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the occupied port was never recorded as a failure: %+v", snap)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReconcileWalkRecordsTheFingerprintWhenTheGroupMoves pins the CALL SITE,
// which is where the bug was: the walk asked both readings with ||, so an edit
// that moved the group short-circuited the fingerprint update.
//
// The observable cost was a rebuild nobody asked for. Any paths or allow change
// moves the group — they are part of the group key — so the next walk saw the
// stale fingerprint, called it a change, and tore down the channel and tunnels it
// had just built.
//
// The walk is driven directly rather than through the loop, and the loop is never
// started: the background tick repairs a stale fingerprint on its own, so a test
// that waited on the running reconciler would pass either way.
func TestReconcileWalkRecordsTheFingerprintWhenTheGroupMoves(t *testing.T) {
	r := startQuietReconciler(t)
	ext := Extension{}
	ext.Name = "fingerprint"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/one/"}
	mustSave(t, r.store, &ext)

	// The save armed it; one sync brings it up and records the template it was built
	// from.
	r.Sync(t.Context(), ext.Name)

	// The edit moves the group AND changes the fingerprint. One sync must record
	// both, or the next one calls it a change again.
	ext.Paths = []string{"/srv/two/"}
	mustSave(t, r.store, &ext)
	r.Sync(t.Context(), ext.Name)

	st := r.state(ext.Name)
	st.mu.Lock()
	got := st.fingerprint
	st.mu.Unlock()
	if want := fingerprint(&ext); got != want {
		t.Errorf("the walk that saw the group move left the fingerprint stale: got %q, want %q", got, want)
	}
}

// TestWalkReadsTheBackoffWindowUnderTheLock pins the read that raced.
//
// The walk asks three questions about one extension — status, retry budget, and
// when the backoff window elapses — and the third was read straight off the struct
// while resetRetryBudget (the manual-retry path: an MCP call or the panel's key)
// was clearing it under the mutex. The detector flags the unsynchronised access;
// the reason it matters is what the value decides: readyAt is cleared to the zero
// time, and the zero time is precisely what the walk reads as "no window, probe
// now". So a torn read is not merely a -race warning, it is a decision made on a
// half-built value.
//
// Both sides run many rounds with a yield between them. The race needs the two
// accesses to interleave on the same field, and a single walk against a single
// retry usually finishes either side before the other starts — the shape that does
// reproduce is a loop of each, which is what the stress run did too. The iteration
// count is what makes this fail without the fix rather than pass by luck.
func TestWalkReadsTheBackoffWindowUnderTheLock(t *testing.T) {
	r := startQuietReconciler(t)
	ext := Extension{}
	ext.Name = "backoff"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	mustSave(t, r.store, &ext)

	// Spend the budget first: the spent-budget branch is the only one that reads
	// readyAt at all, so the race is unreachable while the budget is whole.
	st := r.state(ext.Name)
	st.exhaustRetryBudget()
	for i := 0; i <= maxAutoRetries; i++ {
		st.recordFailure("target", "boom")
	}

	const rounds = 5000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = r.Retry(ext.Name)
			runtime.Gosched()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			r.Sync(t.Context(), "raced")
			runtime.Gosched()
		}
	}()
	wg.Wait()
}

// startQuietReconciler builds the same reconciler as startReconciler but never

// startQuietReconciler builds the same reconciler as startReconciler but never
// starts its loop, so a test can drive one walk and inspect exactly what that
// walk did. The loop's own periodic tick would otherwise repair state between
// the walk and the assertion.
func startQuietReconciler(t *testing.T) *Reconciler {
	t.Helper()
	srv := sshserver.New()
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
	return NewReconciler(NewStore(dir), sessMgr, msgMgr, sshconfig.NewStore(dir), forward.NewForwardManager())
}

// TestNameStateTellsStaleFromTaken pins the three answers a create depends on.
//
// "Taken" and "loadable" are separate questions on purpose. A stale template must
// stay repairable, and the two paths that could repair one are keyed on its name:
// create (which overwrites it) and update (which loads, then patches). Refusing
// both would leave a file only someone with shell access to the data directory
// could fix — which is where the first version of this check landed.
func TestNameStateTellsStaleFromTaken(t *testing.T) {
	s := newTestStore(t)

	if taken, loadable := s.NameState("free"); taken || loadable {
		t.Errorf("an unused name should be free, got taken=%v loadable=%v", taken, loadable)
	}

	// A name that cannot be stored at all is NOT "taken": answering "already
	// exists" sends the caller looking for a template to delete instead of fixing
	// their name, and the save reports the real reason.
	for _, bad := range []string{"a/b", "has space", ""} {
		if taken, _ := s.NameState(bad); taken {
			t.Errorf("an invalid name %q must not read as taken", bad)
		}
	}

	good := Extension{Mode: "pty"}
	good.Name = "live"
	good.Target = "internal"
	if err := s.Save(&good); err != nil {
		t.Fatal(err)
	}
	if taken, loadable := s.NameState("live"); !taken || !loadable {
		t.Errorf("a working template should be taken and loadable, got taken=%v loadable=%v", taken, loadable)
	}

	// The stale case: a file that exists but no longer validates. Written by hand
	// because Save would refuse it.
	stale := Extension{}
	stale.Name = "stale"
	stale.Target = "internal"
	stale.Paths = nil // an empty window declares no capability, so Validate refuses it
	if err := s.Save(&stale); err == nil {
		t.Fatal("a template with no capability should not have saved")
	}
	writeStaleTemplate(t, s, "stale")
	if taken, loadable := s.NameState("stale"); !taken || loadable {
		t.Errorf("a broken file should be taken but not loadable, got taken=%v loadable=%v", taken, loadable)
	}
}

// writeStaleTemplate puts a config.toml on disk that will not pass Validate, the
// way a hand-edit or an older build can leave one.
func writeStaleTemplate(t *testing.T, s *Store, name string) {
	t.Helper()
	dir := filepath.Join(s.root(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "kind = \"skill\"\ntarget = \"internal\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
