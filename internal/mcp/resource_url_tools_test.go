package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/forward"
)

// Locator acceptance across the whole tool surface.
//
// The docs promise that "MCP tools accept locators anywhere an id or profile
// name is expected", but each handler used to resolve ids itself, so the promise
// held only where someone had remembered to wire it up. The failures were not
// uniform: some tools rejected a locator outright, and some — the worse case —
// accepted it and then used the locator TEXT as if it were an id. A forward
// registered under a locator string was never matched by the DEAD cascade
// (its listener outlived the session), and a notification rule likewise escaped
// cleanup and broadcast a URI naming something that is not a shell.
//
// These tests pin the two halves of the contract separately:
//   - accepted: the tool must not reject a locator;
//   - canonical: the resource must end up keyed by the real id, not the argument.

// locatorFixture starts one internal session with three channels so index 1, 2
// and 3 all exist (index 3 is spare, for tests that close index 2).
func locatorFixture(t *testing.T) (s *Server, sid, primary, second, third string) {
	t.Helper()
	s = newTestServer(t)
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
	sid = m["session_id"].(string)
	primary = m["shell_id"].(string)
	second = startSubShell(t, s, sid)
	third = startSubShell(t, s, sid)
	return s, sid, primary, second, third
}

// sessionLocators returns the spellings that must all name the same session. The
// '#' forms are the locators; the bare id is what the server itself hands out.
// "session-<id>" is deliberately absent: at the top level that spelling is an
// entry (profile) name, since profile names share the namespace.
func sessionLocators(sid string) []string {
	return []string{sid, "termcp://#" + sid, "#" + sid, "termcp://#session-" + sid}
}

func shellLocators(sid, shellID string, index int) []string {
	return []string{
		shellID,
		"termcp://#" + sid + ":" + strconv.Itoa(index),
	}
}

// TestSessionLocatorsResolveToTheSameSession pins requireSession's contract: every
// accepted spelling names the same session, and the resolved id — not the
// spelling — is what callers must act on.
func TestSessionLocatorsResolveToTheSameSession(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)

	for _, loc := range append(sessionLocators(sid), "termcp://#"+sid+":1") {
		sess, bad := s.requireSession(loc)
		if bad != nil {
			t.Errorf("requireSession(%q) = %s", loc, bad.Content[0].(mcpgo.TextContent).Text)
			continue
		}
		if sess.ID != sid {
			t.Errorf("requireSession(%q).ID = %q, want %q", loc, sess.ID, sid)
		}
	}

	// A raw shell id also names its container, which is what lets session-level
	// tools accept a shell id without a lookup round-trip.
	for _, loc := range []string{sid, "termcp://#" + sid} {
		if _, bad := s.requireSession(loc); bad != nil {
			t.Errorf("requireSession(%q) unexpectedly failed", loc)
		}
	}
}

// TestSessionLocatorsAreRejectedWithTheRightCode keeps the failures actionable:
// a malformed locator is a caller error (invalid_argument), while an unknown one
// is a missing resource (session_not_found). Collapsing them would make a typo
// look like a deleted session.
func TestSessionLocatorsAreRejectedWithTheRightCode(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		loc  string
		want string
	}{
		{"termcp://#ghost", CodeSessionNotFound},
		{"termcp://#ghost:2", CodeSessionNotFound},
		{"termcp://#abc:0", CodeInvalidArgument},
		{"termcp://shells/xyz", CodeInvalidArgument},
		{"termcp://not-a-session", CodeInvalidArgument}, // an entry is not a session
		{"definitely-not-real", CodeSessionNotFound},
	}
	for _, tc := range cases {
		_, bad := s.requireSession(tc.loc)
		if bad == nil {
			t.Errorf("requireSession(%q) succeeded, want %s", tc.loc, tc.want)
			continue
		}
		if code, _ := decodeToolError(t, bad); code != tc.want {
			t.Errorf("requireSession(%q) error_code = %q, want %q", tc.loc, code, tc.want)
		}
	}
}

