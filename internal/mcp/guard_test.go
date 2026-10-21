package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// guardTestSession creates a running session carrying the given restriction, so
// a guard test exercises a real session the way a restricted extension would.
func guardTestSession(t *testing.T, s *Server, allow, paths []string) string {
	t.Helper()
	r, err := newTestRestrictedSession(s, allow, paths)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newTestRestrictedSession(s *Server, allow, paths []string) (string, error) {
	r, err := session.NewRestriction(allow, paths)
	if err != nil {
		return "", err
	}
	sess, err := s.sessMgr.Create(session.Config{
		Command:     testShell(),
		Args:        nil,
		Mode:        api.ModePipe,
		Name:        "restricted",
		SSHConfig:   "internal",
		Rows:        24,
		Cols:        80,
		Restriction: r,
	})
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// createTestSession makes an ordinary unrestricted session for the guard tests.
func createTestSession(t *testing.T, s *Server, mode api.SessionMode) string {
	t.Helper()
	sess, err := s.sessMgr.Create(session.Config{
		Command:   testShell(),
		Args:      nil,
		Mode:      mode,
		Name:      "plain",
		SSHConfig: "internal",
		Rows:      24,
		Cols:      80,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// TestGuardAllowsUnrestrictedSession pins the backward-compatibility contract:
// a session created without a restriction keeps the whole tool surface, so the
// guard is invisible to every existing caller.
func TestGuardAllowsUnrestrictedSession(t *testing.T) {
	s := newTestServer(t)
	sid := createTestSession(t, s, api.ModePipe)

	// Tools the whitelist of a skill extension would forbid all pass here.
	for _, tool := range []string{"file_write", "file_delete", "session_terminate"} {
		if bad := (toolGuard{server: s}).check(context.Background(), tool, lookupGate(tool), makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": "/etc/shadow",
		})); bad != nil {
			t.Errorf("unrestricted session: %s was refused", tool)
		}
	}
}

// TestGuardRefusesToolsOutsideWhitelist pins the real refusal: a restricted
// session returns a CodeForbidden result naming the tool, and the allowed ones
// still pass.
func TestGuardRefusesToolsOutsideWhitelist(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read", "file_stat"}, []string{"~/skills/foo/"})

	for _, tool := range []string{"file_read", "file_stat"} {
		if bad := (toolGuard{server: s}).check(context.Background(), tool, lookupGate(tool), makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": "~/skills/foo/SKILL.md",
		})); bad != nil {
			code, msg := decodeToolError(t, bad)
			t.Errorf("allowed tool %s was refused: %s (%s)", tool, msg, code)
		}
	}

	for _, tool := range []string{"file_write", "file_delete", "shell_input", "session_terminate"} {
		bad := (toolGuard{server: s}).check(context.Background(), tool, lookupGate(tool), makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": "~/skills/foo/SKILL.md",
		}))
		if bad == nil {
			t.Fatalf("restricted session: %s should have been refused", tool)
		}
		code, msg := decodeToolError(t, bad)
		if code != CodeForbidden {
			t.Errorf("%s refusal code = %q, want %q (msg: %s)", tool, code, CodeForbidden, msg)
		}
	}
}

// TestGuardRefusesPathOutsideWhitelist pins that reachability is the gate, not
// direction: reading a path outside the tree is refused even though file_read
// itself is allowed.
func TestGuardRefusesPathOutsideWhitelist(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read"}, []string{"~/skills/foo/"})

	bad := (toolGuard{server: s}).check(context.Background(), "file_read", lookupGate("file_read"), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "/etc/passwd",
	}))
	if bad == nil {
		t.Fatal("file_read outside the whitelist path should be refused")
	}
	code, msg := decodeToolError(t, bad)
	if code != CodeForbidden {
		t.Errorf("code = %q, want %q", code, CodeForbidden)
	}
	if !strings.Contains(msg, "outside") {
		t.Errorf("message should name the path problem, got %q", msg)
	}

	// The same tool on an allowed path passes.
	if bad := (toolGuard{server: s}).check(context.Background(), "file_read", lookupGate("file_read"), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "~/skills/foo/scripts/run.sh",
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("file_read on an allowed path was refused: %s", msg)
	}
}

