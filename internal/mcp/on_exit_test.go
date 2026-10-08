package mcp

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// oneShotMarker is printed by the one-shot command so the archived transcript can
// be checked for it. It is deliberately produced by an `echo`-style command that
// ends on its own -- the shape that used to leave a running tile behind.
const oneShotMarker = "ONE-SHOT-OUTPUT-MARKER"

// testOneShotCommand returns a run-to-exit command that prints oneShotMarker.
// Cross-platform: the same session_start shape must archive itself on every OS
// the CI matrix covers, so the command is spelled per platform rather than
// assuming a POSIX shell.
func testOneShotCommand() (string, []any) {
	if runtime.GOOS == "windows" {
		return testShell(), testShellArgs("-NoLogo", "-NoProfile", "-Command", "Write-Output "+oneShotMarker)
	}
	return testShell(), testShellArgs("-c", "echo "+oneShotMarker)
}

// TestOneShotSessionArchivesItselfWhenTheCommandEnds locks the fix for the
// session pile-up: an agent that starts a session only to run one command used
// to leave a "running" tile behind forever. The shell exits in milliseconds,
// nothing observes the container afterwards, and the session sat in the running
// list until the process restarted -- indistinguishable from a session someone
// is still using.
//
// on_exit="close" is the answer for that caller: the session archives itself when
// its last shell ends by itself, and its output stays readable (which is the
// whole reason the caller started it). on_exit="keep" (the default) must keep the
// old behaviour, because an interactive shell the user exits is NOT the end of
// the connection.
//
// The test drives real MCP handlers and reads the real registry, so it fails on a
// handler that accepts the argument but never acts on it.
func TestOneShotSessionArchivesItselfWhenTheCommandEnds(t *testing.T) {
	s := newTestServer(t)

	// A pipe command that ends by itself, exactly the shape an agent uses for
	// "run this and show me the output".
	cmd, args := testOneShotCommand()
	startReq := makeRequest(map[string]any{
		"ssh_config": "internal",
		"command":    cmd,
		"args":       args,
		"mode":       "pipe",
		"on_exit":    "close",
		"name":       "one-shot",
	})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	if startRes.IsError {
		t.Fatalf("session_start failed: %s", startRes.Content[0].(mcpgo.TextContent).Text)
	}
	sm := parseResult(t, startRes)
	sid, _ := sm["session_id"].(string)
	if sid == "" {
		t.Fatal("no session_id returned")
	}

	// The command ends within milliseconds; the container must follow it to DEAD.
	if !waitForExited(t, s, sid, 20*time.Second) {
		t.Fatalf("on_exit=close session %s never reached exited: a one-shot command left a running tile behind", sid)
	}

	// Archiving must not cost the caller its output: that is the point of running
	// the command. The DEAD session keeps its log readable.
	readReq := makeRequest(map[string]any{"shell_id": sid, "timeout": 1.0})
	readRes, err := s.handleReadOutput(context.Background(), readReq)
	if err != nil {
		t.Fatal(err)
	}
	if readRes.IsError {
		t.Fatalf("reading a one-shot session's output after it archived failed: %s",
			readRes.Content[0].(mcpgo.TextContent).Text)
	}
	if txt := readRes.Content[0].(mcpgo.TextContent).Text; !strings.Contains(txt, oneShotMarker) {
		t.Errorf("archived session lost its output (want %q): %s", oneShotMarker, txt)
	}
}

