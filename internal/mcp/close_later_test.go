package mcp

import (
	"strings"
	"testing"
)

// TestCloseLaterOnPipeSessionSaysClosing pins the deferred-close contract at the MCP
// surface: a pipe session is marked closing and answered 200, not terminated (#255).
func TestCloseLaterOnPipeSessionSaysClosing(t *testing.T) {
	s := newTestServer(t)

	// A pipe session whose command stays alive long enough to be deferred.
	h := openPipe(t, s, "sleep 5")
	sessID := strings.Split(h, ":")[0]

	res := termText(t, s, map[string]any{"cmd": "close " + sessID + " --later"})
	if !statusLineIs(res, "200") {
		t.Fatalf("close --later on a pipe session must answer 200:\n%s", res)
	}
	if !strings.Contains(res, "closing") {
		t.Errorf("close --later must report the closing state:\n%s", res)
	}
	// The session must still exist: --later defers, it does not terminate.
	get := termText(t, s, map[string]any{"cmd": "get " + sessID})
	if !strings.Contains(get, sessID) {
		t.Errorf("a deferred session must still be addressable:\n%s", get)
	}
	if strings.Contains(get, "status: exited") {
		t.Errorf("close --later must not terminate the session:\n%s", get)
	}
}

// TestCloseLaterRefusesPTYSession pins that an interactive pty session is refused,
// because a pty never ends by itself (#255).
func TestCloseLaterRefusesPTYSession(t *testing.T) {
	s := newTestServer(t)

	h := openPty(t, s)
	sessID := strings.Split(h, ":")[0]

	res := termText(t, s, map[string]any{"cmd": "close " + sessID + " --later"})
	if !statusLineIs(res, "400") {
		t.Fatalf("close --later on a pty session must be refused with 400:\n%s", res)
	}
	if !strings.Contains(res, "pty") {
		t.Errorf("the refusal must name the pty as the reason:\n%s", res)
	}
	// Refused means untouched: the session keeps running.
	get := termText(t, s, map[string]any{"cmd": "get " + sessID})
	if !strings.Contains(get, "status: running") {
		t.Errorf("a refused --later must leave the session running:\n%s", get)
	}
}

// TestCloseLaterOnAChannelIsRefused pins that --later addresses a session, not a
// channel, and says which spelling to use instead (#255).
func TestCloseLaterOnAChannelIsRefused(t *testing.T) {
	s := newTestServer(t)

	h := openPty(t, s)

	res := termText(t, s, map[string]any{"cmd": "close " + h + " --later"})
	if !statusLineIs(res, "400") {
		t.Fatalf("close --later on a channel must be refused with 400:\n%s", res)
	}
	if !strings.Contains(res, "channel") {
		t.Errorf("the refusal must explain that a channel is not a session:\n%s", res)
	}
}