// TestGuardResolvesByShellID pins that a shell-addressed tool is gated through
// its parent session's restriction, since the restriction belongs to the
// container the shell was created under.
func TestGuardResolvesByShellID(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"shell_output"}, nil)
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session not found")
	}
	shellID := sess.PrimaryShellID()
	if shellID == "" {
		t.Fatal("test session has no primary shell")
	}

	if bad := (toolGuard{server: s}).check(context.Background(), "shell_output", lookupGate("shell_output"), makeRequest(map[string]any{
		"shell_id": shellID,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("shell_output on the session's own shell was refused: %s", msg)
	}
	if bad := (toolGuard{server: s}).check(context.Background(), "shell_input", lookupGate("shell_input"), makeRequest(map[string]any{
		"shell_id": shellID,
	})); bad == nil {
		t.Error("shell_input should be refused on a session that allows only shell_output")
	}
}

// TestGuardIgnoresSessionlessTools pins that tools which address no existing
// session pass untouched: session_start and friends have nothing to gate
// against, and gating them would break ordinary session creation.
func TestGuardIgnoresSessionlessTools(t *testing.T) {
	s := newTestServer(t)
	// A restricted session exists to prove the guard is armed and still does not
	// fire on a call that names no target.
	guardTestSession(t, s, []string{"file_read"}, nil)

	for _, args := range []map[string]any{
		{"command": "bash"},
		{"ssh_config": "internal"},
		{},
	} {
		if bad := (toolGuard{server: s}).check(context.Background(), "session_start", lookupGate("session_start"), makeRequest(args)); bad != nil {
			t.Errorf("session_start with %v was refused", args)
		}
	}
}

// TestGuardUnknownSessionPasses pins that the guard does not preempt the
// handlers' own not-found errors: an id that resolves to nothing is the
// handler's business, so the guard lets it through and the refusal the caller
// sees comes from the tool with its proper code.
func TestGuardUnknownSessionPasses(t *testing.T) {
	s := newTestServer(t)
	bad := (toolGuard{server: s}).check(context.Background(), "file_read", lookupGate("file_read"), makeRequest(map[string]any{
		"session_id": "no-such-session",
	}))
	if bad != nil {
		t.Error("an unresolvable id should reach the handler, not be refused by the guard")
	}
}

// TestGuardThroughHandler pins the end-to-end path: the guard runs inside
// withLogging, so a call that the session's whitelist refuses fails with the
// forbidden code before the handler is entered.
func TestGuardThroughHandler(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read"}, []string{"~/skills/foo/"})

	result, err := withLogging("file_delete", s.handleFileDelete, s)(context.Background(), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "~/skills/foo/x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("file_delete on a read-only extension session must be refused")
	}
	if code, _ := decodeToolError(t, result); code != CodeForbidden {
		t.Errorf("code = %q, want %q", code, CodeForbidden)
	}
}

// TestGuardResolvesEverySpellingOfTheTarget pins the property the guard's first
// version got wrong: it must resolve a session from exactly the spellings the
// handlers accept, or the whitelist is decoration.
//
// The handlers (requireSession / requireShell) accept a raw session id, a raw
// shell id, and a termcp:// locator in either argument. The guard handled a raw
// shell id only, so every other spelling resolved to nothing, skipped the
// check, and reached the handler — which then resolved the id correctly and did
// the work. A caller could therefore read any path, or run any tool, on a
// session whose template forbade it by writing the id differently.
func TestGuardResolvesEverySpellingOfTheTarget(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_stat"}, []string{"~/skills/foo/"})
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session not found")
	}
	shellID := sess.PrimaryShellID()
	if shellID == "" {
		t.Fatal("test session has no primary shell")
	}
	locatorSession := "termcp://#" + sid
	locatorShell := "termcp://#" + sid + ":1"

	// (tool, args) pairs that must all be refused: the tool is outside the
	// whitelist and the path is outside the tree, and each pair reaches the same
	// session by a different spelling.
	cases := []struct {
		name string
		args map[string]any
	}{
		{"session_id raw", map[string]any{"session_id": sid, "remote_path": "/etc/passwd"}},
		{"session_id locator", map[string]any{"session_id": locatorSession, "remote_path": "/etc/passwd"}},
		{"session_id as shell id", map[string]any{"session_id": shellID, "remote_path": "/etc/passwd"}},
		{"shell_id raw", map[string]any{"shell_id": shellID}},
		{"shell_id is the session id", map[string]any{"shell_id": sid}},
		{"shell_id locator", map[string]any{"shell_id": locatorShell}},
	}
	for _, tc := range cases {
		if bad := (toolGuard{server: s}).check(context.Background(), "file_write", lookupGate("file_write"), makeRequest(tc.args)); bad == nil {
			t.Errorf("%s: file_write must be refused", tc.name)
		} else if code, _ := decodeToolError(t, bad); code != CodeForbidden {
			t.Errorf("%s: code = %q, want %q", tc.name, code, CodeForbidden)
		}
	}

	// The path rule is enforced through the same spellings, including on the tool
	// that IS allowed.
	for _, tc := range cases {
		args := map[string]any{"remote_path": "/etc/passwd"}
		for k, v := range tc.args {
			if k == "remote_path" {
				continue
			}
			args[k] = v
		}
		if bad := (toolGuard{server: s}).check(context.Background(), "file_stat", lookupGate("file_stat"), makeRequest(args)); bad == nil {
			t.Errorf("%s: file_stat outside the path whitelist must be refused", tc.name)
		}
	}
}

