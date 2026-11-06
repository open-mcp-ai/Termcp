package mcp

import (
	"strings"
	"testing"
)

func TestANamedSessionIsAddressable(t *testing.T) {
	s := newTestServer(t)

	// A caller names one session at open time.
	open := termText(t, s, map[string]any{"cmd": `connect internal --name dogfood --argv sleep 60`})
	h := handleFrom(t, open) // id:1
	if h == "" {
		t.Fatalf("no handle: %s", open)
	}
	id := h[:strings.Index(h, ":")]

	// The same channel is reachable by name: `dogfood:1`.
	out := termText(t, s, map[string]any{"cmd": "read dogfood:1 --tail 1"})
	if strings.Contains(out, "not found") || strings.Contains(out, "Error-Code") {
		t.Errorf("read dogfood:1 refused the named session:\n%s", out)
	}

	// The session itself is reachable bare (`get dogfood`), and the id still works.
	gotID := termText(t, s, map[string]any{"cmd": "get " + id})
	if strings.Contains(gotID, "Error-Code") {
		t.Errorf("the session id stopped resolving:\n%s", gotID)
	}

	// Closing by name closes that session, not the id.
	if closed := termText(t, s, map[string]any{"cmd": "close dogfood"}); strings.Contains(closed, "Error-Code") {
		t.Errorf("close dogfood failed:\n%s", closed)
	}
}

func TestAnAmbiguousNameIsRefusedAndListsCandidates(t *testing.T) {
	s := newTestServer(t)

	a := handleFrom(t, termText(t, s, map[string]any{"cmd": `connect internal --name twin --argv sleep 60`}))
	b := handleFrom(t, termText(t, s, map[string]any{"cmd": `connect internal --name twin --argv sleep 60`}))
	if a == "" || b == "" {
		t.Fatalf("both opens must mint handles: %s / %s", a, b)
	}
	defer termText(t, s, map[string]any{"cmd": "close " + strings.Split(a, ":")[0]})
	defer termText(t, s, map[string]any{"cmd": "close " + strings.Split(b, ":")[0]})

	out := termText(t, s, map[string]any{"cmd": "get twin"})
	if !strings.Contains(out, "2 sessions are named \"twin\"") {
		t.Errorf("an ambiguous name was not refused with the candidate count:\n%s", out)
	}
	if strings.Contains(out, "Error-Code: session_not_found") {
		t.Errorf("an ambiguous name is not a missing session:\n%s", out)
	}
}
