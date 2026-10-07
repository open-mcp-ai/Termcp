package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// TestResourceURLStartSessionEndToEnd: "termcp://internal" as ssh_config in
// session_start must behave exactly like "internal".
func TestResourceURLStartSessionEndToEnd(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "termcp://internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	if m["ssh_config"] != "internal" {
		t.Fatalf("ssh_config = %v, want internal", m["ssh_config"])
	}
	if sid := m["session_id"].(string); sid == "" {
		t.Fatal("session_id empty")
	}
	// Cleanup.
	_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": m["session_id"],
		"force":      true,
	}))
}

// TestShellLocatorIndexStableAcrossClose is the regression for issue #73, driven
// through the tool surface the user actually hits.
//
// The Web UI's copy button emits termcp://#<sid>:N, and N used to be a *position*
// in the session's current shell list — recomputed per lookup. Closing an earlier
// channel then shifted every survivor down one, so the locator the user had copied
// as :2 started naming what had been :3. The channel index is now assigned at
// creation and never reused, and MCP resolves it through the same helper the REST
// resolver and the Web UI use.
func TestShellLocatorIndexStableAcrossClose(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	sid := m["session_id"].(string)
	primary := m["shell_id"].(string)

	// Two more channels, so :2 and :3 exist and the middle one can be watched.
	second := startSubShell(t, s, sid)
	third := startSubShell(t, s, sid)

	// :2 and :3 name the channels they were created as.
	if cs, bad := s.requireShell("termcp://#" + sid + ":2"); bad != nil || cs.ID != second {
		t.Fatalf(":2 = %v (bad=%v), want %q", cs, bad, second)
	}
	if cs, bad := s.requireShell("termcp://#" + sid + ":3"); bad != nil || cs.ID != third {
		t.Fatalf(":3 = %v (bad=%v), want %q", cs, bad, third)
	}

	// Close the MIDDLE channel. A positional lookup would now answer :3 with
	// whatever slid into third place, which is the bug from the issue report.
	// (The primary channel is not used here because closing the internal primary
	// shell is deliberately a no-op — see handleCloseShell — so it would never
	// actually leave the list.)
	closeShell(t, s, second)

	cs, bad := s.requireShell("termcp://#" + sid + ":3")
	if bad != nil {
		t.Fatalf(":3 after closing the middle channel failed: %s", bad.Content[0].(mcpgo.TextContent).Text)
	}
	if cs.ID != third {
		t.Fatalf(":3 after the close resolved to %q, want the third channel %q", cs.ID, third)
	}
	if cs, bad := s.requireShell("termcp://#" + sid + ":1"); bad != nil || cs.ID != primary {
		t.Fatalf(":1 = %v (bad=%v), want the untouched primary %q", cs, bad, primary)
	}

	// The closed channel's number is retired, not handed to a survivor: a locator
	// for it must fail cleanly instead of addressing a different shell.
	_, bad = s.requireShell("termcp://#" + sid + ":2")
	if bad == nil {
		t.Fatal(":2 must not resolve after its channel was closed")
	}
	if code, _ := decodeToolError(t, bad); code != CodeShellNotFound {
		t.Fatalf(":2 error_code = %q, want %q", code, CodeShellNotFound)
	}

	// shell_output resolves through the same numbering, so a copied :3 must read the
	// same channel shell_input would write.
	src, bad := s.resolveOutputSource("termcp://#" + sid + ":3")
	if bad != nil {
		t.Fatalf("shell_output :3 failed: %s", bad.Content[0].(mcpgo.TextContent).Text)
	}
	if src.shellID != third {
		t.Fatalf("shell_output :3 read shell %q, want %q", src.shellID, third)
	}

	_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid,
		"force":      true,
	}))
}