// TestGuardGatesLocalPath pins that the download destination is a path too.
//
// file_read(mode=file) writes local_path on the termcp host and file_write
// streams from it; leaving it out of the guard let an extension granted a single
// remote tree read any file the termcp process can open.
func TestGuardGatesLocalPath(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read", "file_write"}, []string{"~/skills/foo/"})

	bad := (toolGuard{server: s}).check(context.Background(), "file_read", lookupGate("file_read"), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "~/skills/foo/SKILL.md",
		"local_path":  "/home/someone/.ssh/id_rsa",
	}))
	if bad == nil {
		t.Fatal("a local_path outside the whitelist must be refused")
	}
	code, msg := decodeToolError(t, bad)
	if code != CodeForbidden {
		t.Errorf("code = %q, want %q", code, CodeForbidden)
	}
	if !strings.Contains(msg, "local_path") {
		t.Errorf("the refusal should name the argument, got %q", msg)
	}

	// Inside the whitelist it passes. A local download directory is an author's
	// choice, so the rule must be usable rather than a blanket ban.
	if bad := (toolGuard{server: s}).check(context.Background(), "file_read", lookupGate("file_read"), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "~/skills/foo/SKILL.md",
		"local_path":  "~/skills/foo/SKILL.md",
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("a local_path inside the whitelist was refused: %s", msg)
	}
}

var _ = context.Background

