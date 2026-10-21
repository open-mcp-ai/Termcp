package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// TestADoorTheTemplateDoesNotGrantIsRefused is the security half: a session
// limited to reading files must not be able to open a peer the process did not
// start, because that is reach the template never gave it.
func TestADoorTheTemplateDoesNotGrantIsRefused(t *testing.T) {
	s := newTestServer(t)
	guardTestSession(t, s, []string{"file.read"}, nil)

	for _, door := range []string{"mcp", "jrpc", "cdp"} {
		res := termCall(t, s, map[string]any{"cmd": door + " open http://127.0.0.1:1"})
		body := toolResultText(res)
		if !res.IsError || !strings.Contains(body, "does not grant "+door+".open") {
			t.Errorf("%s open was not refused by a session that does not grant it:\n%s", door, body)
		}
	}
}

// TestADoorTheTemplateGrantsIsAdmitted is the other half: the row is a real
// permission, not a wall. A template that names the door lets the call through.
//
// It asserts on the HANDLE, which only an admitted open produces. Asserting on a
// transport error would be wrong: an HTTP channel connects lazily (the first
// frame is what dials), so a successful `mcp open` against a dead port is the
// correct outcome and says nothing about the gate.
func TestADoorTheTemplateGrantsIsAdmitted(t *testing.T) {
	s := newTestServer(t)
	guardTestSession(t, s, []string{"mcp.open"}, nil)

	out := termText(t, s, map[string]any{"cmd": "mcp open http://127.0.0.1:1"})
	if strings.Contains(out, "does not grant") {
		t.Fatalf("a session that grants mcp.open was refused:\n%s", out)
	}
	h := handleFrom(t, out)
	termText(t, s, map[string]any{"cmd": "mcp close " + h})
}

// TestAnUnrestrictedInstanceIsUnaffected pins that the check is a whitelist and
// nothing else: the ordinary case (no restricted session at all) must behave
// exactly as it did before the rows existed.
func TestAnUnrestrictedInstanceIsUnaffected(t *testing.T) {
	s := newTestServer(t)
	srv := devtoolsPeer(t)
	out := termText(t, s, map[string]any{"cmd": "cdp open " + srv.URL})
	h := handleFrom(t, out)
	termText(t, s, map[string]any{"cmd": "cdp close " + h})
}

// TestTheDoorsAreNameableCapabilities pins that a template CAN say the words.
// Before the rows existed, `--allow mcp.open` did not parse as a capability and
// the door was unreachable by a whitelist in both directions.
func TestTheDoorsAreNameableCapabilities(t *testing.T) {
	for _, id := range []string{"jrpc.open", "mcp.open", "cdp.open"} {
		if _, ok := session.NewRestriction([]string{id}, nil); ok != nil {
			t.Errorf("%s is not a capability a template can name", id)
		}
	}
}
