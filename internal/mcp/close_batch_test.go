package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestCloseBatch(t *testing.T) {
	s := newTestServer(t)
	connect := func(name string) string {
		t.Helper()
		h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --name " + name}))
		id, _, _ := strings.Cut(h, ":")
		t.Cleanup(func() { _ = s.sessMgr.Delete(id) })
		return id
	}
	keep, stop := connect("keep"), connect("stop")
	child := handleFrom(t, termText(t, s, map[string]any{"cmd": "open " + keep + " --name worker"}))
	for _, bad := range []string{stop + "," + keep + ":oops", stop + ",", stop + "," + keep + " --channel"} {
		r := s.Term(t.Context(), "close "+bad, nil)
		if len(r.Slots) != 1 || r.Slots[0].Code != 400 || s.sessMgr.Get(stop).Info().Status != api.SessionRunning {
			t.Fatalf("ill-formed batch %q must not act: %+v", bad, r.Slots)
		}
	}
	targets := []string{child, "missing", stop}
	r := s.Term(t.Context(), "close "+strings.Join(targets, ","), nil)
	if len(r.Slots) != len(targets) {
		t.Fatalf("want one slot per target, got %+v", r.Slots)
	}
	for i, code := range []int{204, 404, 204} {
		if slot := r.Slots[i]; slot.Code != code || slot.Headers["Path"] != "close "+targets[i] {
			t.Errorf("target %s: got %+v", targets[i], slot)
		}
	}
	if s.sessMgr.Get(keep).Info().Status != api.SessionRunning || s.sessMgr.Get(stop).Info().Status != api.SessionExited {
		t.Fatal("only the explicitly named whole session should have stopped; both records must survive")
	}
	if sh, err := s.requireShell(child); err == nil && !sh.IsBufferClosed() {
		t.Fatal("named child channel is still open")
	}
}