// TestSessionLifecycleToolsAcceptLocators covers terminate / delete / info, which
// form the "clean up after yourself" path an agent runs from a copied locator.
//
// Regression: terminate resolved the locator and then terminated the ARGUMENT.
// Manager.Terminate is a no-op for an unknown id, so the tool answered
// {"success":true} while the session kept running — a false success is worse
// than an error, because the caller believes the resource is gone.
func TestSessionLifecycleToolsAcceptLocators(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)

	// info: must resolve, and must be able to feed its result back in.
	res, err := s.handleGetSessionInfo(context.Background(), makeRequest(map[string]any{"session_id": "termcp://#" + sid}))
	if err != nil || res.IsError {
		t.Fatalf("session_info via locator failed: %v %v", err, res)
	}
	if info := parseResult(t, res); info["id"] != sid {
		t.Fatalf("session_info returned id %v, want %q", info["id"], sid)
	}

	// terminate: the locator must actually close the session.
	res, err = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": "termcp://#" + sid,
		"force":      true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("session_terminate via locator failed: %v %v", err, res)
	}
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session vanished")
	}
	if status := sess.Info().Status; status == "running" {
		t.Fatal("session_terminate answered success but the session is still running")
	}

	// delete: must actually erase it. Delete validates its argument as a storage
	// path component, so an unresolved locator containing '#' and ':' would be
	// refused as an invalid id rather than deleting what it names.
	res, err = s.handleDeleteSession(context.Background(), makeRequest(map[string]any{"session_id": "termcp://#" + sid}))
	if err != nil || res.IsError {
		t.Fatalf("session_delete via locator failed: %v %v", err, res)
	}
	if s.sessMgr.Get(sid) != nil {
		t.Fatal("session_delete answered success but the session is still registered")
	}
}

