package mcp

import (
	"context"
	"strings"
	"testing"
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
		_ , bad := s.requireShell(id)
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