// TestGuardResolvesIndirectResourceIDs pins the last two ways a call could name
// work without naming a session.
//
// Two tools address what they act on by the RESOURCE's own id: forward(action=close)
// takes forward_id and shell_notify(action=unregister) takes rule_id. Neither
// argument was one the guard read, so the call resolved to no session, skipped the
// check, and reached the handler — a caller holding a restricted extension handle
// could close a tunnel it was never granted, or unregister a rule belonging to a
// session it may not touch. That is the same hole the REST forward-close route had
// (internal/webui/handler_forward.go looks the owner up for exactly this reason),
// on the surface an extension handle actually reaches.
//
// The guard resolves the owner from the same managers the handlers use, so this is
// a check rather than a second copy of the lookup.
func TestGuardResolvesIndirectResourceIDs(t *testing.T) {
	s := newTestServer(t)
	s.forwardMgr = forward.NewForwardManager()

	// A restricted session that may neither forward nor notify.
	sid := guardTestSession(t, s, []string{"file_read"}, []string{"~/skills/foo/"})
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session not found")
	}
	shellID := sess.PrimaryShellID()
	if shellID == "" {
		t.Fatal("test session has no primary shell")
	}

	// A forward and a rule are created directly on the managers: the handlers that
	// would normally make them are themselves gated, which is the point.
	// localDial=true keeps the tunnel off the SSH transport, so the test needs no
	// client — it is the same call the internal profile makes.
	fw, err := s.forwardMgr.CreateDynamic(sid, "restricted", "", 0, nil, true)
	if err != nil {
		t.Fatalf("CreateDynamic: %v", err)
	}
	rule, err := s.notifyMgr.Register(sid, shellID, notify.ChannelResource, notify.EventOutput, nil, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if bad := (toolGuard{server: s}).check(context.Background(), "forward", lookupGate("forward"), makeRequest(map[string]any{
		"action": "close", "forward_id": fw.ForwardID,
	})); bad == nil {
		t.Error("forward(close) on a session that granted no forwards must be refused")
	} else if code, _ := decodeToolError(t, bad); code != CodeForbidden {
		t.Errorf("forward(close) code = %q, want %q", code, CodeForbidden)
	}
	if bad := (toolGuard{server: s}).check(context.Background(), "shell_notify", lookupGate("shell_notify"), makeRequest(map[string]any{
		"action": "unregister", "rule_id": rule.ID,
	})); bad == nil {
		t.Error("shell_notify(unregister) on a session that granted no shell_notify must be refused")
	} else if code, _ := decodeToolError(t, bad); code != CodeForbidden {
		t.Errorf("shell_notify(unregister) code = %q, want %q", code, CodeForbidden)
	}

	// A resource owned by an ordinary session is untouched: the guard is
	// invisible to every caller that predates the whitelist.
	plain := createTestSession(t, s, api.ModePipe)
	plainShell := s.sessMgr.Get(plain).PrimaryShellID()
	fw2, err := s.forwardMgr.CreateDynamic(plain, "plain", "", 0, nil, true)
	if err != nil {
		t.Fatalf("CreateDynamic: %v", err)
	}
	rule2, err := s.notifyMgr.Register(plain, plainShell, notify.ChannelResource, notify.EventOutput, nil, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if bad := (toolGuard{server: s}).check(context.Background(), "forward", lookupGate("forward"), makeRequest(map[string]any{
		"action": "close", "forward_id": fw2.ForwardID,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("an unrestricted session's forward was refused: %s", msg)
	}
	if bad := (toolGuard{server: s}).check(context.Background(), "shell_notify", lookupGate("shell_notify"), makeRequest(map[string]any{
		"action": "unregister", "rule_id": rule2.ID,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("an unrestricted session's rule was refused: %s", msg)
	}

	// An id nothing owns is the handler's not-found error, not a refusal.
	if bad := (toolGuard{server: s}).check(context.Background(), "forward", lookupGate("forward"), makeRequest(map[string]any{
		"action": "close", "forward_id": "fw-does-not-exist",
	})); bad != nil {
		t.Error("an unknown forward_id should reach the handler, not be refused by the guard")
	}
}

// TestGuardGatesEveryEntryOfABatch pins the hole the comma form opened.
//
// A batch argument ("a,b") was resolved as ONE string. GetChildShell("a,b") finds
// nothing, and "found no session" is the guard's answer for "nothing to gate
// here", so the check returned nil and the handler — which splits the list itself
// — then acted on every entry. The whitelist and the namespace gate were both
// skipped for the whole batch, which is to say the two features that exist to
// bound a caller's reach were walkable around with a single comma.
//
// The rule is the one the session_id branch already documented: a batch is
// refused WHOLESALE, because acting on part of it would report a partial success
// and leave the caller to guess which entries landed.
func TestGuardGatesEveryEntryOfABatch(t *testing.T) {
	s := newTestServer(t)

	// A session whose whitelist forbids shell_input, and an ordinary one beside
	// it so the batch has a legitimate entry too: the refusal must not depend on
	// the restricted session coming first.
	locked := guardTestSession(t, s, []string{"shell_output"}, nil)
	lockedSess := s.sessMgr.Get(locked)
	if lockedSess == nil {
		t.Fatal("session not found")
	}
	lockedShell := lockedSess.PrimaryShellID()

	plain := createTestSession(t, s, api.ModePipe)
	plainSess := s.sessMgr.Get(plain)
	if plainSess == nil {
		t.Fatal("session not found")
	}
	plainShell := plainSess.PrimaryShellID()

	guard := toolGuard{server: s}
	for _, tc := range []struct {
		name string
		ids  string
	}{
		{"restricted first", lockedShell + "," + plainShell},
		{"restricted last", plainShell + "," + lockedShell},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, tool := range []string{"shell_input", "shell_key"} {
				bad := guard.check(context.Background(), tool, lookupGate(tool), makeRequest(map[string]any{
					"shell_id": tc.ids,
					"key":      "enter",
					"text":     "echo nope",
				}))
				if bad == nil {
					t.Fatalf("%s: a batch naming a shell its whitelist forbids must be refused", tool)
				}
				if code, _ := decodeToolError(t, bad); code != CodeForbidden {
					t.Errorf("%s: code = %q, want %q", tool, code, CodeForbidden)
				}
			}
		})
	}

	// The unrestricted batch is untouched: the guard is invisible to callers that
	// never opted into a whitelist.
	if bad := guard.check(context.Background(), "shell_input", lookupGate("shell_input"), makeRequest(map[string]any{
		"shell_id": plainShell + "," + plainShell,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("an unrestricted batch was refused: %s", msg)
	}

	// session_id batches keep the behaviour they already had, single id included.
	if bad := guard.check(context.Background(), "session_terminate", lookupGate("session_terminate"), makeRequest(map[string]any{
		"session_id": plain + "," + locked,
	})); bad == nil {
		t.Error("a session_id batch naming a restricted session must be refused")
	}
	if bad := guard.check(context.Background(), "shell_output", lookupGate("shell_output"), makeRequest(map[string]any{
		"session_id": locked,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("a single allowed id was refused: %s", msg)
	}
}

// TestGuardBatchKeepsNamespacesApart pins the other half of the same hole: a
// comma is not a way into somebody else's namespace.
//
// The namespace gate ran once, on an id that resolved to nothing, so a namespaced
// caller that joined its own shell id with a foreign one had the whole batch
// waved through — the guard never looked at either entry.
func TestGuardBatchKeepsNamespacesApart(t *testing.T) {
	s := newTestServer(t)
	alice := namespaceSession(t, s, "alice")
	bob := namespaceSession(t, s, "bob")

	shellOf := func(sid string) string {
		t.Helper()
		sess := s.sessMgr.Get(sid)
		if sess == nil {
			t.Fatal("session not found")
		}
		return sess.PrimaryShellID()
	}
	aliceShell, bobShell := shellOf(alice), shellOf(bob)
	guard := toolGuard{server: s}

	for _, ids := range []string{aliceShell + "," + bobShell, bobShell + "," + aliceShell} {
		bad := guard.check(namespaceContext("alice"), "shell_input", lookupGate("shell_input"), makeRequest(map[string]any{
			"shell_id": ids,
		}))
		if bad == nil {
			t.Fatalf("alice reached bob's shell through the batch %q", ids)
		}
		if code, _ := decodeToolError(t, bad); code != CodeForbidden {
			t.Errorf("code = %q, want %q", code, CodeForbidden)
		}
	}

	// Alice's own batch still works, which is what makes the refusal above
	// isolation rather than a broken feature.
	if bad := guard.check(namespaceContext("alice"), "shell_input", lookupGate("shell_input"), makeRequest(map[string]any{
		"shell_id": aliceShell + "," + aliceShell,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("alice was refused her own batch: %s", msg)
	}
}

// TestGuardBatchRefusalStopsTheWrite pins the end-to-end property the two tests
// above only imply: a refused batch must leave NO trace in the foreign shell.
//
// A guard that returns an error after the handler has begun writing would pass a
// check-the-refusal test and still leak bytes, so this one reads the transcript
// back. It is the assertion that would catch a "check each id but write as you
// go" implementation.
func TestGuardBatchRefusalStopsTheWrite(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	r, err := session.NewRestriction([]string{"shell_output"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := s.sessMgr.Create(session.Config{
		Command: testShell(), Mode: api.ModePipe, Name: "locked",
		SSHConfig: "internal", Rows: 24, Cols: 80, Restriction: r,
	})
	if err != nil {
		t.Fatal(err)
	}
	plainID := createTestSession(t, s, api.ModePipe)
	lockedShell := locked.PrimaryShellID()
	plainShell := s.sessMgr.Get(plainID).PrimaryShellID()

	res, err := withLogging("shell_input", s.handleSendInput, s)(context.Background(), makeRequest(map[string]any{
		"shell_id": plainShell + "," + lockedShell,
		"text":     "echo SHOULD_NOT_LAND\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("the batch was not refused")
	}

	// Give a leaked write time to reach the log, then read it back. A pipe child
	// records no echo, so anything in the transcript came from an actual write.
	time.Sleep(200 * time.Millisecond)
	out, _, err := s.sessMgr.OutputByteRange(locked.ID, lockedShell, 0, 1<<20)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if strings.Contains(string(out), "SHOULD_NOT_LAND") {
		t.Fatalf("the refused write reached the restricted shell: %q", string(out))
	}
}

// TestGuardGatesEveryHandleSpelling is a SECURITY regression test, not a
// convenience one.
//
// checkEach skips an id that resolves to nothing, leaving the not-found error to
// the handler. That is only safe while the guard resolves every spelling the
// handler accepts — and it did not: `shell_id: "<sid>:2"` reached requireShell
// (which understands the short handle) while sessionForShellID resolved nothing,
// so a session restricted to shell_output accepted shell_input.
//
// The four spellings below are the same channel. Each must be refused, and the
// bare-id cases must stay refused too, so a future spelling cannot be added to
// one side only.
func TestGuardGatesEveryHandleSpelling(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"shell_output"}, nil)

	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session missing")
	}
	second, err := sess.CreateChildShell(testShell(), nil, false, 24, 80, "second")
	if err != nil || second == nil {
		t.Fatalf("creating a second channel: %v", err)
	}

	spellings := []string{
		sid,                       // the session itself
		second.ID,                 // a raw shell id
		"termcp://#" + sid + ":2", // the locator the Web UI emits
		sid + ":2",                // the short handle ps/info/pty print
	}
	for _, spelling := range spellings {
		bad := (toolGuard{server: s}).check(context.Background(), "shell_input", lookupGate("shell_input"), makeRequest(map[string]any{
			"shell_id": spelling,
			"data":     "echo pwned\n",
		}))
		if bad == nil {
			t.Errorf("shell_input with shell_id=%q was NOT refused: the whitelist does not gate this spelling", spelling)
			continue
		}
		if code, _ := decodeToolError(t, bad); code != CodeForbidden {
			t.Errorf("%q refused with code %q, want %q", spelling, code, CodeForbidden)
		}
	}
}

// intPtr names a value the API sends as an optional integer.
func intPtr(v int) *int { return &v }