// TestShellLocatorResolvesAgainstLiveSetNotSnapshot covers the branch that made
// issue #73 worse than a plain renumbering. Live-vs-snapshot used to be decided
// from the *primary* shell: with the first channel closed, a session that was
// still running was treated as DEAD, so :N was answered from the persisted
// snapshot — a different source (and a different set) than the one the Web UI
// was showing. A session with any live channel must keep answering from the live
// map.
//
// The primary channel is closed on the session object rather than through
// shell_close, because the tool deliberately no-ops for the internal endpoint
// (the process outlives the tab). The state under test — live channels, no
// primary — is reachable at the session layer and over a remote shell-close
// path, which is what the resolver must handle.
func TestShellLocatorResolvesAgainstLiveSetNotSnapshot(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	sid := m["session_id"].(string)
	primary := m["shell_id"].(string)

	second := startSubShell(t, s, sid)
	third := startSubShell(t, s, sid)

	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session vanished")
	}
	if err := sess.CloseChildShell(primary); err != nil {
		t.Fatalf("close primary channel: %v", err)
	}

	// Preconditions for the branch under test: the session is still live and has
	// live channels, but its primary shell is gone — so the old check
	// (PrimaryShell() != nil) sent it down the DEAD/snapshot path.
	if !sess.HasLiveShells() {
		t.Fatal("session unexpectedly has no live shells")
	}
	if sess.PrimaryShell() != nil {
		t.Fatal("primary shell still present; the test no longer exercises the branch")
	}

	// :2 is the channel the user copied. It must resolve, to the right shell, and
	// from the live stream rather than the persisted log of a running session.
	src, bad := s.resolveOutputSource("termcp://#" + sid + ":2")
	if bad != nil {
		t.Fatalf("shell_output :2 failed: %s", bad.Content[0].(mcpgo.TextContent).Text)
	}
	if src.live == nil {
		t.Fatalf(":2 resolved through the persisted snapshot for a session with live shells (shellID=%s)", src.shellID)
	}
	if src.shellID != second {
		t.Fatalf(":2 read shell %q, want the live second channel %q", src.shellID, second)
	}

	// The survivors keep the numbers they were created with, including the one
	// that was never the primary.
	if cs, bad := s.requireShell("termcp://#" + sid + ":3"); bad != nil || cs.ID != third {
		t.Fatalf(":3 = %v (bad=%v), want the live third channel %q", cs, bad, third)
	}

	// The closed channel's number is retired, not reused by a survivor.
	if _, bad := s.requireShell("termcp://#" + sid + ":1"); bad == nil {
		t.Fatal(":1 must not resolve after the primary channel was closed")
	}

	_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid,
		"force":      true,
	}))
}

// startSubShell opens one channel and returns its shell_id, failing the test if
// the tool refuses.
func startSubShell(t *testing.T, s *Server, sid string) string {
	t.Helper()
	res, err := s.handleStartSubShell(context.Background(), makeRequest(map[string]any{"session_id": sid}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("shell_open failed: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	id, _ := parseResult(t, res)["shell_id"].(string)
	if id == "" {
		t.Fatalf("shell_open returned no shell_id: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	return id
}

// closeShell closes one channel and fails the test if the tool refuses.
func closeShell(t *testing.T, s *Server, shellID string) {
	t.Helper()
	res, err := s.handleCloseShell(context.Background(), makeRequest(map[string]any{"shell_id": shellID}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("shell_close %q failed: %s", shellID, res.Content[0].(mcpgo.TextContent).Text)
	}
}

// TestRequireShellResourceURL: shell_input / shell_output must accept the
// web-UI-copied locators: termcp://#<sid> (→ primary) and termcp://#<sid>:1.
func TestRequireShellResourceURL(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	sid := m["session_id"].(string)
	shellID := m["shell_id"].(string)

	// Raw shell id still resolves.
	if cs, bad := s.requireShell(shellID); bad != nil || cs == nil {
		t.Fatalf("raw shell id failed: %v", bad)
	}

	// Short session locator → primary shell.
	cs, bad := s.requireShell("termcp://#" + sid)
	if bad != nil {
		t.Fatalf("session locator failed: %v", bad)
	}
	if cs.ID != shellID {
		t.Fatalf("session locator resolved to shell %q, want %q", cs.ID, shellID)
	}

	// Indexed shell locator :1 → primary too.
	cs, bad = s.requireShell("termcp://#" + sid + ":1")
	if bad != nil {
		t.Fatalf("shell :1 locator failed: %v", bad)
	}
	if cs.ID != shellID {
		t.Fatalf("shell :1 locator resolved to shell %q, want %q", cs.ID, shellID)
	}

	// session_terminate accepts a session locator.
	termRes, _ := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": "termcp://#" + sid,
		"force":      true,
	}))
	if termRes.IsError {
		t.Fatalf("terminate by locator failed: %s", parseResult(t, termRes)["error"])
	}
}

// TestRequireShellResourceURLIndexOutOfRange: :99 on a 1-shell session must be
// a clean shell_not_found error.
func TestRequireShellResourceURLIndexOutOfRange(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	sid := m["session_id"].(string)

	_, bad := s.requireShell("termcp://#" + sid + ":99")
	if bad == nil {
		t.Fatal("expected error for out-of-range shell index")
	}
	code, msg := decodeToolError(t, bad)
	if code != CodeShellNotFound {
		t.Fatalf("error_code = %q, want %q (msg: %s)", code, CodeShellNotFound, msg)
	}
	if !strings.Contains(msg, "out of range") {
		t.Fatalf("error does not contain 'out of range': %s", msg)
	}

	_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid,
		"force":      true,
	}))
}

