package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestRmArchiveBatch(t *testing.T) {
	s := newTestServer(t)
	newSession := func(name string) string {
		handle := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --name " + name}))
		id := strings.Split(handle, ":")[0]
		t.Cleanup(func() { _ = s.sessMgr.Delete(id) })
		return id
	}
	first, live, last := newSession("first"), newSession("live"), newSession("last")
	for _, id := range []string{first, last} {
		termText(t, s, map[string]any{"cmd": "close " + id})
	}
	ids := []string{first, live, "missing", last}
	line := "rm " + strings.Join(ids, ",")
	for _, flag := range []string{" --force", " --dry-run", ""} {
		reply := s.Term(t.Context(), line+flag, nil)
		if flag == " --force" {
			if len(reply.Slots) != 1 || reply.Slots[0].Code != 400 {
				t.Fatalf("batch force must be refused: %+v", reply.Slots)
			}
		} else {
			success := 204
			if flag != "" {
				success = 200
			}
			if len(reply.Slots) != len(ids) {
				t.Fatalf("want one slot per requested session, got %+v", reply.Slots)
			}
			if result := reply.Slots[0].Headers["Batch-Result"]; result != "ok=2 failed=2 held=0" {
				t.Errorf("unexpected batch summary %q", result)
			}
			for i, code := range []int{success, 409, 404, success} {
				slot := reply.Slots[i]
				if slot.Code != code || slot.Headers["Path"] != "rm "+ids[i]+flag {
					t.Errorf("entry %s: got %+v, want %d", ids[i], slot, code)
				}
			}
		}
		for _, id := range []string{first, last} {
			if exists := s.sessMgr.Get(id) != nil; exists != (flag != "") {
				t.Fatalf("flag %q: archive %s exists=%t", flag, id, exists)
			}
		}
		if sess := s.sessMgr.Get(live); sess == nil || sess.Info().Status != api.SessionRunning {
			t.Fatalf("flag %q: running session changed", flag)
		}
	}
}

// TestRmRunningSessionNeedsForce pins the guard against accidental erasure.
//
// `close` and `rm` look interchangeable and are not: close stops a session and keeps
// its transcript readable, while rm erases the record permanently. Accepting a
// running session without comment answered 204 for an action that wiped work in
// flight.
func TestRmRunningSessionNeedsForce(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "connect internal --name live"})
	h := handleFrom(t, text)
	sid := strings.Split(h, ":")[0]

	// 1. Without --force on a running session: 409 Conflict.
	got := termText(t, s, map[string]any{"cmd": "rm " + sid})
	if !strings.HasPrefix(got, "TERM/1 409") {
		t.Fatalf("expected 409 on a running session, got:\n%s", got)
	}
	for _, want := range []string{"is running", "--force", "close " + sid} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, got)
		}
	}

	// 2. The session survived the refusal.
	got = termText(t, s, map[string]any{"cmd": "get " + sid})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("the session should still exist after refusal:\n%s", got)
	}

	// 3. With --force: erased immediately, 204.
	got = termText(t, s, map[string]any{"cmd": "rm " + sid + " --force"})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("rm --force should succeed, got:\n%s", got)
	}

	// 4. Gone.
	got = termText(t, s, map[string]any{"cmd": "get " + sid})
	if !strings.HasPrefix(got, "TERM/1 404") {
		t.Fatalf("the session should be gone after rm --force:\n%s", got)
	}
}

// TestRmStoppedSessionDoesNotNeedForce pins that a stopped session has nothing left
// to lose, so ceremony is not added where no danger exists.
func TestRmStoppedSessionDoesNotNeedForce(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "connect internal --name dead"})
	h := handleFrom(t, text)
	sid := strings.Split(h, ":")[0]

	// Stop it first.
	termText(t, s, map[string]any{"cmd": "close " + sid})

	// rm without --force: succeeds because it is already stopped.
	got := termText(t, s, map[string]any{"cmd": "rm " + sid})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("rm on a stopped session should succeed without --force, got:\n%s", got)
	}
}

// TestRmRefusesAChannelHandle pins the TargetKind declaration.
//
// `rm <session>:1` used to resolve the locator, find the session, and erase the whole
// container. A caller who named a channel asked about that channel, and erasing
// everything is a bigger answer than the question.
func TestRmRefusesAChannelHandle(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "rm a1f3:1"})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("expected 400 on a channel handle, got:\n%s", got)
	}
	for _, want := range []string{"needs a SESSION", "not the channel", "close a1f3:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, got)
		}
	}
}
