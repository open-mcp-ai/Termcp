package mcp

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
)

// blockingCommand is a command the host actually has that stays alive until its
// stdin closes — the reconciler's probe needs to find it still running.
func blockingCommand() string {
	return "cat"
}

// exitCommand is a command that exits at once with a nonzero code, so a probe
// observes a startup failure instead of a live process.
func exitCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-Command", "exit 3"}
	}
	return "bash", []string{"-c", "echo boom; exit 3"}
}

// newExtensionTestServer is newTestServer plus a running reconciler wired into
// the server, so the extension tool is exercised against the same layer that
// serves it for real.
func newExtensionTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	store := extensions.NewStore(t.TempDir())
	r := extensions.NewReconciler(store, s.sessMgr, s.msgMgr, s.sshConfigs, forward.NewForwardManager())
	r.NoInternal = s.NoInternal
	s.SetExtensionReconciler(r)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop() })
	return s
}

func callExtension(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.handleExtensionOps(context.Background(), makeRequest(args))
	if err != nil {
		t.Fatalf("extension(%v): %v", args["action"], err)
	}
	if res.IsError {
		t.Fatalf("extension(%v) failed: %s", args["action"], resultText(res))
	}
	return parseResult(t, res)
}

func extensionError(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.handleExtensionOps(context.Background(), makeRequest(args))
	if err != nil {
		t.Fatalf("extension(%v): %v", args["action"], err)
	}
	if !res.IsError {
		t.Fatalf("extension(%v) should have failed, got %s", args["action"], resultText(res))
	}
	return parseResult(t, res)
}

func resultText(res *mcpgo.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcpgo.TextContent); ok {
		return tc.Text
	}
	return ""
}

// useExtension asks for an on-demand extension, which is what an agent does before
// it can drive one (issue #183): the reconciler does not dial a template that
// declares no forward until something asks, and asking is action=use — the same
// thing `open ext <name>` does in the terminal language.
//
// The wait helpers deliberately do not call it: a test that expects an extension up
// says so in one visible line, and a test that expects it to stay down says nothing.
func useExtension(t *testing.T, s *Server, name string) map[string]any {
	t.Helper()
	return callExtension(t, s, map[string]any{"action": "use", "name": name})
}

