package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestUseHostContainerIsPerHostSingleton pins issue #255's central claim: N runs on
// one host produce ONE visible session, and each run is recorded inside it.
func TestUseHostContainerIsPerHostSingleton(t *testing.T) {
	s := newTestServer(t)

	for _, cmd := range []string{"echo alpha", "echo beta"} {
		res := s.runWithArgs("run use internal $1", cmd)
		if len(res.slots) != 1 || res.slots[0].Code != 200 {
			t.Fatalf("run %q failed: %+v", cmd, res.slots)
		}
	}

	containers := useContainers(t, s)
	if len(containers) != 1 {
		t.Fatalf("two runs on one host must share one container, got %d", len(containers))
	}
	if containers[0].Name != "use:internal" {
		t.Errorf("container name = %q, want use:internal", containers[0].Name)
	}
	if !containers[0].UseHost {
		t.Error("container must carry UseHost=true so connect/open skip it")
	}

	// Both commands are visible as recorded channels, in the order they ran.
	sess := s.sessMgr.Get(containers[0].ID)
	if sess == nil {
		t.Fatal("container is not addressable by id")
	}
	shells := sess.ShellsForView()
	if len(shells) != 2 {
		t.Fatalf("want 2 recorded commands in the container, got %d: %+v", len(shells), shells)
	}
	for i, want := range []string{"echo alpha", "echo beta"} {
		if shells[i].Command != want {
			t.Errorf("recorded command %d = %q, want %q", i, shells[i].Command, want)
		}
		if shells[i].Status != api.SessionExited {
			t.Errorf("recorded command %d status = %q, want exited", i, shells[i].Status)
		}
	}
}

// TestCloseLaterOnUseHostContainerFulfilsItself pins that a deferred close on a
// container with nothing in flight ends it immediately rather than leaving it up
// forever: a use container holds no live shell, so nothing else would ever fire.
func TestCloseLaterOnUseHostContainerFulfilsItself(t *testing.T) {
	s := newTestServer(t)

	s.runWithArgs("run use internal $1", "echo one")
	containers := useContainers(t, s)
	if len(containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(containers))
	}
	id := containers[0].ID

	res := termText(t, s, map[string]any{"cmd": "close " + id + " --later"})
	if !statusLineIs(res, "200") {
		t.Fatalf("close --later must answer 200:\n%s", res)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := s.sessMgr.Get(id); c == nil || c.Info().Status != api.SessionRunning {
			return // fulfilled
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a deferred close on an idle use container never ended the session")
}

// TestARunWhileClosingIsRefused pins that --later actually stops new work: a command
// dispatched after the request must be refused, not silently run into a window that is
// on its way out.
func TestARunWhileClosingIsRefused(t *testing.T) {
	s := newTestServer(t)

	// A long-running command holds the container open so the close cannot fulfil.
	go s.runWithArgs("run use internal $1", "sleep 5")
	time.Sleep(500 * time.Millisecond)

	containers := useContainers(t, s)
	if len(containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(containers))
	}
	id := containers[0].ID

	if res := termText(t, s, map[string]any{"cmd": "close " + id + " --later"}); !statusLineIs(res, "200") {
		t.Fatalf("close --later must answer 200:\n%s", res)
	}

	res := s.runWithArgs("run use internal $1", "echo should-not-run")
	if len(res.slots) != 1 {
		t.Fatalf("want 1 slot, got %d", len(res.slots))
	}
	if res.slots[0].Code != 409 {
		t.Fatalf("a run while closing must be refused with 409, got %d: %s", res.slots[0].Code, res.slots[0].Body)
	}
	if !strings.Contains(res.slots[0].Body, "closing") {
		t.Errorf("the refusal must say the container is closing: %s", res.slots[0].Body)
	}
}

// useContainers lists the live `use:*` containers.
func useContainers(t *testing.T, s *Server) []api.Session {
	t.Helper()
	var out []api.Session
	for _, sess := range s.sessMgr.ListAll() {
		if sess.UseHost && sess.Status == api.SessionRunning {
			out = append(out, sess)
		}
	}
	return out
}