// TestRequireShellArchivedLocator: after the session is archived, locator and
// raw-id lookups must surface the archived hint instead of a generic
// "shell not found" (regression: the hint used to be swallowed by err == nil).
func TestRequireShellArchivedLocator(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	sid := m["session_id"].(string)

	if _, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid,
		"force":      true,
	})); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{
		"termcp://#" + sid,        // short session locator
		"termcp://#" + sid + ":1", // shell locator
		sid,                       // raw archived session id
	} {
		_, bad := s.requireShell(id)
		if bad == nil {
			t.Fatalf("%s: expected an error for a closed session", id)
		}
		code, got := decodeToolError(t, bad)
		if code != CodeSessionNotFound {
			t.Errorf("%s: error_code = %q, want %q", id, code, CodeSessionNotFound)
		}
		if !strings.Contains(got, "closed") || !strings.Contains(got, "shell_output") {
			t.Errorf("%s: error should mention closed + shell_output, got %q", id, got)
		}
	}
}

// TestRequireShellMalformedLocator: parser diagnostics must reach the caller.
func TestRequireShellMalformedLocator(t *testing.T) {
	s := newTestServer(t)
	_, bad := s.requireShell("termcp://#abc:0")
	if bad == nil {
		t.Fatal("expected error for malformed locator")
	}
	code, got := decodeToolError(t, bad)
	if code != CodeInvalidArgument {
		t.Fatalf("error_code = %q, want %q", code, CodeInvalidArgument)
	}
	if !strings.Contains(got, "invalid shell index") {
		t.Fatalf("error should contain parse diagnosis, got %q", got)
	}
}

// TestRequireShellEntryLocator: an entry URL is not a shell locator.
func TestRequireShellEntryLocator(t *testing.T) {
	s := newTestServer(t)
	_, bad := s.requireShell("termcp://not-a-session")
	if bad == nil {
		t.Fatal("expected error for entry locator")
	}
	code, got := decodeToolError(t, bad)
	if code != CodeInvalidArgument {
		t.Fatalf("error_code = %q, want %q", code, CodeInvalidArgument)
	}
	if !strings.Contains(got, "entry") {
		t.Fatalf("error should mention entry, got %q", got)
	}
}

// TestRequireShellGarbageAndBroadcastURI locks the fallback chain endpoints:
// a non-locator garbage string stays a plain shell_not_found (no parser
// diagnostics), and the notification broadcast URI gets its own diagnosis.
func TestRequireShellGarbageAndBroadcastURI(t *testing.T) {
	s := newTestServer(t)

	_, bad := s.requireShell("definitely-not-real")
	code, msg := decodeToolError(t, bad)
	if code != CodeShellNotFound {
		t.Fatalf("garbage error_code = %q, want %q", code, CodeShellNotFound)
	}
	if !strings.Contains(msg, "not found") || strings.Contains(msg, "locator") || strings.Contains(msg, "entry") {
		t.Fatalf("garbage should get the plain shell-not-found message, got %q", msg)
	}

	_, bad = s.requireShell("termcp://shells/xyz")
	code, msg = decodeToolError(t, bad)
	if code != CodeInvalidArgument {
		t.Fatalf("broadcast URI error_code = %q, want %q", code, CodeInvalidArgument)
	}
	if !strings.Contains(msg, "notification broadcast URI") {
		t.Fatalf("broadcast URI diagnosis missing, got %q", msg)
	}
}