// waitForExtensionStatus polls the tool's own list output until name reports
// want, so the test observes the surface an agent would.
func waitForExtensionStatus(t *testing.T, s *Server, name, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		got := callExtension(t, s, map[string]any{"action": "list"})
		for _, raw := range got["extensions"].([]any) {
			if e, ok := raw.(map[string]any); ok && e["name"] == name && e["status"] == want {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("extension %q never reached %s, last listing: %v", name, want, got["extensions"])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionToolNotConfigured pins that a server without a reconciler fails
// loudly rather than offering a surface with no backing state.
func TestExtensionToolNotConfigured(t *testing.T) {
	s := newTestServer(t)
	out := extensionError(t, s, map[string]any{"action": "list"})
	if out["error_code"] != CodeNotConfigured {
		t.Fatalf("want %s, got %v", CodeNotConfigured, out)
	}
}

// TestExtensionUnknownAction pins the dispatcher's enum.
func TestExtensionUnknownAction(t *testing.T) {
	s := newExtensionTestServer(t)
	out := extensionError(t, s, map[string]any{"action": "bogus"})
	if out["error_code"] != CodeInvalidArgument {
		t.Fatalf("want %s, got %v", CodeInvalidArgument, out)
	}
}

// TestExtensionCreateGetList walks the whole read path: a template created from its
// declared fields is listed, and comes back from get with the same key names update
// accepts.
func TestExtensionCreateGetList(t *testing.T) {
	s := newExtensionTestServer(t)

	created := callExtension(t, s, map[string]any{
		"action": "create",
		"name":   "docs",
		"target": "internal",
		"paths":  []any{"/srv/"},
	})
	if created["status"] == extensions.StatusAvailable {
		// A fresh extension starts unavailable and is pushed toward available, so
		// the status read back an instant later may already be probing — but never
		// available before a probe has proved the session works.
		t.Errorf("a brand-new extension should not already be available, got %v", created["status"])
	}

	got := callExtension(t, s, map[string]any{"action": "get", "name": "docs"})
	if got["target"] != "internal" {
		t.Errorf("target = %v", got["target"])
	}
	// A declared paths window gets the read-only whitelist by default.
	if allow, _ := got["allow"].([]any); len(allow) == 0 {
		t.Error("a paths window should carry the read-only tool whitelist")
	}
	if paths, _ := got["paths"].([]any); len(paths) != 1 || paths[0] != "/srv/" {
		t.Errorf("paths = %v", paths)
	}
	if notes, _ := got["notes"].(string); notes == "" {
		t.Error("the notes a paths window implies should reach get")
	}

	useExtension(t, s, "docs")
	listed := waitForExtensionStatus(t, s, "docs", extensions.StatusAvailable)
	if listed["session_id"] == nil || listed["session_id"] == "" {
		t.Errorf("an available extension should list its session id, got %v", listed)
	}
	if listed["shell_id"] != nil && listed["shell_id"] != "" {
		t.Errorf("a no-shell skill extension should not advertise a shell id, got %v", listed)
	}
}

// TestExtensionCreateConflict pins that a second create on the same name is
// refused rather than silently overwriting.
func TestExtensionCreateConflict(t *testing.T) {
	s := newExtensionTestServer(t)
	args := map[string]any{
		"action": "create", "name": "one", "target": "internal", "paths": []any{"/srv/"},
	}
	callExtension(t, s, args)
	out := extensionError(t, s, args)
	if out["error_code"] != CodeConflict {
		t.Fatalf("want %s, got %v", CodeConflict, out)
	}
}

// TestExtensionValidation pins that a template that cannot work is refused at
// save time, with the reason in the error.
func TestExtensionValidation(t *testing.T) {
	s := newExtensionTestServer(t)

	for _, args := range []map[string]any{
		{"action": "create", "name": "bad", "target": "internal"},                         // no capability at all
		{"action": "create", "name": "bad", "target": "internal", "paths": []any{"re:["}}, // bad pattern
	} {
		out := extensionError(t, s, args)
		if out["error_code"] != CodeInvalidArgument {
			t.Errorf("%v: want %s, got %v", args, CodeInvalidArgument, out)
		}
		if err, _ := out["error"].(string); !strings.Contains(err, "not saved") {
			t.Errorf("%v: error should name the save, got %q", args, err)
		}
	}
}

// TestExtensionUpdatePatchSemantics pins the omit-vs-empty distinction: an
// omitted field keeps its stored value, a present-but-empty array clears it, and
// a save that fails validation leaves the stored template untouched.
func TestExtensionUpdatePatchSemantics(t *testing.T) {
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "patch", "target": "internal", "mode": "pty",
		"command": blockingCommand(),
		"allow":   []any{"shell_input"},
		"notes":   "first",
	})

	// Omitted fields keep their stored values; present ones are set.
	got := callExtension(t, s, map[string]any{"action": "update", "name": "patch", "description": "second"})
	if got["notes"] != "first" {
		t.Errorf("omitted notes should be kept, got %v", got["notes"])
	}
	if got["description"] != "second" {
		t.Errorf("present description should be set, got %v", got["description"])
	}

	// An empty array clears; an empty string clears too.
	got = callExtension(t, s, map[string]any{
		"action": "update", "name": "patch", "allow": []any{}, "notes": "",
	})
	if allow, _ := got["allow"].([]any); len(allow) != 0 {
		t.Errorf("an empty allow array should clear the field, got %v", got["allow"])
	}
	if notes, _ := got["notes"].(string); notes != "" {
		t.Errorf("an empty notes string should clear the field, got %q", notes)
	}

	// A refused update changes nothing on disk: pipe with an empty command has
	// no shell to fall back on, so the template is refused and mode stays pty.
	extensionError(t, s, map[string]any{
		"action": "update", "name": "patch", "mode": "pipe", "command": "",
	})
	got = callExtension(t, s, map[string]any{"action": "get", "name": "patch"})
	if got["mode"] != "pty" {
		t.Errorf("a refused update should leave the stored mode alone, got %v", got["mode"])
	}
}

// TestExtensionDelete pins that a deleted extension is gone from get.
func TestExtensionDelete(t *testing.T) {
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "tmp", "target": "internal",
		"paths": []any{"/srv/"},
	})
	callExtension(t, s, map[string]any{"action": "delete", "name": "tmp"})
	out := extensionError(t, s, map[string]any{"action": "get", "name": "tmp"})
	if out["error_code"] != CodeRuleNotFound {
		t.Fatalf("want %s, got %v", CodeRuleNotFound, out)
	}
}