// TestOneShotDefaultKeepsTheContainerRunning is the counterweight: the default
// must not archive anything. A session left as "keep" is the reusable connection
// the container contract promises, and turning that into an auto-close would
// break forwards, SFTP and new shells for every caller that never asked.
func TestOneShotDefaultKeepsTheContainerRunning(t *testing.T) {
	s := newTestServer(t)

	// No on_exit argument at all: the default path.
	cmd, args := testOneShotCommand()
	startReq := makeRequest(map[string]any{
		"ssh_config": "internal",
		"command":    cmd,
		"args":       args,
		"mode":       "pipe",
		"name":       "default-keep",
	})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	sm := parseResult(t, startRes)
	sid, _ := sm["session_id"].(string)

	// Wait for the shell to exit, so the assertion is about the container staying
	// up *after* its work finished rather than about a still-starting shell.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		infoRes, _ := s.handleGetSessionInfo(context.Background(), makeRequest(map[string]any{"session_id": sid}))
		txt := infoRes.Content[0].(mcpgo.TextContent).Text
		if strings.Contains(txt, `"status":"exited"`) {
			t.Fatalf("a default (on_exit=keep) session archived itself: %s", txt)
		}
		if strings.Contains(txt, oneShotMarker) {
			break // the command already produced its output
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Give the auto-close path a chance to fire before declaring it absent.
	time.Sleep(500 * time.Millisecond)

	infoRes, _ := s.handleGetSessionInfo(context.Background(), makeRequest(map[string]any{"session_id": sid}))
	if txt := infoRes.Content[0].(mcpgo.TextContent).Text; !strings.Contains(txt, `"status":"running"`) {
		t.Fatalf("on_exit=keep must leave the container running after its shell exits, got: %s", txt)
	}
}

// TestSessionStartRejectsAnUnknownOnExit pins the validation: a typo must be
// refused, not silently treated as the default. A caller who wrote "clsoe" and
// got the keep behaviour would see the pile-up they were trying to avoid, with no
// indication why.
func TestSessionStartRejectsAnUnknownOnExit(t *testing.T) {
	s := newTestServer(t)

	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"ssh_config": "internal",
		"on_exit":    "clsoe",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("an unknown on_exit was accepted")
	}
	if txt := res.Content[0].(mcpgo.TextContent).Text; !strings.Contains(txt, "on_exit") {
		t.Errorf("the refusal does not name the offending argument: %s", txt)
	}
}

// TestOnExitIsDiscoverableOnTheWire pins the other half of the feature: a policy
// the model cannot see is one it will never use. The description budget is tight
// (tool descriptions sat 4 bytes under the limit before this change), so the
// sentence naming on_exit is exactly the kind of thing a later edit trims away to
// make room -- and the feature would then exist but stay unused.
//
// It reads tools/list, not the source: compactToolDescriptions replaces the
// description declared at registration, so asserting on tools.go would pass while
// the model saw nothing.
func TestOnExitIsDiscoverableOnTheWire(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	ctx := originContext("http://127.0.0.1:18765")

	var start, oneShotDesc string
	var enumValues []any
	for _, tool := range listTools(t, s, ctx) {
		if tool.Name != "session_start" {
			continue
		}
		start = tool.Description
		prop, ok := tool.InputSchema.Properties["on_exit"].(map[string]any)
		if !ok {
			t.Fatalf("session_start has no on_exit property; got %v", tool.InputSchema.Properties)
		}
		if d, ok := prop["description"].(string); ok {
			oneShotDesc = d
		}
		enumValues, _ = prop["enum"].([]any)
	}
	if start == "" {
		t.Fatal("session_start missing from tools/list")
	}

	// The tool description must name the argument: that is what a model reads when
	// it decides how to start a one-shot command.
	if !strings.Contains(start, "on_exit") {
		t.Errorf("session_start description never mentions on_exit:\n%s", start)
	}
	if !strings.Contains(start, `"close"`) {
		t.Errorf("session_start description mentions on_exit but not the value that fixes the pile-up:\n%s", start)
	}

	// The property carries the semantics, including that the default is the old
	// behaviour (a model must not think every session now auto-closes).
	if !strings.Contains(oneShotDesc, "keep") || !strings.Contains(oneShotDesc, "close") {
		t.Errorf("on_exit property description does not explain both values:\n%s", oneShotDesc)
	}
	if !strings.Contains(oneShotDesc, "archiv") {
		t.Errorf("on_exit property description does not say what close does:\n%s", oneShotDesc)
	}

	// The wire enum and the handler's validation must agree: an enum offering a
	// value the handler refuses is a documented-but-broken argument.
	if len(enumValues) != 2 {
		t.Fatalf("on_exit enum = %v, want keep+close", enumValues)
	}
	got := map[string]bool{}
	for _, v := range enumValues {
		if str, ok := v.(string); ok {
			got[str] = true
		}
	}
	if !got["keep"] || !got["close"] {
		t.Errorf("on_exit enum = %v, want keep+close", enumValues)
	}
}