// TestStartSessionSSHConfigLocatorErrors: an ssh_config value written in
// termcp:// syntax must be diagnosed precisely — malformed syntax and
// session/shell locators are invalid_argument, not "profile not found".
func TestStartSessionSSHConfigLocatorErrors(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name    string
		cfg     string
		wantMsg string
	}{
		{"malformed locator", "termcp://#abc:0", "invalid shell index"},
		{"session locator", "termcp://#abcdef123456", "session/shell locator"},
		{"shell locator", "termcp://#abcdef123456:2", "session/shell locator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
				"ssh_config": tc.cfg,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Fatalf("expected error for ssh_config %q", tc.cfg)
			}
			code, msg := decodeToolError(t, res)
			if code != CodeInvalidArgument {
				t.Fatalf("error_code = %q, want %q (msg: %s)", code, CodeInvalidArgument, msg)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("error message %q should contain %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestShellOutputAcceptsLocator: shell_output (resolveOutputSource) must accept
// the same locators as the other shell_id tools — session form and :N channel
// form — for both live and archived sessions.
func TestShellOutputAcceptsLocator(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, res)
	sid := m["session_id"].(string)
	shellID := m["shell_id"].(string)

	for _, id := range []string{"termcp://#" + sid, "termcp://#" + sid + ":1"} {
		src, bad := s.resolveOutputSource(id)
		if bad != nil {
			t.Fatalf("%s: %s", id, parseResult(t, bad)["error"])
		}
		if src.shellID != shellID {
			t.Fatalf("%s: resolved shell %q, want %q", id, src.shellID, shellID)
		}
	}

	// Out-of-range channel on a live session.
	if _, bad := s.resolveOutputSource("termcp://#" + sid + ":9"); bad == nil {
		t.Fatal("expected error for out-of-range channel locator")
	} else if code, msg := decodeToolError(t, bad); code != CodeShellNotFound || !strings.Contains(msg, "out of range") {
		t.Fatalf("out-of-range: code=%q msg=%q", code, msg)
	}

	// Entry locator and malformed locator are invalid_argument.
	if _, bad := s.resolveOutputSource("termcp://someentry"); bad == nil {
		t.Fatal("expected error for entry locator")
	} else if code, _ := decodeToolError(t, bad); code != CodeInvalidArgument {
		t.Fatalf("entry locator code = %q, want %q", code, CodeInvalidArgument)
	}
	if _, bad := s.resolveOutputSource("termcp://#abc:0"); bad == nil {
		t.Fatal("expected error for malformed locator")
	} else if code, _ := decodeToolError(t, bad); code != CodeInvalidArgument {
		t.Fatalf("malformed locator code = %q, want %q", code, CodeInvalidArgument)
	}

	// Closed (DEAD) session: session form and channel form both read the
	// persisted byte log, since the in-memory buffer is gone.
	if _, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid,
		"force":      true,
	})); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"termcp://#" + sid, "termcp://#" + sid + ":1"} {
		src, bad := s.resolveOutputSource(id)
		if bad != nil {
			t.Fatalf("closed %s: %s", id, parseResult(t, bad)["error"])
		}
		// Terminated sessions retain their live in-memory buffer until explicit
		// delete; output reading continues through the buffer without degradation.
		if src.live == nil && src.msgMgr == nil {
			t.Fatalf("closed %s: expected a valid output source", id)
		}
	}
	if _, bad := s.resolveOutputSource("termcp://#" + sid + ":9"); bad == nil {
		t.Fatal("expected error for out-of-range channel on closed session")
	} else if code, msg := decodeToolError(t, bad); code != CodeShellNotFound || !strings.Contains(msg, "out of range") {
		t.Fatalf("closed out-of-range: code=%q msg=%q", code, msg)
	}
}