// TestBatchSessionLocatorsResolvePerEntry pins the comma-separated form used by
// the Web UI's bulk cleanup: entries are resolved independently, so a locator
// entry and a raw-id entry both work and a bad entry does not abort the rest.
func TestBatchSessionLocatorsResolvePerEntry(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)

	res, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": "termcp://#" + sid + ",ghost-id",
		"force":      true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Results []struct {
			SessionID string `json:"session_id"`
			OK        bool   `json:"ok"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 2 {
		t.Fatalf("batch returned %d results, want 2", len(body.Results))
	}
	if !body.Results[0].OK {
		t.Errorf("locator entry failed: %+v", body.Results[0])
	}
	if body.Results[1].OK {
		t.Errorf("unknown entry should have failed: %+v", body.Results[1])
	}
	if sess := s.sessMgr.Get(sid); sess == nil || sess.Info().Status == "running" {
		t.Error("the locator entry of a batch did not terminate its session")
	}
}

// TestShellToolsAcceptLocators walks every tool that takes a shell_id. Each must
// accept the documented spellings and act on the channel the locator names.
func TestShellToolsAcceptLocators(t *testing.T) {
	t.Run("resize", func(t *testing.T) {
		s, sid, _, second, _ := locatorFixture(t)
		for _, loc := range shellLocators(sid, second, 2) {
			res, err := s.handleResizePty(context.Background(), makeRequest(map[string]any{
				"shell_id": loc, "rows": 30, "cols": 100,
			}))
			if err != nil || res.IsError {
				t.Fatalf("shell_resize(%q) failed: %v %v", loc, err, res)
			}
		}
		// The size must have landed on the channel the locator named, not another.
		cs := s.sessMgr.GetChildShell(second)
		if cs == nil {
			t.Fatal("second channel missing")
		}
		if info := cs.Info(); info.Rows != 30 || info.Cols != 100 {
			t.Errorf("resize via locator set %dx%d on the target, want 30x100", info.Rows, info.Cols)
		}
	})

	t.Run("reader register/unregister", func(t *testing.T) {
		s, sid, _, second, _ := locatorFixture(t)
		res, err := s.handleRegisterReader(context.Background(), makeRequest(map[string]any{"shell_id": "termcp://#" + sid + ":2"}))
		if err != nil || res.IsError {
			t.Fatalf("shell_reader_register via locator failed: %v %v", err, res)
		}
		readerID, _ := parseResult(t, res)["reader_id"].(float64)
		if readerID == 0 {
			t.Fatal("no reader_id returned")
		}
		res, err = s.handleUnregisterReader(context.Background(), makeRequest(map[string]any{
			"shell_id": "termcp://#" + sid + ":2", "reader_id": readerID,
		}))
		if err != nil || res.IsError {
			t.Fatalf("shell_reader_unregister via locator failed: %v %v", err, res)
		}
		_ = second
	})

	t.Run("close", func(t *testing.T) {
		s, sid, primary, second, third := locatorFixture(t)
		// Regression: shell_close looked the argument up by raw id only, so a
		// locator was answered with shell_not_found even though it resolved.
		res, err := s.handleCloseShell(context.Background(), makeRequest(map[string]any{"shell_id": "termcp://#" + sid + ":2"}))
		if err != nil || res.IsError {
			t.Fatalf("shell_close via locator failed: %v %v", err, res)
		}
		if cs := s.sessMgr.GetChildShell(second); cs != nil {
			t.Error("shell_close answered success but the channel still exists")
		}
		// The channel it named is gone; its neighbours are untouched and keep
		// their numbers (the issue #73 contract).
		if cs := s.sessMgr.GetChildShell(primary); cs == nil {
			t.Error("closing :2 closed the primary channel instead")
		}
		if cs := s.sessMgr.GetChildShell(third); cs == nil {
			t.Error("closing :2 closed the third channel instead")
		}
	})

	t.Run("open and list", func(t *testing.T) {
		s, sid, _, _, _ := locatorFixture(t)
		res, err := s.handleStartSubShell(context.Background(), makeRequest(map[string]any{"session_id": "termcp://#" + sid}))
		if err != nil || res.IsError {
			t.Fatalf("shell_open via locator failed: %v %v", err, res)
		}
		// The response's session_id must be an id, not the locator that was passed
		// in: callers paste these back.
		if got := parseResult(t, res)["session_id"]; got != sid {
			t.Errorf("shell_open echoed session_id %v, want the canonical %q", got, sid)
		}

		res, err = s.handleListSubshells(context.Background(), makeRequest(map[string]any{"session_id": "termcp://#" + sid}))
		if err != nil || res.IsError {
			t.Fatalf("shell_list via locator failed: %v %v", err, res)
		}
		body := parseResult(t, res)
		if body["session_id"] != sid {
			t.Errorf("shell_list echoed session_id %v, want the canonical %q", body["session_id"], sid)
		}
		if shells, _ := body["shells"].([]any); len(shells) == 0 {
			t.Error("shell_list via locator returned no shells")
		}
	})

	t.Run("message spans", func(t *testing.T) {
		s, sid, _, second, _ := locatorFixture(t)
		// Regression: the marks were read with the raw argument, so a locator
		// addressed a nonexistent log and returned an empty transcript silently.
		res, err := s.handleMessageOps(context.Background(), makeRequest(map[string]any{
			"action": "list", "session_id": "termcp://#" + sid, "shell_id": "termcp://#" + sid + ":2",
		}))
		if err != nil || res.IsError {
			t.Fatalf("message(list) via locator failed: %v %v", err, res)
		}
		body := parseResult(t, res)
		if body["session_id"] != sid || body["shell_id"] != second {
			t.Errorf("message(list) returned session_id=%v shell_id=%v, want %q/%q",
				body["session_id"], body["shell_id"], sid, second)
		}
		// Omitting shell_id means the primary channel, and it must say so.
		res, err = s.handleMessageOps(context.Background(), makeRequest(map[string]any{
			"action": "list", "session_id": "termcp://#" + sid,
		}))
		if err != nil || res.IsError {
			t.Fatalf("message(list) without shell_id failed: %v %v", err, res)
		}
		if got := parseResult(t, res)["shell_id"]; got == "" || got == nil {
			t.Error("message(list) resolved no shell for a session-level locator")
		}
	})
}

// TestForwardToolsKeyForwardsByResolvedSession is the regression for the
// listener-leak: a forward created through a locator stored the locator string as
// its SessionID, so the DEAD cascade (which matches on the real session id) never
// found it and the local listener outlived its session.
func TestForwardToolsKeyForwardsByResolvedSession(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)
	s.forwardMgr = forward.NewForwardManager()
	s.sessMgr.SetOnDeadHook(s.forwardMgr.CloseBySession)

	res, err := s.handleForwardOps(context.Background(), makeRequest(map[string]any{
		"action": "dynamic", "session_id": "termcp://#" + sid, "local_port": 0,
	}))
	if err != nil || res.IsError {
		t.Fatalf("forward(dynamic) via locator failed: %v %v", err, res)
	}

	fws := s.forwardMgr.List()
	if len(fws) != 1 {
		t.Fatalf("forward count = %d, want 1", len(fws))
	}
	if fws[0].SessionID != sid {
		t.Fatalf("forward stored session_id %q, want the canonical %q — the DEAD cascade cannot match it",
			fws[0].SessionID, sid)
	}

	// The cascade must actually reach it.
	if _, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid, "force": true,
	})); err != nil {
		t.Fatal(err)
	}
	if got := len(s.forwardMgr.List()); got != 0 {
		t.Errorf("%d forward(s) survived the session's DEAD cascade", got)
	}
}

// TestForwardToolsWithoutManagerFailCleanly pins that the create actions report
// not_configured instead of dereferencing a nil manager. list/close already
// guarded; the three create actions did not, so a deployment without a forward
// manager (an API-only build) panicked on a forward request.
func TestForwardToolsWithoutManagerFailCleanly(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)
	s.forwardMgr = nil

	for _, action := range []string{"local", "remote", "dynamic"} {
		args := map[string]any{"action": action, "session_id": sid, "remote_host": "localhost", "remote_port": 22, "local_port": 0}
		if action == "remote" {
			// remote validates its own listener port before reaching the manager.
			args["local_port"] = 12345
		}
		res, err := s.handleForwardOps(context.Background(), makeRequest(args))
		if err != nil {
			t.Fatalf("forward(%s) with no manager returned a transport error: %v", action, err)
		}
		if !res.IsError {
			t.Errorf("forward(%s) with no manager should fail", action)
			continue
		}
		if code, _ := decodeToolError(t, res); code != CodeNotConfigured {
			t.Errorf("forward(%s) error_code = %q, want %q", action, code, CodeNotConfigured)
		}
	}
}

// TestNotifyToolsStoreResolvedShellID is the regression for the rule-leak: a rule
// registered through a locator stored the locator as its ShellID, so
// (a) the cascade cleanup keyed on the real shell id never removed it, leaving
// timers running for a shell that no longer exists, and (b) the resource
// broadcast URI became termcp://shells/termcp://#<sid>:2, which names nothing.
func TestNotifyToolsStoreResolvedShellID(t *testing.T) {
	s, sid, _, second, _ := locatorFixture(t)
	loc := "termcp://#" + sid + ":2"

	res, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action": "register", "shell_id": loc, "channel": "resource", "event": "output",
	}))
	if err != nil || res.IsError {
		t.Fatalf("shell_notify(register) via locator failed: %v %v", err, res)
	}
	body := parseResult(t, res)
	if body["shell_id"] != second {
		t.Fatalf("rule stored shell_id %v, want the canonical %q", body["shell_id"], second)
	}

	// list: the optional filter must resolve too, or a locator selects nothing.
	res, err = s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action": "list", "shell_id": loc,
	}))
	if err != nil || res.IsError {
		t.Fatalf("shell_notify(list) via locator failed: %v %v", err, res)
	}
	rules, _ := parseResult(t, res)["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("shell_notify(list) via locator returned %d rules, want the 1 just registered", len(rules))
	}

	// Cascade cleanup by the real shell id — what the exit watcher calls — must
	// find the rule.
	s.notifyMgr.ClearShell(second)
	if left := s.notifyMgr.List(second); len(left) != 0 {
		t.Errorf("ClearShell(%s) left %d rule(s): the cascade cannot see them", second, len(left))
	}
}

// TestNotifyUserResolvesSessionLocator pins that the human-facing notification
// highlights the right card: the UI matches on the id, so a locator echoed back
// would highlight nothing.
func TestNotifyUserResolvesSessionLocator(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)

	var got string
	s.uiNotify = func(level, title, message, sessionID string, durationSec int) int {
		got = sessionID
		return 1
	}
	res, err := s.handleNotifyUser(context.Background(), makeRequest(map[string]any{
		"message": "hello", "session_id": "termcp://#" + sid,
	}))
	if err != nil || res.IsError {
		t.Fatalf("notify_user via locator failed: %v %v", err, res)
	}
	if got != sid {
		t.Fatalf("uiNotify received session_id %q, want the canonical %q", got, sid)
	}
	if body := parseResult(t, res); body["session_id"] != sid {
		t.Errorf("notify_user echoed session_id %v, want %q", body["session_id"], sid)
	}
}

// TestFileToolsAcceptSessionLocator covers the SFTP group, whose handlers take
// session_id through sshClientForSession. Only the session-locator spelling is
// exercised: the operations themselves are covered elsewhere, and the point here
// is that the id is resolved, not that SFTP works.
func TestFileToolsAcceptSessionLocator(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)

	res, err := s.handleFileGetwd(context.Background(), makeRequest(map[string]any{"session_id": "termcp://#" + sid}))
	if err != nil || res.IsError {
		t.Fatalf("file_getwd via locator failed: %v %v", err, res)
	}
	if wd, _ := parseResult(t, res)["directory"].(string); wd == "" {
		t.Error("file_getwd returned no directory")
	}

	// A shell locator names the same container, so it must work here too.
	res, err = s.handleFileStat(context.Background(), makeRequest(map[string]any{
		"session_id": "termcp://#" + sid + ":1", "remote_path": ".",
	}))
	if err != nil || res.IsError {
		text := ""
		if res != nil {
			text = res.Content[0].(mcpgo.TextContent).Text
		}
		t.Fatalf("file_stat via shell locator failed: %v %s", err, text)
	}
}

// TestLocatorArgumentEchoesCanonicalIDs is the cross-cutting guard: no response
// may hand back an id field that still holds the locator the caller passed.
// Callers paste these fields into the next call (and into scripts), so an echo
// propagates the locator into places that treat it as an id.
func TestLocatorArgumentEchoesCanonicalIDs(t *testing.T) {
	s, sid, primary, second, _ := locatorFixture(t)
	base := "termcp://#" + sid

	calls := []struct {
		what string
		args map[string]any
		keys []string
	}{
		{"shell_list", map[string]any{"session_id": base}, []string{"session_id"}},
		{"shell_open", map[string]any{"session_id": base}, []string{"session_id"}},
		{"message", map[string]any{"action": "list", "session_id": base, "shell_id": base + ":2"}, []string{"session_id", "shell_id"}},
	}
	for _, c := range calls {
		var res *mcpgo.CallToolResult
		var err error
		switch c.what {
		case "shell_list":
			res, err = s.handleListSubshells(context.Background(), makeRequest(c.args))
		case "shell_open":
			res, err = s.handleStartSubShell(context.Background(), makeRequest(c.args))
		case "message":
			res, err = s.handleMessageOps(context.Background(), makeRequest(c.args))
		}
		if err != nil || res.IsError {
			t.Errorf("%s failed: %v %v", c.what, err, res)
			continue
		}
		body := parseResult(t, res)
		for _, k := range c.keys {
			v, _ := body[k].(string)
			if v == "" {
				continue // absent fields are not an echo
			}
			if strings.Contains(v, "termcp://") || strings.Contains(v, "#") {
				t.Errorf("%s echoed %s=%q; response ids must be canonical", c.what, k, v)
			}
		}
	}

	// Sanity: the ids the fixture returned are themselves canonical, so the check
	// above is not passing because everything is empty.
	for _, id := range []string{sid, primary, second} {
		if id == "" || strings.ContainsAny(id, "#:") {
			t.Fatalf("fixture produced a non-canonical id %q", id)
		}
	}
}

// TestDeadSessionShellLookupIsRefusedConsistently pins that every spelling of a
// shell reaches the same verdict on a closed session.
//
// The retained shell of a DEAD session stays in the live map (its tail output must
// remain readable), so a raw shell id could be returned before any status check
// ran, while the session-id and locator spellings were refused. The write then
// failed deeper down at the closed transport with a generic operation error, which
// reads like a transient fault rather than "this session is closed — read its
// output instead".
func TestDeadSessionShellLookupIsRefusedConsistently(t *testing.T) {
	s, sid, primary, second, _ := locatorFixture(t)

	if _, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid, "force": true,
	})); err != nil {
		t.Fatal(err)
	}

	// Both retained channels, through every spelling.
	for _, id := range []string{
		primary, second,
		sid,
		"termcp://#" + sid, "termcp://#" + sid + ":1", "termcp://#" + sid + ":2",
	} {
		_, bad := s.requireShell(id)
		if bad == nil {
			t.Errorf("requireShell(%q) accepted a shell in a closed session", id)
			continue
		}
		code, msg := decodeToolError(t, bad)
		if code != CodeSessionNotFound {
			t.Errorf("requireShell(%q) error_code = %q, want %q", id, code, CodeSessionNotFound)
		}
		if !strings.Contains(msg, "closed") || !strings.Contains(msg, "shell_output") {
			t.Errorf("requireShell(%q) message should name the state and the way out, got %q", id, msg)
		}
	}

	// A write must be refused the same way, not surface a transport error.
	for _, id := range []string{primary, "termcp://#" + sid + ":2"} {
		res, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{"shell_id": id, "text": "echo hi"}))
		if err != nil {
			t.Fatalf("shell_input(%q) returned a transport error: %v", id, err)
		}
		if !res.IsError {
			t.Errorf("shell_input(%q) wrote into a closed session", id)
			continue
		}
		if code, _ := decodeToolError(t, res); code != CodeSessionNotFound {
			t.Errorf("shell_input(%q) error_code = %q, want %q", id, code, CodeSessionNotFound)
		}
	}

	// Reading is still allowed: that is the whole point of keeping the session.
	out, err := s.handleReadOutput(context.Background(), makeRequest(map[string]any{
		"shell_id": "termcp://#" + sid + ":1", "tail_lines": 5.0,
	}))
	if err != nil || out.IsError {
		t.Fatalf("shell_output on a closed session must still work: %v %v", err, out)
	}
}

// TestApprovalGateCannotBeBypassedByALocator is the regression for an
// approval-mode hole that a locator spelling opened.
//
// The gate looked the session up by the RAW argument and, on a miss, declined to
// gate — while the handler then resolved the same argument itself and performed
// the operation. So an approval-gated session held a write named by raw id and
// executed the very same write named by locator, with nobody asked. Gating is the
// feature that decides whether an agent may write to a remote host, so the two
// spellings must reach the same verdict.
func TestApprovalGateCannotBeBypassedByALocator(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)
	if err := s.sessMgr.EnableApproval(sid, 1, 0); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	for _, tc := range []struct {
		spelling string
		target   string
	}{
		{"raw id", filepath.Join(dir, "by-id.txt")},
		{"locator", filepath.Join(dir, "by-locator.txt")},
		{"session locator", filepath.Join(dir, "by-session.txt")},
	} {
		sidArg := sid
		if tc.spelling == "locator" {
			sidArg = "termcp://#" + sid
		}
		if tc.spelling == "session locator" {
			sidArg = "termcp://#" + sid + ":1"
		}
		res, err := s.handleFileWrite(context.Background(), makeRequest(map[string]any{
			"session_id": sidArg, "remote_path": tc.target, "data": "gated",
		}))
		if err != nil {
			t.Fatalf("%s: %v", tc.spelling, err)
		}
		if res.IsError {
			t.Fatalf("%s: gated write returned an error: %s", tc.spelling, res.Content[0].(mcpgo.TextContent).Text)
		}
		if !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "review") {
			t.Errorf("%s: write was not held for review: %s", tc.spelling, res.Content[0].(mcpgo.TextContent).Text)
		}
		if _, statErr := os.Stat(tc.target); statErr == nil {
			t.Errorf("%s: the gated write executed before approval", tc.spelling)
		}
	}

	// An ungated session must still not be gated: the check is the session's
	// approval setting, not the presence of a locator.
	plain, plainSid, _, _, _ := locatorFixture(t)
	res, err := plain.handleFileWrite(context.Background(), makeRequest(map[string]any{
		"session_id": "termcp://#" + plainSid, "remote_path": filepath.Join(t.TempDir(), "plain.txt"), "data": "x",
	}))
	if err != nil || res.IsError {
		t.Fatalf("ungated write via locator failed: %v %v", err, res)
	}
	if strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "review") {
		t.Error("an ungated session held a write for review")
	}
}

// TestFileToolURLsUseResolvedSessionID pins that the URLs and id a file tool
// hands back are usable. These are meant to be opened or pasted, so a locator in
// the path produces /api/sessions/termcp://#<sid>/files/download — a URL carrying
// '#' and '://' that addresses nothing.
func TestFileToolURLsUseResolvedSessionID(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)
	s.baseURL = "http://127.0.0.1:0"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what   string
		handle func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}{
		{"file_stat", s.handleFileStat},
		{"file_urls", s.handleGetFileURLs},
	} {
		res, err := tc.handle(context.Background(), makeRequest(map[string]any{
			"session_id":  "termcp://#" + sid,
			"remote_path": filepath.Join(dir, "probe.txt"),
		}))
		if err != nil || res.IsError {
			t.Fatalf("%s via locator failed: %v %v", tc.what, err, res)
		}
		body := parseResult(t, res)
		if got := body["session_id"]; got != sid {
			t.Errorf("%s echoed session_id %v, want the canonical %q", tc.what, got, sid)
		}
		for _, key := range []string{"download_url", "upload_url"} {
			u, _ := body[key].(string)
			if u == "" {
				continue
			}
			if !strings.Contains(u, "/api/sessions/"+sid+"/") {
				t.Errorf("%s %s = %q, want it to address session %q", tc.what, key, u, sid)
			}
			if strings.Contains(u, "termcp://") || strings.Contains(u, "#") {
				t.Errorf("%s %s = %q carries the locator; the URL addresses nothing", tc.what, key, u)
			}
		}
	}
}

// TestMessageSpansOnClosedSessionResolveShells pins that a read still works when
// a closed session's channel is named explicitly.
//
// The mark index lives in log.bin, which outlives the transport, so
// message(session_id=...) served a DEAD session while message(shell_id=...)
// refused one: the write path's status check had leaked into a read. Both
// spellings must answer.
func TestMessageSpansOnClosedSessionResolveShells(t *testing.T) {
	s, sid, primary, _, _ := locatorFixture(t)

	if _, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid, "force": true,
	})); err != nil {
		t.Fatal(err)
	}

	for _, args := range []map[string]any{
		{"action": "list", "session_id": sid},
		{"action": "list", "session_id": sid, "shell_id": primary},
		{"action": "list", "session_id": "termcp://#" + sid, "shell_id": "termcp://#" + sid + ":1"},
	} {
		res, err := s.handleMessageOps(context.Background(), makeRequest(args))
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if res.IsError {
			t.Errorf("%v: reading spans of a closed session failed: %s", args, res.Content[0].(mcpgo.TextContent).Text)
			continue
		}
		if body := parseResult(t, res); body["shell_id"] == "" || body["shell_id"] == nil {
			t.Errorf("%v: no shell resolved for a closed session", args)
		}
	}
}