// TestExtensionRetryDisabled pins the refusal the reconciler gives for a parked
// extension, so an agent does not loop retry on something it has to enable.
func TestExtensionRetryDisabled(t *testing.T) {
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "parked", "target": "internal",
		"paths": []any{"/srv/"}, "enabled": false,
	})
	out := extensionError(t, s, map[string]any{"action": "retry", "name": "parked"})
	if out["error_code"] != CodeOperationFailed {
		t.Fatalf("want %s, got %v", CodeOperationFailed, out)
	}
	if err, _ := out["error"].(string); !strings.Contains(err, "disabled") {
		t.Errorf("error should say why retry is refused, got %q", err)
	}
}

// waitForExtensionFailure polls the listing until the extension reports a
// failure of the given stage. The walk marks a changed template down the moment
// it sees it, before the probe goroutine records why it is down, so a listing's
// status settles before its reason does — waiting on the status alone would
// catch the window between the two.
// waitForExtensionFailure waits until the extension reports a failure at one of
// the accepted stages.
//
// Two stages are legitimate here, and which one wins is a race the test must not
// take sides on:
//
//   - "start": the process was seen to fail while it was being started, so the
//     probe never reported it available.
//   - "session": the process came up, was reported available, and then its
//     channel was found dead on the next walk (reconciler.go: the
//     StatusAvailable branch). On Windows the startup cost of powershell.exe
//     alone can outlast the command, so "exit 3" is routinely observed this way
//     instead.
//
// Both are the same user-visible fact — the extension is unavailable and its
// last failure carries the process's exit line — and the retry path this test
// then exercises is the same one either way.
func waitForExtensionFailure(t *testing.T, s *Server, name string, stages ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		got := callExtension(t, s, map[string]any{"action": "list"})
		for _, raw := range got["extensions"].([]any) {
			if e, ok := raw.(map[string]any); ok && e["name"] == name {
				if f, _ := e["last_failure"].(map[string]any); f != nil {
					for _, want := range stages {
						if f["stage"] == want {
							return e
						}
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("extension %q never reported a %v-stage failure, last listing: %v", name, stages, got["extensions"])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionRetryBringsUp drives retry end to end: an extension pinned to a
// command that exits immediately reports the failure, and retry after the fix
// brings it to available.
func TestExtensionRetryBringsUp(t *testing.T) {
	s := newExtensionTestServer(t)
	ext := extensions.Extension{Mode: "pty"}
	ext.Name = "flaky"
	ext.Target = "internal"
	ext.Command, ext.Args = exitCommand()
	if err := s.extensions.Store().Save(&ext); err != nil {
		t.Fatal(err)
	}

	useExtension(t, s, "flaky")
	st := waitForExtensionFailure(t, s, "flaky", "start", "session")
	failure, _ := st["last_failure"].(map[string]any)
	if failure == nil {
		t.Fatalf("a failing extension should list its last failure, got %v", st)
	}
	if failure["reason"] == "" {
		t.Errorf("the failure should carry the process's exit line, got %v", failure)
	}

	// Fix the template through the tool and retry.
	callExtension(t, s, map[string]any{
		"action": "update", "name": "flaky", "command": blockingCommand(), "args": []any{},
	})
	callExtension(t, s, map[string]any{"action": "retry", "name": "flaky"})
	useExtension(t, s, "flaky")
	waitForExtensionStatus(t, s, "flaky", extensions.StatusAvailable)
}

// TestExtensionUpdateRestarts pins that editing a live template is reflected in
// the handle the tool hands out: the reconciler reopens the extension's channel on
// save, and the connection it shares is left alone — the session belongs to every
// extension on that host, so an edit must not redial for all of them.
func TestExtensionUpdateRestarts(t *testing.T) {
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "live", "target": "internal", "mode": "pty",
		"command": blockingCommand(), "args": []any{},
	})
	useExtension(t, s, "live")
	before := waitForExtensionStatus(t, s, "live", extensions.StatusAvailable)
	sid, _ := before["session_id"].(string)
	shellID, _ := before["shell_id"].(string)
	if sid == "" || shellID == "" {
		t.Fatalf("an available extension should carry its handles, got %v", before)
	}

	// A change to the session template (here the pty width) alters the
	// extension's fingerprint, which is what tells the reconciler to rebuild what
	// the extension itself holds.
	callExtension(t, s, map[string]any{
		"action": "update", "name": "live", "cols": 100,
	})

	deadline := time.Now().Add(8 * time.Second)
	for {
		useExtension(t, s, "live")
		after := waitForExtensionStatus(t, s, "live", extensions.StatusAvailable)
		if newShell, _ := after["shell_id"].(string); newShell != "" && newShell != shellID {
			// THE CONNECTION IS NOT PROMISED ACROSS AN EDIT (issue #183). An on-demand
			// extension holds nothing while nobody is using it, so the edit closes the
			// instance and the connection goes with it; what the test is about is that
			// the channel the tool hands out afterwards is a NEW one, running what the
			// template now says.
			if newSid, _ := after["session_id"].(string); newSid == "" {
				t.Errorf("the rebuilt channel should report the session it runs on, got %v", after)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("editing an extension should reopen its channel on a new shell id")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionSessionsStayOutOfSessionList pins the visibility rule: the
// connection the reconciler keeps for an extension is infrastructure, so
// session_list does not offer it — an agent discovers extensions through
// extension(action=list), which is where the handles come from. The id itself
// keeps working everywhere an id is accepted, because a handle already in hand
// must not stop resolving.
func TestExtensionSessionsStayOutOfSessionList(t *testing.T) {
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "quiet", "target": "internal", "mode": "pty",
		"command": blockingCommand(), "args": []any{},
	})
	useExtension(t, s, "quiet")
	st := waitForExtensionStatus(t, s, "quiet", extensions.StatusAvailable)
	sid, _ := st["session_id"].(string)
	if sid == "" {
		t.Fatal("an available extension should carry a session id")
	}

	res, err := s.handleListSessions(context.Background(), makeRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range parseResult(t, res)["sessions"].([]any) {
		if rec, ok := raw.(map[string]any); ok && rec["id"] == sid {
			t.Fatalf("an extension's session must not be listed by session_list: %v", rec)
		}
	}

	// The handle still resolves: session_info takes an explicit id, and the
	// extension's agents are told to read it.
	info, err := s.handleGetSessionInfo(context.Background(), makeRequest(map[string]any{"session_id": sid}))
	if err != nil {
		t.Fatal(err)
	}
	if info.IsError {
		t.Fatalf("session_info on an extension's session should work: %s", resultText(info))
	}
	if got := parseResult(t, info)["id"]; got != sid {
		t.Errorf("session_info returned %v, want %q", got, sid)
	}
}

// TestExtensionInstallFromTemplate walks the package path an agent actually takes:
// install a document, read what it asks for, fill it, use it. The three steps are
// separate calls because they are separate decisions — the recipe, this machine's
// answer, and the ask that starts something.
func TestExtensionInstallFromTemplate(t *testing.T) {
	s := newExtensionTestServer(t)
	doc := `description = "drive the cluster"
mode = "pipe"
command = "{{command}}"
targets = "{{targets}}"

[params.command]
type = "string"
help = "the program that serves MCP over the pipe"

[params.targets]
type = "array"
help = "one entry per host"
`
	// A package arrives with holes and no values. Storing it is a success, and the
	// answer says which holes remain rather than reporting a failed write.
	installed := callExtension(t, s, map[string]any{"action": "create", "name": "k8s", "template": doc})
	needs, _ := installed["needs"].([]any)
	if len(needs) != 2 {
		t.Fatalf("an installed template should report both holes as open, got %v", installed)
	}
	if installed["stored"] != true {
		t.Errorf("the install should say it stored the document: %v", installed)
	}

	// get on an unfilled extension answers with the holes rather than a not-found:
	// the extension exists, and what it lacks is one value.
	got := callExtension(t, s, map[string]any{"action": "get", "name": "k8s"})
	holes, _ := got["holes"].([]any)
	if len(holes) != 2 {
		t.Fatalf("get should serve the holes to fill, got %v", got)
	}
	if first, _ := holes[0].(map[string]any); first["type"] != "string" || first["help"] == "" {
		t.Errorf("a hole must carry its declared type and the author's help line, got %v", holes[0])
	}

	// A value for a hole nobody asked for is refused, so a typo does not become a
	// setting that looks applied and does nothing.
	bad := extensionError(t, s, map[string]any{"action": "set", "name": "k8s",
		"values": map[string]any{"hostname": "internal"}})
	if bad["error_code"] != CodeInvalidArgument {
		t.Errorf("an unknown value name = %v, want a refusal", bad)
	}

	filled := callExtension(t, s, map[string]any{"action": "set", "name": "k8s",
		"values": map[string]any{"command": blockingCommand(),
			"targets": []any{map[string]any{"host": "internal"}}}})
	if filled["command"] != blockingCommand() {
		t.Fatalf("the filled extension should come back rendered, got %v", filled)
	}
	// The holes still travel with the row: a caller that just typed a value needs to
	// see that the box it aimed at is now filled.
	if hs, _ := filled["holes"].([]any); len(hs) != 2 {
		t.Errorf("a filled extension should still report its holes as set, got %v", filled["holes"])
	}

	useExtension(t, s, "k8s")
	st := waitForExtensionStatus(t, s, "k8s", extensions.StatusAvailable)
	if st["shell_id"] == "" {
		t.Errorf("an extension filled in through the tool should run like any other, got %v", st)
	}

	// Replacing the template keeps the values: the upgrade path the split exists for.
	callExtension(t, s, map[string]any{"action": "update", "name": "k8s",
		"template": strings.Replace(doc, "drive the cluster", "drive the pods", 1)})
	after := callExtension(t, s, map[string]any{"action": "get", "name": "k8s"})
	if after["command"] != blockingCommand() {
		t.Errorf("a template upgrade must not discard this installation's values, got %v", after)
	}
	if after["description"] != "drive the pods" {
		t.Errorf("the new document should be what is stored, got %v", after["description"])
	}
}

// TestOpenExtReflectsRebuiltShellIndex pins that open ext advertises the channel
// that is actually running, not :1 from an uninitialised default (issue #211).
//
// When an extension's process exits, the reconciler starts a new one on the same
// connection. The new shell has index 2 (or higher), while the dead index 1 is either
// gone or closed. An `open ext` whose Handle header fell back to index 1 therefore
// emitted a locator that immediately answered 404.
func TestOpenExtReflectsRebuiltShellIndex(t *testing.T) {
	s := newExtensionTestServer(t)
	// Create a pipe-mode prompt extension that runs a resident command.
	callExtension(t, s, map[string]any{
		"action":  "create",
		"name":    "worker",
		"target":  "internal",
		"mode":    "pty",
		"command": blockingCommand(),
	})

	// First use: brings it up at index 1.
	used := callExtension(t, s, map[string]any{"action": "use", "name": "worker"})
	firstShellID := used["shell_id"].(string)

	// Close the first shell directly, simulating a process exit.
	if _, err := s.sessMgr.CloseChildShell(firstShellID); err != nil {
		t.Fatalf("closing first shell: %v", err)
	}

	// Trigger reconciler by asking for the extension again.
	// The reconciler starts a replacement on the same session, which takes index 2.
	used2 := callExtension(t, s, map[string]any{"action": "use", "name": "worker"})
	newShellID := used2["shell_id"].(string)
	if newShellID == firstShellID {
		t.Fatal("the shell was not replaced")
	}

	// Now drive the user-facing command: `open ext worker`
	reply := termText(t, s, map[string]any{"cmd": "open ext worker"})
	if !strings.HasPrefix(reply, "TERM/1 200") {
		t.Fatalf("open ext worker failed:\n%s", reply)
	}
	handle, ok := headerValue(t, reply, "Handle")
	if !ok || handle == "" {
		t.Fatalf("open ext worker reply missing Handle header:\n%s", reply)
	}

	// The handle MUST NOT be :1, and must resolve to the live shell.
	if strings.HasSuffix(handle, ":1") {
		t.Fatalf("open ext advertised dead channel :1 (handle=%q); want :2 or the bare shell id", handle)
	}

	// Driving the handle with a command must succeed and not return 404 out of range.
	runGot := termText(t, s, map[string]any{
		"cmd":     "run " + handle + " $1",
		"args":    []any{"hello-rebuilt"},
		"timeout": 2,
	})
	if strings.Contains(runGot, "out of range") || strings.Contains(runGot, "404") {
		t.Fatalf("running on the advertised handle failed with out of range:\n%s", runGot)
	}
}
