package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// termCall drives the single entry the way a model would, with the same JSON
// argument shape the wire carries.
func termCall(t *testing.T, s *Server, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	res, err := s.call(context.Background(), "term", args)
	if err != nil {
		t.Fatalf("term(%v): %v", args, err)
	}
	return res
}

// termText is a term call's text projection.
func termText(t *testing.T, s *Server, args map[string]any) string {
	t.Helper()
	return toolResultText(termCall(t, s, args))
}

// termTextWithCtx runs a term call on a caller-supplied context, which is how a test
// supplies the client session that sampling forwarding looks for.
func termTextWithCtx(t *testing.T, s *Server, ctx context.Context, args map[string]any) string {
	t.Helper()
	res, err := s.call(ctx, "term", args)
	if err != nil {
		t.Fatalf("term(%v): %v", args, err)
	}
	return toolResultText(res)
}

// headerValue reads one header out of a rendered slot. It is deliberately a
// text-level helper: the point of these tests is what a caller READS, and both
// projections come from one value.
func headerValue(t *testing.T, text, key string) (string, bool) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v, true
		}
	}
	return "", false
}

// headerValueOr is headerValue without a test handle, for helpers that need a
// header inside an expression.
func headerValueOr(text, key, def string) string {
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v
		}
	}
	return def
}

// handleFrom reads the Handle header, which is how a caller learns a new channel.
func handleFrom(t *testing.T, text string) string {
	t.Helper()
	h, ok := headerValue(t, text, "Handle")
	if !ok {
		t.Fatalf("no Handle header in:\n%s", text)
	}
	return h
}

// A fresh instance answers the empty call. This is the first move of every
// recovery, so it must be legal, cheap, and dense — never an error.
func TestTermEmptyCallIsTheGuide(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{})
	if res.IsError {
		t.Fatalf("an empty cmd must not be an error:\n%s", toolResultText(res))
	}
	text := toolResultText(res)
	if !strings.HasPrefix(text, "TERM/1 200 OK\n") {
		t.Errorf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	// It must say what this is, where capability lives, where state lives, and what
	// is running right now.
	for _, want := range []string{"cmd = the DSL", "sh = a shell command", "ls ext", "ls", "help", "running now"} {
		if !strings.Contains(text, want) {
			t.Errorf("the guide misses %q:\n%s", want, text)
		}
	}
	// And it must not require remembering anything: an address is obtained, not
	// supplied.
	if !strings.Contains(text, "from `ls`") {
		t.Errorf("the guide must point at where channel addresses come from:\n%s", text)
	}
}

// help lists the directives; help <op> teaches one. This is the "I have not used
// those yet" half of progressive disclosure; learn_mode is the "I just used it
// wrong" half.
func TestTermHelp(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "help"})
	for _, op := range []string{"run", "read", "ls", "open", "connect", "create", "help"} {
		if !strings.Contains(text, "\n"+op+" ") && !strings.Contains(text, "\n"+op+"\n") {
			t.Errorf("help misses directive %q:\n%s", op, text)
		}
	}
	// The one-line template comes FIRST in the body and is not replaced by the page:
	// a caller scanning for shape must still find it before it has to read prose.
	// The page itself is allowed to be long -- most directives have one, and `run`
	// joined them when it gained the pty-completion explanation. Asserting a fixed
	// line count here only pinned that `run` happened to be the one without a page.
	single := termText(t, s, map[string]any{"cmd": "help run"})
	body := single
	if _, rest, ok := strings.Cut(single, "\n\n"); ok {
		body = rest // skip the envelope: status line and headers
	}
	// The prefix, not the whole line: the invariant is that the USAGE TEMPLATE comes first,
	// and the template's own spelling is pinned by the generated-docs test. Pinning it here
	// too made this test fail on a flag being added to `run` — a change it has nothing to
	// say about.
	if first, _, _ := strings.Cut(body, "\n"); !strings.HasPrefix(first, "run <channel>") {
		t.Errorf("the body does not open with the usage template:\n%s", single)
	}
}

// An unknown directive is a 400 that TEACHES: the failure is exactly the moment
// the caller does not know the shape, so the usage line rides along whether or
// not learn_mode was asked for.
func TestTermUnknownDirectiveTeaches(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{"cmd": "raed a1f3:1"})
	if !res.IsError {
		t.Fatal("an unknown directive must be an error")
	}
	text := toolResultText(res)
	if !strings.HasPrefix(text, "TERM/1 400 Bad Request\n") {
		t.Errorf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	if code, _ := headerValue(t, text, "Error-Code"); code != "unknown_directive" {
		t.Errorf("Error-Code = %q", code)
	}
	if _, ok := headerValue(t, text, "Usage"); !ok {
		t.Errorf("a 400 must carry a Usage line:\n%s", text)
	}
}

// A successful call NEVER carries Usage without learn_mode: teaching on every
// successful reply would be noise the caller cannot turn off.
func TestTermSuccessIsSilentWithoutLearnMode(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "ls"})
	if _, ok := headerValue(t, text, "Usage"); ok {
		t.Errorf("a successful reply carried Usage without learn_mode:\n%s", text)
	}
	// A successful reply must also not carry the constant fields.
	for _, key := range []string{"Idempotent", "Status", "Exit-Code", "Error-Code"} {
		if _, ok := headerValue(t, text, key); ok {
			t.Errorf("a plain successful reply carried %s, which is constant:\n%s", key, text)
		}
	}
}

// learn_mode attaches the usage line, once per directive even across a batch.
func TestTermLearnModeDeduplicatesUsage(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")

	text := termText(t, s, map[string]any{
		"cmd":        "read " + h + "; read " + h,
		"learn_mode": true,
		"timeout":    float64(0),
	})
	if n := strings.Count(text, "Usage: "); n != 1 {
		t.Errorf("Usage appears %d times for two slots of one directive, want 1:\n%s", n, text)
	}
}

// A fresh pipe channel hands back a handle, which is news by definition.
func TestTermPipeReturnsHandle(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "connect internal --argv sleep 5"})
	if !strings.HasPrefix(text, "TERM/1 200 OK\n") {
		t.Fatalf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	h := handleFrom(t, text)
	if h == "" {
		t.Fatal("empty handle")
	}
	// The handle must be USABLE, and it must be the short form.
	if strings.Contains(h, "termcp://") {
		t.Errorf("Handle = %q, want the short form (the termcp:// prefix costs 9 chars per slot)", h)
	}
	if info := termText(t, s, map[string]any{"cmd": "get " + h}); !strings.HasPrefix(info, "TERM/1 200 OK") {
		t.Errorf("the returned handle does not resolve: %s", info)
	}
}

// --argv is a word list: every word after it reaches the program, quoting groups
// an argument, and a flag after it is refused rather than fed to the program.
func TestTermArgvCarriesItsWords(t *testing.T) {
	s := newTestServer(t)
	for _, cmd := range []string{
		"connect internal --argv echo hello",
		"connect internal --argv sh -c 'echo hello'",
		"connect internal --argv sh -c \"echo hello\"", // a quoted argument keeps its space
	} {
		text := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(text, "TERM/1 200 OK") {
			t.Fatalf("%s:\n%s", cmd, text)
		}
		h := handleFrom(t, text)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, closed := s.channelExitState(h); closed {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		out := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
		if !strings.Contains(out, "hello") {
			t.Errorf("%s produced no hello:\n%s", cmd, out)
		}
	}

	// A known flag after --argv would be handed to the program if accepted.
	res := termCall(t, s, map[string]any{"cmd": "connect internal --argv echo hi --name chat"})
	if !res.IsError || !strings.Contains(toolResultText(res), "flags come before --argv") {
		t.Errorf("a flag after --argv was not refused:\n%s", toolResultText(res))
	}
	// A bare payload used to be dropped without a word.
	res = termCall(t, s, map[string]any{"cmd": "connect internal sleep 5"})
	if !res.IsError || !strings.Contains(toolResultText(res), "--argv") {
		t.Errorf("a bare payload was not refused:\n%s", toolResultText(res))
	}
}

// The MODE follows ssh's own rule: a command means no terminal, no command means a
// terminal. The claim that matters is not which word produced which mode, but that
// the reply says so — a caller reading "pipe" knows an exit code is coming and a
// caller reading "pty" knows it is not.
func TestTermModeFollowsTheArgv(t *testing.T) {
	s := newTestServer(t)
	pipeText := termText(t, s, map[string]any{"cmd": "connect internal --argv sleep 5"})
	if !strings.Contains(pipeText, "pipe") {
		t.Errorf("a command should have made a pipe, got %q", pipeText)
	}
	ptyText := termText(t, s, map[string]any{"cmd": "connect internal"})
	if !strings.Contains(ptyText, "pty") {
		t.Errorf("no command should have made a pty, got %q", ptyText)
	}
	// And --pty overrides the rule, so the two are not simply welded together.
	forced := termText(t, s, map[string]any{"cmd": "connect internal --argv sleep 5 --pty"})
	if !strings.Contains(forced, "pty") {
		t.Errorf("--pty did not force a terminal, got %q", forced)
	}
}

// A pty has no completion signal, so `run` samples the caller's time window and
// returns what the terminal produced in it.
func TestTermRunOnPtyReadsTheWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	start := time.Now()
	text := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo hi"},
		"timeout": float64(2),
	})
	elapsed := time.Since(start)
	if !strings.Contains(text, "hi") {
		t.Errorf("the command's output is missing:\n%s", text)
	}
	if elapsed > 5*time.Second {
		t.Errorf("run took %s; the caller's window is 2s", elapsed)
	}
	// A live pty channel has no exit code, because a shell does not report that a
	// command inside it finished.
	if _, ok := headerValue(t, text, "Exit-Code"); ok {
		t.Errorf("a running channel reported an Exit-Code:\n%s", text)
	}
}

// A pty has no completion signal, so `run` must wait the caller's time window
// rather than read the instant after typing. A command that prints after a
// delay proves the wait happened — and a session-level handle must resolve to
// the channel, or the wait branch is skipped entirely.
func TestTermPtyRunWaitsTheWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	awaitPtyReady(t, s, h)
	text := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sleep 1 && echo LATE"},
		"timeout": float64(3),
	})
	if !strings.Contains(text, "LATE") {
		t.Fatalf("run returned before the command's output:\n%s", text)
	}
}

// `run` reads from where it submitted, so a previous command's unread tail is not
// delivered as if it belonged to this one.
func TestTermRunStartsAtItsOwnOffset(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	awaitPtyReady(t, s, h)
	// THE MARKER IS HOW "seq FINISHED" IS KNOWN, and a quiet heuristic is not.
	//
	// The 64-byte window deliberately leaves most of seq's output unread — that is the
	// point of the test — but the WRITER is still going when this call returns. The wait
	// used to be "the buffer has not grown for 50ms", which a chunked writer satisfies in
	// the middle of its own output: the second body then carried seq's remaining lines,
	// and this test reported them as a replay. A marker printed AFTER seq is an end, not a
	// pause.
	// TWO THINGS HAVE TO TRAVEL AS THE VALUE, and both are about the marker meaning what
	// it says.
	//
	// The `;` is the DSL's statement separator, so the same text written into cmd is two
	// statements and the second is not a directive.
	//
	// And the marker is BUILT by the command (`seq-finished-$(echo marker)`) rather than
	// written in it: a wait that greps the transcript for a literal string matches the
	// ECHO of the line that contains it, which returns before the command has run — the
	// wait would then be measuring nothing, which is how this test spent a while passing
	// for the wrong reason.
	termText(t, s, map[string]any{
		"cmd":         "run " + h + " $1",
		"args":        []any{"seq 1 60; echo seq-finished-$(echo marker)"},
		"timeout":     float64(2),
		"bytes_limit": float64(64),
	})
	waitForContent(t, s, h, "seq-finished-marker")

	second := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo SECOND"}, "timeout": float64(2)})
	if !strings.Contains(second, "SECOND") {
		t.Fatalf("the second command's output is missing:\n%s", second)
	}
	// The residual of the first command must not be in the second body.
	if strings.Contains(second, "\n1\n2\n3") {
		t.Errorf("the second run replayed the first command's unread tail:\n%s", second)
	}
}

// A closed channel's terminal state is reported on every reply about it,
// consistently — and a non-zero exit makes each of those replies a failure.
func TestTermExitCodeIsAFunctionOfTheChannel(t *testing.T) {
	s := newTestServer(t)
	script := writeScript(t, "exit 3")
	text := termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"})
	h := handleFrom(t, text)
	// Wait for the process to end.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _, closed := s.channelExitState(h); closed && st == "exited" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	first := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if st, _ := headerValue(t, first, "Status"); st != "exited" {
		t.Fatalf("Status = %q, want exited:\n%s", st, first)
	}
	if _, ok := headerValue(t, first, "Exit-Code"); !ok {
		t.Fatalf("a closed channel must report its exit code:\n%s", first)
	}
	// Same answer on the next read: the status is state, not an event.
	second := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if st1, _ := headerValue(t, first, "Status"); st1 != "exited" {
		t.Errorf("first read Status = %q", st1)
	}
	if st2, _ := headerValue(t, second, "Status"); st2 != "exited" {
		t.Errorf("second read Status = %q, want the same answer as the first", st2)
	}
}

// A read of a closed channel whose process failed is a 200 that still carries the
// bytes, the range and the exit code.
//
// IT USED TO BE A 500, and that was wrong in a way the caller could not see past:
// the read itself succeeded — it delivered every byte — while the status said the
// call failed, so any OTHER statement in the same cmd was reported as failed too
// (isError is a property of the whole call). The exit code is a fact about the
// program in the transcript, not about the read, so it travels in Exit-Code and
// Error-Code: child_exit while the code stays 200.
func TestTermFailureKeepsTheBytes(t *testing.T) {
	s := newTestServer(t)
	// A multi-statement program is a script file, not shell syntax in --argv.
	script := writeScript(t, "echo boom >&2; exit 3")
	text := termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"})
	h := handleFrom(t, text)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, closed := s.channelExitState(h); closed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res := termCall(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	out := toolResultText(res)
	if !strings.HasPrefix(out, "TERM/1 200 OK\n") {
		t.Fatalf("status line = %q:\n%s", strings.SplitN(out, "\n", 2)[0], out)
	}
	if res.IsError {
		t.Error("a read that delivered its bytes is not a failed call")
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("the failing command's stderr was dropped:\n%s", out)
	}
	if _, ok := headerValue(t, out, "Content-Range"); !ok {
		t.Errorf("a failed channel must still say where to continue reading:\n%s", out)
	}
	if code, _ := headerValue(t, out, "Exit-Code"); code != "3" {
		t.Errorf("Exit-Code = %q, want 3:\n%s", code, out)
	}
}

// A non-zero pipe exit is a command result, not a failed TERM request. The slot
// keeps Error-Code/Exit-Code for callers, but remains 2xx so later statements and
// HTTP clients do not mistake the successful execution/read for a server failure.
func TestPipeRunNonzeroExitIsSuccessfulRequestAndDoesNotShortCircuit(t *testing.T) {
	s := newTestServer(t)
	// A pipe shell reads stdin so this channel stays alive for more than one `run` —
	// the case at issue, unlike a one-shot --argv program. `sh` with no arguments is
	// such a shell on every platform this test runs on, and it writes nothing but the
	// caller's own bytes (cmd.exe would echo a GBK codepage banner first).
	connected := termText(t, s, map[string]any{"cmd": "connect internal --argv sh"})
	handle := handleFrom(t, connected)
	failed := termCall(t, s, map[string]any{
		"cmd":     "run " + handle + " $1 && help",
		"args":    []any{"exit 4"},
		"timeout": float64(5),
	})
	out := toolResultText(failed)
	if !strings.HasPrefix(out, "TERM/1 2") {
		t.Fatalf("command exit must not fail the TERM request:\n%s", out)
	}
	if failed.IsError {
		t.Fatalf("nonzero child exit is a failing slot fact, not an MCP call error:\n%s", out)
	}
	if code, _ := headerValue(t, out, "Exit-Code"); code != "4" {
		t.Errorf("Exit-Code = %q, want 4:\n%s", code, out)
	}
	if code, _ := headerValue(t, out, "Error-Code"); code != "command_failed" {
		t.Errorf("Error-Code = %q, want command_failed:\n%s", code, out)
	}

	if !strings.Contains(out, "Path: help") {
		t.Fatalf("`&& help` must execute after a non-zero child exit because the TERM request succeeded:\n%s", out)
	}
	_ = termCall(t, s, map[string]any{"cmd": "close " + handle})
}

// ps is terminal text, one line per channel, and every handle in it is usable
// verbatim as another directive's target.
func TestTermPSLinesAreCopyable(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "ls"})
	if strings.Contains(text, "{") || strings.Contains(text, "[") {
		t.Errorf("ls must be terminal columns, not JSON:\n%s", text)
	}
	h := openPipe(t, s, "sleep 5")
	text = termText(t, s, map[string]any{"cmd": "ls"})
	if !strings.Contains(text, h) {
		t.Fatalf("ls does not list %s:\n%s", h, text)
	}
	// The format requirement that matters: copy a column into the next command.
	body := text[strings.Index(text, "\n\n")+2:]
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// THE STATUS LINE, NOT THE WORD "404". A channel id is a uuid, and one that
		// begins with those three digits made this fail about once in four thousand
		// runs — a flake whose cause was a substring match, not a race. The reply's
		// first line is the status, which is the thing being asked about.
		probe := termText(t, s, map[string]any{"cmd": "get " + fields[0]})
		if !strings.HasPrefix(probe, "TERM/1 2") {
			t.Errorf("the channel %q from ls does not resolve:\n%s", fields[0], probe)
		}
	}
}

// info is `key: value`, one fact per line.
//
// The two ADDRESSES are two different nodes and answer about different things: a bare
// session id is the connection container, and `<id>:<index>` is one channel on it
// (issue #142 — the channel address used to return the session record byte for byte,
// so a mistyped index was indistinguishable from a hit).
func TestTermInfoIsKeyValueLines(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")
	sid := strings.SplitN(h, ":", 2)[0]

	// The session: what the link is, and which channels hang off it.
	sess := termText(t, s, map[string]any{"cmd": "get " + sid})
	for _, key := range []string{"id: ", "status: ", "ssh_config: ", "shell: "} {
		if !strings.Contains(sess, key) {
			t.Errorf("get <session> misses %q:\n%s", key, sess)
		}
	}
	if strings.Contains(sess, "{") {
		t.Errorf("get must be key: value lines, not JSON:\n%s", sess)
	}

	// The channel: the facts about THIS channel, which the session record never had.
	chanInfo := termText(t, s, map[string]any{"cmd": "get " + h})
	for _, key := range []string{"channel: " + h, "index: ", "mode: pipe", "status: ", "bytes: "} {
		if !strings.Contains(chanInfo, key) {
			t.Errorf("get <channel> misses %q:\n%s", key, chanInfo)
		}
	}
	// A pipe has no terminal, so it must not be given a size.
	if strings.Contains(chanInfo, "size: ") {
		t.Errorf("a pipe channel was given a terminal size:\n%s", chanInfo)
	}
}

// Every window parameter applies to every directive, not just the byte streams:
// a generated reply is exactly as capable of flooding a context as a log.
func TestTermWindowAppliesToGeneratedReplies(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 6; i++ {
		openPipe(t, s, "sleep 5")
	}
	text := termText(t, s, map[string]any{"cmd": "ls", "lines_limit": float64(2), "bytes_limit": float64(80)})
	body := text[strings.Index(text, "\n\n")+2:]
	if len(body) > 200 {
		t.Errorf("ls ignored the window (%d bytes):\n%s", len(body), text)
	}
}

// The total byte cap is a property of the reply, not of a slot.
func TestTermTotalBytesLimitAcrossSlots(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "seq 1 5000")
	time.Sleep(300 * time.Millisecond)
	cmd := "read " + h + "; read " + h + "; read " + h
	res := termCall(t, s, map[string]any{"cmd": cmd, "total_bytes_limit": float64(64), "timeout": float64(1)})
	text := toolResultText(res)

	// The cap counts BODY bytes across slots, which is what the caller pays for.
	// The text projection is the honest measure: each slot's body is everything
	// after its blank line, and the next status line belongs to the next slot.
	parts := strings.Split(text, "TERM/1 ")[1:]
	bodies := 0
	for i, part := range parts {
		j := strings.Index(part, "\n\n")
		if j < 0 {
			continue
		}
		body := part[j+2:]
		if i+1 < len(parts) {
			// Text joins responses with one newline; that byte is the separator
			// between two responses, not part of either body.
			body = strings.TrimSuffix(body, "\n")
		}
		bodies += len(body)
	}
	if bodies > 64 {
		t.Errorf("the total cap was exceeded: %d body bytes > 64:\n%s", bodies, text)
	}
	// The slot that crossed the cap still says where to continue, and the range
	// describes the bytes it actually returned.
	if !strings.Contains(text, "Content-Range: bytes 0-63/") {
		t.Errorf("the truncated slot's range does not describe what it returned:\n%s", text)
	}
	// Truncation is not failure: a capped reply is still valid.
	if res.IsError {
		t.Errorf("a capped reply must not be an error:\n%s", text)
	}
}

// bytes_limit: 0 hands back the handle and consumes nothing, so a JSON-RPC frame
// on a pipe stays there for `mcp recv`.
func TestTermBytesLimitZeroConsumesNothing(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "seq 1 100")
	text := termText(t, s, map[string]any{"cmd": "read " + h, "bytes_limit": float64(0), "timeout": float64(0)})
	if !strings.Contains(text, "TERM/1 204 No Content") {
		t.Fatalf("bytes_limit 0 must return no bytes:\n%s", text)
	}
	after := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(1)})
	if !strings.Contains(after, "1\n2\n3") {
		t.Errorf("a bytes_limit 0 read consumed the stream; the bytes are gone:\n%s", after)
	}
}

// TestMCPRecvDeliversTheFrameAndDoesNotRepeatIt is the regression test for a recv
// that could never return a frame.
//
// mcpRecvFrame passed the channel's current byte LENGTH as shell_output's offset.
// That parameter is an absolute position, so it asked for the bytes just past the
// end: recv answered "no complete frame yet" forever while the frame sat in the
// channel, and `read` on the same handle showed it plainly. Two things are
// asserted here because the old shape also made a second recv plausible as a fix:
// the frame arrives, and a second recv does not deliver it again.
func TestMCPRecvDeliversTheFrameAndDoesNotRepeatIt(t *testing.T) {
	s := newTestServer(t)
	// cat echoes the frame back on the channel's own stdout, which is where recv
	// reads. A one-shot producer would exit before the second read.
	h := openPipe(t, s, "cat")

	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if out := termText(t, s, map[string]any{"cmd": "call " + h + " --send " + frame}); !statusLineIs(out, "202") {
		t.Fatalf("write did not accept the frame:\n%s", out)
	}

	// The peer is a process; poll a few times rather than assuming it has written.
	var got string
	for i := 0; i < 20; i++ {
		got = termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
		if strings.Contains(got, `"method":"tools/list"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(got, `"method":"tools/list"`) {
		t.Fatalf("read never delivered a frame that was on the channel:\n%s", got)
	}

	twice := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if strings.Contains(twice, `"method":"tools/list"`) {
		t.Errorf("a second recv re-delivered the same frame; the cursor did not advance:\n%s", twice)
	}
}

// TestMCPRecvCursorIsDroppedOnClose pins that a closed handle does not keep a
// read position.
//
// Handles are minted monotonically (jrpcHandleSeq), so close-and-reopen never
// reuses a key: without the delete, the cursor map grows by one entry per channel
// the process ever opened and nothing reclaims them. An unnoticed leak in a
// long-lived instance is the worst kind, because the workaround is a restart.
func TestMCPRecvCursorIsDroppedOnClose(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)

	// THE CURSOR IS SET DIRECTLY, because the invariant is about close, not about how
	// a frame was obtained. Producing a real frame was platform-dependent in a way
	// that cost two CI rounds: spawning `echo '{…}'` fails under powershell, and typing
	// it at a "pty" assumes a terminal that echoes, which Windows does not always give.
	// The frame-reading half is covered by TestMCPRecvCursorUsesRawByteSpan.
	s.setRecvCursor(h, 7)

	if out := termText(t, s, map[string]any{"cmd": "close " + h}); !statusLineIs(out, "204") {
		t.Fatalf("close: %s", out)
	}
	if _, ok := s.recvCursors.Load(h); ok {
		t.Errorf("handle %s still has a recv cursor after close; the map grows for the life of the process", h)
	}
}

// TestMCPRecvCursorUsesRawByteSpan pins that the cursor advances by the RAW
// window length, not by the length of the rendered string.
//
// windowResult reports raw_bytes alongside the text precisely for this: ANSI
// stripping and escaping change a window's rendered length, so an offset derived
// from the text lands somewhere other than where the next frame begins. The
// steady-state symptom is the one this whole fix exists to remove — a frame that
// is present but reported as absent, because the read resumed past it.
func TestMCPRecvCursorUsesRawByteSpan(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")

	frame := `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	if out := termText(t, s, map[string]any{"cmd": "call " + h + " --send " + frame}); !statusLineIs(out, "202") {
		t.Fatalf("mcp send: %s", out)
	}

	var got string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got = termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
		if strings.Contains(got, `"id":7`) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(got, `"id":7`) {
		t.Fatalf("recv never delivered the frame: %s", got)
	}

	// The cursor must sit at or before the stream length, and the next recv must
	// not re-deliver this frame.
	next := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if strings.Contains(next, `"id":7`) {
		t.Errorf("the frame was delivered twice; the cursor did not advance past it: %s", next)
	}
}

// TestMCPRecvDrainsEveryQueuedFrame pins that recv is a QUEUE, not a snapshot.
//
// A resident peer answers every request it has been given, so by the time the
// caller reads, several complete frames are sitting on the channel at once. That
// is the normal case for an MCP conversation: initialize, then initialized, then
// tools/list are three lines written back to back, and the replies are waiting.
//
// The bug this pins: recv read a WINDOW (max_bytes) and reported 204 whenever the
// window held no complete frame, even with frames queued behind it. It looked
// exactly like a lost message — a caller that had written three requests was told
// "no complete frame yet" while all three replies were in the channel. The
// workaround was to abandon recv and page the raw stream by hand, which is the
// behaviour this test makes unnecessary.
func TestMCPRecvDrainsEveryQueuedFrame(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")

	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":3,"method":"notifications/initialized"}`,
	}
	for _, f := range frames {
		if out := termText(t, s, map[string]any{"cmd": "call " + h + " --send " + f}); !statusLineIs(out, "202") {
			t.Fatalf("write did not accept %s:\n%s", f, out)
		}
	}

	// Every queued frame must come out of recv, in order, with no 204 in between.
	// The loop polls rather than sleeping a fixed time: the peer is a process, so
	// how soon its writes land is not the test's to assume. A 204 is retried
	// instead of ending the loop, because "not there YET" and "never" are exactly
	// the two cases the old code could not tell apart.
	want := []string{`"id":1`, `"id":2`, `"id":3`}
	var seen []string
	deadline := time.Now().Add(3 * time.Second)
	for len(seen) < len(want) && time.Now().Before(deadline) {
		got := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
		if strings.Contains(got, "no complete frame yet") {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		for _, w := range want {
			if strings.Contains(got, w) && !containsStr(seen, w) {
				seen = append(seen, w)
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("recv delivered %d of %d queued frames (%v); frames were waiting while recv reported 204",
			len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("recv delivered frames out of order: got %v, want %v", seen, want)
			break
		}
	}

	// And it is a consumer, not a snapshot: with the queue drained, the next recv
	// has nothing to give rather than replaying the three frames again.
	after := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(0)})
	if strings.Contains(after, `"jsonrpc"`) {
		t.Errorf("recv re-delivered a frame after the queue drained:\n%s", after)
	}
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// A batch target produces one slot per entry: N questions, N answers.
func TestTermBatchProducesOneSlotPerTarget(t *testing.T) {
	s := newTestServer(t)
	a := openPipe(t, s, "sleep 5")
	b := openPipe(t, s, "sleep 5")
	text := termText(t, s, map[string]any{
		"cmd":     "read " + a + "," + b,
		"timeout": float64(0),
	})
	if n := strings.Count(text, "TERM/1 "); n != 2 {
		t.Errorf("two targets produced %d slots:\n%s", n, text)
	}
}

// The structured projection mirrors the text one, slot for slot, key for key.
//
// It is requested explicitly because it is off by default: the two projections
// carry the same bytes, so the JSON twin is a duplicate a caller opts into. The
// switch is about the WIRE, not about the value — the reply builds both either
// way, which is what this test can therefore check on one call.
func TestTermStructuredContentIsIsomorphic(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")
	res := termCall(t, s, map[string]any{
		"cmd":                "read " + h + "; read " + h,
		"timeout":            float64(0),
		"structured_content": true,
	})

	if res.StructuredContent == nil {
		t.Fatal("structured_content:true produced no structuredContent")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var slots []map[string]any
	var one map[string]any
	if err := json.Unmarshal(raw, &one); err != nil {
		t.Fatalf("structuredContent is not an object: %s", raw)
	}
	if wrapped, ok := one["slots"].([]any); ok {
		for _, item := range wrapped {
			slot, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("structuredContent slots contain a non-object: %s", raw)
			}
			slots = append(slots, slot)
		}
	} else {
		slots = []map[string]any{one}
	}

	texts := strings.Split(toolResultText(res), "TERM/1 ")[1:]
	if len(texts) != len(slots) {
		t.Fatalf("%d text slots vs %d structured slots", len(texts), len(slots))
	}
	for i, slot := range slots {
		for _, key := range []string{"protocol", "code", "reason"} {
			if _, ok := slot[key]; !ok {
				t.Errorf("slot %d misses the structured status-line key %q", i, key)
			}
		}
		if slot["protocol"] != "TERM/1" {
			t.Errorf("slot %d protocol = %v", i, slot["protocol"])
		}
		// Every header key in the text must exist in the structured twin.
		for _, line := range strings.Split("TERM/1 "+texts[i], "\n")[1:] {
			if line == "" {
				break
			}
			key, _, ok := strings.Cut(line, ": ")
			if !ok {
				continue
			}
			if _, ok := slot[key]; !ok {
				t.Errorf("slot %d: text has header %q that the structured twin lacks: %v", i, key, slot)
			}
		}
		// And the body travels in both projections: a client that renders only
		// structuredContent must still see it. Reply.Text joins slots with one
		// newline, so a non-last segment carries that separator extra.
		seg := "TERM/1 " + texts[i]
		if i < len(texts)-1 {
			seg = strings.TrimSuffix(seg, "\n")
		}
		body := ""
		if parts := strings.SplitN(seg, "\n\n", 2); len(parts) == 2 {
			body = parts[1]
		}
		if slot["body"] != body {
			t.Errorf("slot %d structured body = %v, want %q", i, slot["body"], body)
		}
	}
}

// The alias directive names a directive, and the expansion is visible in Path so
// a caller sees what actually ran.
func TestTermAliasExpands(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")
	if res := termCall(t, s, map[string]any{"cmd": "create alias look read " + h}); res.IsError {
		t.Fatalf("alias failed:\n%s", toolResultText(res))
	}
	text := termText(t, s, map[string]any{"cmd": "look", "timeout": float64(0)})
	path, ok := headerValue(t, text, "Path")
	if !ok {
		t.Fatalf("a reply always carries Path:\n%s", text)
	}
	if !strings.Contains(path, "read "+h) {
		t.Errorf("Path = %q, want the EXPANDED directive", path)
	}
	list := termText(t, s, map[string]any{"cmd": "ls alias"})
	if !strings.Contains(list, "look = read") {
		t.Errorf("alias list = %q", list)
	}
}

// `file` maps onto the existing file tools, one per action, and a read is never
// reviewed while a write is.
func TestTermFileDirective(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "get nosuch file /etc/hostname"})
	// No session named: the failure must be a NOT FOUND (404), not a crash.
	if !strings.Contains(text, "TERM/1 ") {
		t.Fatalf("no envelope:\n%s", text)
	}
	// An op the SFTP layer does not know must not be answered as if it worked. With
	// the resource tree the first refusal may be the missing session rather than the
	// unknown op, so any 4xx is the contract; a 2xx would be the bug.
	unknown := termText(t, s, map[string]any{"cmd": "get nosuch file /x --op nope"})
	if !strings.Contains(unknown, "TERM/1 4") {
		t.Errorf("an unknown file action was not refused:\n%s", unknown)
	}
}

// The three collections are all listed by one verb.
func TestTermFwdAndProfileDirectives(t *testing.T) {
	s := newTestServer(t)
	fwd := termText(t, s, map[string]any{"cmd": "ls forward"})
	if !strings.Contains(fwd, "TERM/1 200 OK") {
		t.Errorf("ls  = %q", fwd)
	}
	prof := termText(t, s, map[string]any{"cmd": "ls host"})
	if !strings.Contains(prof, "TERM/1 200 OK") {
		t.Errorf("ls host = %q", prof)
	}
	ext := termText(t, s, map[string]any{"cmd": "ls ext"})
	// The test server has no extension reconciler, so the honest answer is
	// not_configured (422) — what matters is that the directive REACHES the
	// extension layer and reports its state instead of failing to translate.
	if !strings.Contains(ext, "TERM/1 422") || !strings.Contains(ext, "not_configured") {
		t.Errorf("ls ext = %q", ext)
	}
}

// A payload reaches the shell byte for byte, including the constructs the parser
// must not touch.
func TestTermPayloadIsNotRewritten(t *testing.T) {
	s := newTestServer(t)
	// A POSIX shell is named explicitly: the payload rule is about the bytes
	// reaching the shell, not about which shell the profile happens to default to.
	script := writeScript(t, `echo $((6*7)); echo "a  b"`)
	h := openPipe(t, s, "sh '"+script+"'")
	// Read the whole stream, not a window of it: this test is about the bytes that
	// reached the shell, and a window would only prove they appear early.
	text := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		text += termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(2)})
		// Only the expected bytes end the loop: "Status: exited" can appear on a
		// window that raced the exit, while the final line is still unread.
		if strings.Contains(text, "a  b") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(text, "42") {
		// A login shell may not have finished starting; read the window it produced.
		time.Sleep(500 * time.Millisecond)
		text = termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(5)})
	}
	if !strings.Contains(text, "42") {
		t.Errorf("the shell did not expand the arithmetic, so the payload was rewritten:\n%s", text)
	}
	if !strings.Contains(text, "a  b") {
		t.Errorf("the quoted whitespace was not preserved:\n%s", text)
	}
	// The script is what the shell saw, so the expansion proves nothing was
	// rewritten on the way in.
	path, _ := headerValue(t, text, "Path")
	if !strings.Contains(path, h) {
		t.Errorf("Path = %q, want the channel", path)
	}
}

// The single entry is one tool, and its schema is the budget it must fit in.
func TestTermIsTheOnlyEntryAndFitsItsBudget(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	var term *mcpgo.Tool
	tools := listTools(t, s, originContext("http://127.0.0.1:18765"))
	for i := range tools {
		if tools[i].Name == termToolName {
			term = &tools[i]
		}
	}
	if term == nil {
		t.Fatalf("%q is not registered", termToolName)
	}
	raw, err := json.Marshal(term)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 2500
	if len(raw) >= budget {
		t.Errorf("the term schema is %d bytes, over the %d-byte budget: %s", len(raw), budget, raw)
	}
	// The description must not enumerate directives: the semantics live in the
	// command syntax and in help/learn_mode, which is what lets the surface grow
	// without the tool growing.
	for _, op := range []string{"shell_input", "file_read", "session_start", "pipe", "pty"} {
		if strings.Contains(term.Description, op) {
			t.Errorf("the description names %q; directives belong in cmd's syntax, not the tool blurb:\n%s", op, term.Description)
		}
	}
	// cmd must not be required: the empty call is the entry point.
	if props := term.InputSchema.Properties; props == nil {
		t.Error("no properties")
	}
	for _, name := range term.InputSchema.Required {
		if name == "cmd" {
			t.Error("cmd must not be required: the empty call is how a caller gets its bearings")
		}
	}
}

// Generated replies honor the same space windows as streams: zero bytes means
// no bytes, a line window truncates with 206, and an invalid overflow policy is
// an argument error rather than a silent default.
func TestTermGeneratedRepliesHonorSpaceWindows(t *testing.T) {
	s := newTestServer(t)
	empty := termText(t, s, map[string]any{"cmd": "help", "bytes_limit": float64(0)})
	if !strings.HasPrefix(empty, "TERM/1 204 No Content") {
		t.Fatalf("bytes_limit=0 on help = %q", strings.SplitN(empty, "\n", 2)[0])
	}
	one := termText(t, s, map[string]any{"cmd": "help", "lines_limit": float64(1)})
	if !strings.HasPrefix(one, "TERM/1 206 Partial Content") {
		t.Fatalf("lines_limit=1 on help = %q", strings.SplitN(one, "\n", 2)[0])
	}
	if body := strings.SplitN(one, "\n\n", 2); len(body) != 2 || strings.Count(body[1], "\n") > 1 {
		t.Fatalf("lines_limit=1 body = %q", one)
	}
	noBytes := termText(t, s, map[string]any{"cmd": "ls", "bytes_limit": float64(0)})
	if !strings.HasPrefix(noBytes, "TERM/1 204 No Content") {
		t.Fatalf("bytes_limit=0 on ps = %q", strings.SplitN(noBytes, "\n", 2)[0])
	}
	res := termCall(t, s, map[string]any{"cmd": "help", "on_bytes_overflow": "bogus"})
	if !res.IsError || !strings.Contains(toolResultText(res), "on_bytes_overflow") {
		t.Fatalf("invalid overflow policy was accepted:\n%s", toolResultText(res))
	}
}

// learn_mode attaches the directive's Usage to generated replies too, not only
// to streamed windows.
func TestTermLearnModeAttachesUsageToGeneratedReplies(t *testing.T) {
	s := newTestServer(t)
	ls := termText(t, s, map[string]any{"cmd": "ls", "learn_mode": true})
	if !strings.Contains(ls, "Usage: ") {
		t.Fatalf("ls with learn_mode has no Usage:\n%s", ls)
	}
	help := termText(t, s, map[string]any{"cmd": "help run", "learn_mode": true})
	if !strings.Contains(help, "Usage: help") {
		t.Fatalf("help with learn_mode has no Usage:\n%s", help)
	}
}

// The mcp directive performs the streamable HTTP handshake against a real
// endpoint, and close actually removes the handle.
func TestTermMCPStreamableHandshakeAndClose(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s) // mcp open tunnels through a session
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	// OPEN IS THE HANDSHAKE. It runs initialize + tools/list and reports the tools in
	// the same reply that hands back the handle, so there is no second call to make
	// the state machine advance.
	text := termText(t, s, map[string]any{"cmd": "open internal mcp " + ts.URL})
	h := handleFrom(t, text)
	if !strings.HasPrefix(text, "TERM/1 200 OK") || !strings.Contains(text, "tools (") {
		t.Fatalf("open did not report the peer's tools:\n%s", text)
	}
	if res := termCall(t, s, map[string]any{"cmd": "close " + h}); res.IsError {
		t.Fatalf("close failed:\n%s", toolResultText(res))
	}
	after := termText(t, s, map[string]any{"cmd": "read " + h})
	if !strings.HasPrefix(after, "TERM/1 404") || !strings.Contains(after, "names nothing") {
		t.Fatalf("closed peer still resolves:\n%s", after)
	}
}

// A URL this layer cannot speak is refused with the schemes it CAN speak, not
// with a dial failure three layers down.
//
// The second half used to assert the opposite — that `mcp open .../sse` was
// refused as the legacy transport. That refusal is gone: the frame channel reads
// whichever shape the endpoint answers with (a JSON body or one event on a
// stream), so an endpoint carrying an /sse path is a URL like any other. What
// was actually unsupported was the SHAPE, and the shape is now handled.
func TestTermMcpOpenValidatesURL(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s) // mcp open tunnels through a session
	res := termCall(t, s, map[string]any{"cmd": "open internal mcp not-a-url"})
	body := toolResultText(res)
	if !res.IsError || !strings.Contains(body, "http(s)") || !strings.Contains(body, "ws(s)") {
		t.Fatalf("mcp open refused a non-URL without naming what works:\n%s", body)
	}

	// A URL with a scheme but no listener is a connection failure, which is a
	// different reply from a refusal: the caller has to be able to tell a typo
	// from a dead endpoint.
	res = termCall(t, s, map[string]any{"cmd": "open internal mcp ftp://example.com"})
	if !res.IsError || !strings.Contains(toolResultText(res), "unsupported scheme") {
		t.Fatalf("an unknown scheme was not explained:\n%s", toolResultText(res))
	}
}

// span accepts the same three handle spellings as every other directive,
// including a session id whose channel has exited.
func TestTermSpanAcceptsSessionHandle(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "echo hi")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, closed := s.channelExitState(h); closed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := termText(t, s, map[string]any{"cmd": "span " + h})
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("span on an exited session:\n%s", text)
	}
}

// A disabled internal profile is a configuration conflict (409), not an
// internal server error.
func TestTermInternalDisabledIsConflict(t *testing.T) {
	s := newTestServer(t)
	s.NoInternal = true
	res := termCall(t, s, map[string]any{"cmd": "connect internal --argv echo hi"})
	if !strings.HasPrefix(toolResultText(res), "TERM/1 409 Conflict") {
		t.Fatalf("pipe internal with NoInternal = %q", strings.SplitN(toolResultText(res), "\n", 2)[0])
	}
}

// The documented positional action forms must reach their handlers, and a
// gated write is 403, not 500.
func TestTermPositionalActions(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{"cmd": "edit host create --name demo --host 127.0.0.1 --user demo"})
	if !res.IsError || !strings.Contains(toolResultText(res), "disabled") {
		t.Fatalf("edit host create did not reach the write gate:\n%s", toolResultText(res))
	}
	if !strings.HasPrefix(toolResultText(res), "TERM/1 403 Forbidden") {
		t.Fatalf("edit host write gate status = %q", strings.SplitN(toolResultText(res), "\n", 2)[0])
	}
	res = termCall(t, s, map[string]any{"cmd": "rm ext nope"})
	if !res.IsError {
		t.Fatalf("edit ext delete nope was silently accepted:\n%s", toolResultText(res))
	}
}

// An alias may name another alias; resolution follows the chain to the real
// directive.
func TestTermAliasChains(t *testing.T) {
	s := newTestServer(t)
	defer termText(t, s, map[string]any{"cmd": "rm alias chain1"})
	defer termText(t, s, map[string]any{"cmd": "rm alias chain2"})
	if res := termCall(t, s, map[string]any{"cmd": "create alias chain1 help"}); res.IsError {
		t.Fatal(toolResultText(res))
	}
	if res := termCall(t, s, map[string]any{"cmd": "create alias chain2 chain1"}); res.IsError {
		t.Fatal(toolResultText(res))
	}
	text := termText(t, s, map[string]any{"cmd": "chain2"})
	if !strings.Contains(text, "run <session>") {
		t.Fatalf("chained alias did not reach help:\n%s", text)
	}
}

// --- fixtures ---

// writeScript writes a shell script to a temp file and returns its path, so a
// `pipe --argv` test can run a multi-statement program without putting shell
// syntax into an argv.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// openPipe starts a pipe channel on the loopback profile and returns its handle.
func openPipe(t *testing.T, s *Server, argv string) string {
	t.Helper()
	text := termText(t, s, map[string]any{"cmd": "connect internal --argv " + argv})
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("connect --argv %s failed:\n%s", argv, text)
	}
	return handleFrom(t, text)
}

// awaitPtyReady types a marker into a fresh pty and waits for it to come back.
//
// THE ROUND TRIP IS THE ONLY RELIABLE READINESS SIGNAL, which is why this is a test
// helper and not something the open does: it writes a line into the transcript, and a
// caller's transcript is not the server's to write into. `open` waits for the shell's
// first byte, which is a heuristic with a ceiling — a themed PowerShell draws its banner
// over more than a second, and its line editor installs at the end of that, so input
// arriving in between is discarded (issue #156, and the open-half of #162 S1).
//
// A test that types immediately and then asserts on the output is testing the cursor, or
// the fold, or the still-running header — not the shell's startup — so it establishes the
// precondition first. Waiting for the stream to go quiet does NOT establish it: startup
// pauses for longer than any affordable quiet threshold, measured.
func awaitPtyReady(t *testing.T, s *Server, handle string) {
	t.Helper()
	marker := "pty-ready-probe"
	deadline := time.Now().Add(20 * time.Second)
	for {
		// THE MARKER IS BUILT BY THE SHELL, not written in the line: a probe that greps
		// the transcript for a literal matches the ECHO of the line containing it, so it
		// reports "ready" for a shell that discarded the input entirely — which is the
		// exact failure this probe exists to rule out.
		termText(t, s, map[string]any{
			"cmd":     "run " + handle + " $1",
			"args":    []any{"echo pty-ready-$(echo probe)"},
			"timeout": 3,
		})
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + handle + " --from-line 1", "timeout": 0}), marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pty never accepted a line: %s is not reading", handle)
		}
	}
}

// noisyCommand is a command that keeps printing for several seconds, in the syntax of
// whichever shell a pty open actually gives us.
//
// IT HAS TO BE THE RIGHT SYNTAX, and two tests typed bash's. On Windows the pty runs
// PowerShell, which answers `for i in 1 2 3; do …` with a ParserError — and both tests
// still passed, because they looked for their marker with strings.Contains over the whole
// reply, and the ECHO of the typed line contains the marker too. So the loop never ran and
// nothing said so: an assertion that was matching the command instead of its output. The
// number is printed with each line (`tick 3`), which the command text cannot contain, so a
// caller's assertion can only be satisfied by output.
func noisyCommand(marker string) string {
	if runtime.GOOS == "windows" {
		return "1..40 | ForEach-Object { '" + marker + " ' + $_; Start-Sleep -Milliseconds 200 }"
	}
	return "for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do echo " + marker + " $i; sleep 0.2; done"
}

// openPty starts a pty channel (a login shell) and returns its handle.
func openPty(t *testing.T, s *Server) string {
	t.Helper()
	text := termText(t, s, map[string]any{"cmd": "connect internal"})
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("connect internal failed:\n%s", text)
	}
	return handleFrom(t, text)
}

// Isolation must hold on the single-entry surface too.
//
// `term` carries its target inside `cmd`, so the outer admission check — which
// resolves a session from `shell_id`/`session_id` arguments — has nothing to
// resolve and admits everything. Without a per-statement check, a caller that
// claimed a namespace could read, write to and close another caller's channel;
// this is the regression test for that hole.
func TestTermNamespaceIsolationHoldsPerDirective(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	alice := namespaceContext("alice")
	bob := namespaceContext("bob")

	created, err := s.call(alice, "term", map[string]any{"cmd": "connect internal --argv sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(created)
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("alice could not open a channel:\n%s", text)
	}
	handle := handleFrom(t, text)

	// Every directive that names alice's channel must be refused for bob — not
	// just the ones that reach a handler. info/span/close read the manager
	// directly, which is exactly how they would slip through.
	//
	// `run` and `input` take their shell text as a VALUE now (the payload is one
	// variable reference), so the refusal has to be reached with a well-formed
	// statement: the namespace check is about who may drive the channel, not about
	// what the line says.
	for _, call := range []map[string]any{
		{"cmd": "input " + handle + " $1", "args": []any{"echo pwned"}},
		{"cmd": "run " + handle + " $1", "args": []any{"echo pwned"}},
		{"cmd": "key " + handle + " enter"},
		{"cmd": "read " + handle},
		{"cmd": "get " + handle},
		{"cmd": "span " + handle},
		{"cmd": "close " + handle},
		{"cmd": "resize " + handle + " 10 10"},
	} {
		res, err := s.call(bob, "term", call)
		if err != nil {
			t.Fatal(err)
		}
		out := toolResultText(res)
		if !res.IsError || !strings.HasPrefix(out, "TERM/1 403 Forbidden") {
			t.Errorf("%q was not refused with 403 for another namespace:\n%s", call["cmd"], out)
		}
	}

	// And a batch is refused as a whole: running the entries bob may reach while
	// skipping the ones it may not would make the outcome unknowable.
	bobOwn, err := s.call(bob, "term", map[string]any{"cmd": "connect internal --argv sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	bobHandle := handleFrom(t, toolResultText(bobOwn))
	res, err := s.call(bob, "term", map[string]any{"cmd": "read " + bobHandle + "," + handle})
	if err != nil {
		t.Fatal(err)
	}
	if out := toolResultText(res); !strings.HasPrefix(out, "TERM/1 403 Forbidden") {
		t.Errorf("a batch naming a foreign handle must be refused whole:\n%s", out)
	}

	// A listing must not reveal it either.
	ps, err := s.call(bob, "term", map[string]any{"cmd": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(toolResultText(ps), handle) {
		t.Errorf("bob's ps lists alice's channel %s:\n%s", handle, toolResultText(ps))
	}
}

// An admission refusal is a FAILURE, never a success-shaped reply. Regression
// test for the bug where a directive only inspected the transport error and
// rendered a refused operation as 204/200 — the command had not run.
func TestTermRefusalIsNeverSuccessShaped(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	alice := namespaceContext("alice")
	bob := namespaceContext("bob")

	created, err := s.call(alice, "term", map[string]any{"cmd": "connect internal --argv sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	handle := handleFrom(t, toolResultText(created))

	res, err := s.call(bob, "term", map[string]any{"cmd": "input " + handle + " $1", "args": []any{"echo pwned"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a refused directive reported success")
	}
	out := toolResultText(res)
	if statusLineIs(out, "204") || statusLineIs(out, "200") {
		t.Errorf("a refusal was rendered with a success status:\n%s", out)
	}
	// The failure must name the reason a caller can act on, not a generic one.
	if code, _ := headerValue(t, out, "Error-Code"); code != "forbidden" {
		t.Errorf("Error-Code = %q, want forbidden:\n%s", code, out)
	}
}

// TestProfileListRendersTheHandlersKey pins the listing against the handler's
// own field name.
//
// `profile list` rendered from "configs" while handleListSSHConfigs answers
// "ssh_configs", and listFromDoc treats a missing key as an empty list — so the
// one call that says what you can connect to always answered "(none)" on an
// instance holding four profiles. The fix is the key AND the shape: this listing
// is an array of entries, each carrying the name and — since issue #154 — the
// reason it is unusable when it is.
func TestProfileListRendersTheHandlerKey(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	seed := "kind = \"remote\"\nhost = \"10.0.0.1\"\nuser = \"u\"\npassword = \"placeholder\"\n"
	if err := store.Save("demo-one", []byte(seed)); err != nil {
		t.Fatalf("seeding the profile: %v", err)
	}
	s.sshConfigs = store

	out := termText(t, s, map[string]any{"cmd": "ls host"})
	if !strings.Contains(out, "demo-one") {
		t.Fatalf("ls host dropped the profile it was given:\n%s", out)
	}
	if strings.Contains(out, "(none)") {
		t.Fatalf("ls host answered the empty state while a profile exists:\n%s", out)
	}
}

// TestFileSubActionsAreReachable pins that the three multi-action file verbs can
// be asked at all.
//
// perm/link/fs dispatch on their own sub-action, which the directive reads from
// --op. --op was never in the flag whitelist, so the parser rejected the only
// spelling that worked ("unknown flag for file: --op") and the sub-action
// defaulted to empty ("action must be chmod, chown, or chtimes") — three
// capabilities unreachable from the tool surface with no way to discover it.
func TestFileSubActionsAreReachable(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	defer closeShell(t, s, h)

	for _, tc := range []struct{ action, sub, want string }{
		{"perm", "chmod", "chmod"},
		{"link", "readlink", "readlink"},
		{"fs", "realpath", "realpath"},
	} {
		verb := "get"
		if tc.action == "perm" || tc.action == "link" {
			verb = "write"
			if tc.action == "link" {
				verb = "create"
			}
		}
		out := termText(t, s, map[string]any{"cmd": verb + " " + h + " file /tmp --op " + tc.sub})
		if strings.Contains(out, "unknown flag") {
			t.Errorf("file --action %s --op %s: the flag is still not accepted:\n%s", tc.action, tc.sub, out)
		}
		// Reaching the handler is what matters here; whether the SFTP call succeeds
		// against a stub server is not this directive's contract.
		if strings.Contains(out, "action must be") {
			t.Errorf("file --action %s --op %s did not pass the sub-action through:\n%s", tc.action, tc.sub, out)
		}
	}
}

// TestReadBytesLimitZeroConsumesNothing pins that the directive whose job is
// reading can still be told not to.
//
// readSlotAfter honoured bytes_limit=0; read had its own path that skipped the
// check, so max_bytes=0 reached the handler and meant "no limit" — the opposite
// of the documented "hand back the handle, consume nothing".
func TestReadBytesLimitZeroConsumesNothing(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "seq 1 5")
	waitForOutput(t, s, h, "5")

	out := termText(t, s, map[string]any{"cmd": "read " + h, "bytes_limit": float64(0), "timeout": float64(0)})
	if !strings.HasPrefix(out, "TERM/1 204") {
		t.Fatalf("bytes_limit 0 must return no bytes:\n%s", out)
	}
	after := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(1)})
	if !strings.Contains(after, "1\n2\n3") {
		t.Errorf("a bytes_limit 0 read consumed the stream:\n%s", after)
	}
}

// TestMCPCallTakesTheFrameFirst pins the argument position of the frame.
//
// mcpHTTP joined rest[1:], but mcpChannel had already sliced the handle off, so
// the frame's first word was dropped: `mcp call <h> {"jsonrpc":…}` answered
// "needs a JSON-RPC frame" and only worked with a junk word in front of it.
func TestMCPCallTakesTheFrameFirst(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal mcp " + ts.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{"cmd": `call ` + h + ` {"jsonrpc":"2.0","id":7,"method":"tools/list"}`})
	if !strings.Contains(out, `"name":"term"`) && !strings.Contains(out, `"name": "term"`) {
		t.Fatalf("call did not send the frame it was given:\n%s", out)
	}
}

// TestMCPCallAcceptsEveryQuotingOfTheFrame is the regression test for a frame
// whose quotes reached the JSON parser.
//
// The directive splits its payload with strings.Fields, which neither keeps the
// frame whole nor strips the caller's quoting. A JSON body contains spaces of its
// own (`"name": "term"`), so the bare spelling that happened to work for a
// spaceless frame fell apart for a real one, and both quoted spellings failed
// outright with "the frame is not JSON: invalid character '\”" — an error that
// names the JSON parser for what is a word-splitting bug one layer up.
//
// A frame with spaces is used deliberately: it is the only shape that tells the
// three spellings apart.
func TestMCPCallAcceptsEveryQuotingOfTheFrame(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal mcp " + ts.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Spaces in the params object are the point, not decoration.
	frame := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"ls"}}}`
	for name, cmd := range map[string]string{
		"bare":   "call " + h + " " + frame,
		"single": "call " + h + " '" + frame + "'",
		"double": "call " + h + ` "` + frame + `"`,
	} {
		out := termText(t, s, map[string]any{"cmd": cmd})
		// Assert on the REPLY, not on the echo. The rendered slot repeats the
		// request line under "Path:", so a Contains("jsonrpc") check passes even
		// when the frame was refused — it matches the caller's own text. The
		// reply is the only place the peer's answer appears, so the result id is
		// what tells a parsed frame from a rejected one.
		if !strings.Contains(out, `"result"`) {
			t.Errorf("%s-quoted frame was refused:\n%s", name, out)
		}
	}

	// THE ESCAPED SPELLING IS REFUSED, and the refusal has to say why. It is the
	// one a shell-minded caller reaches for (a shell would read the backslash as an
	// escape); here quoting only groups the line and the bytes inside are left
	// alone, so the frame arrives at the parser full of backslashes. Refusing it
	// with a bare "invalid character" sends the caller to look at their JSON, which
	// is correct — the spelling is what is wrong.
	escaped := `"` + strings.ReplaceAll(frame, `"`, `\"`) + `"`
	out := termText(t, s, map[string]any{"cmd": "call " + h + " " + escaped})
	if !strings.Contains(out, "not JSON") {
		t.Fatalf("an escaped frame must be refused, not sent:\n%s", out)
	}
	if !strings.Contains(out, "single quotes") {
		t.Errorf("the refusal does not teach the spelling that works:\n%s", out)
	}
}

// TestUnquoteWordRemovesOneLayerAndNothingElse pins the restraint.
//
// The temptation is to unescape while the quotes are being peeled — inside double
// quotes a shell would read a backslash-quote as a quote — but unescaping is
// expansion, and this layer does not evaluate. A `$` in a JSON body would be the
// first casualty, and the second would be every caller who meant a literal
// backslash.
func TestUnquoteWordRemovesOneLayerAndNothingElse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{`'{"a":1}'`, `{"a":1}`},
		{`"{\"a\":1}"`, `{\"a\":1}`},
		{`"C:\Program Files"`, `C:\Program Files`},
		{`""`, ``},
		{`"`, `"`},
	} {
		if got := unquoteWord(tc.in); got != tc.want {
			t.Errorf("unquoteWord(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFilePermChmodActuallyChangesTheMode is the regression test for a silently
// destructive bug.
//
// `file --action perm --op chmod --mode 420` reported success and left the file at
// mode 0000. The directive layer sent --mode as a STRING (it IS a string for
// read/write: text|hex|file), while the chmod handler reads it with getFloat64,
// which returns its DEFAULT for a non-number. The mode therefore became 0, the
// call succeeded, and the file was left unreadable — even by its own owner, which
// makes it look like a permissions problem on the target rather than a lost
// argument here.
//
// The existing chmod tests call the tool handler directly with float64(493), so
// the conversion in the directive layer was never exercised. This test enters where
// a caller does, and checks the mode that actually landed on the file rather than
// the fact that the call returned 200.
func TestFilePermChmodActuallyChangesTheMode(t *testing.T) {
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
	sid := parseResult(t, res)["session_id"].(string)

	dir := t.TempDir()
	file := filepath.Join(dir, "mode-probe.txt")
	if err := os.WriteFile(file, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}

	// 420 decimal == 0644. The old bug set 0 and still answered 200.
	out := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + file + " --op chmod --mode 420",
	})
	if !strings.Contains(out, "success") && !statusLineIs(out, "200") {
		t.Fatalf("chmod did not report success: %s", out)
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	// What the mode READS BACK as is platform-specific: Windows maps permissions
	// onto the read-only bit, so 0644 comes back as 0666 and 0755 as 0666 too
	// (measured). The bug this test exists for sent 0, which Windows reports as
	// 0444 — a DIFFERENT value — so the two are distinguishable everywhere and
	// the test does not need to skip. Asserting "not the zero-mode result" is what
	// actually catches it; asserting the exact bits would only catch it on unix.
	got := info.Mode().Perm()
	if got == 0444 {
		t.Fatalf("file mode = %#o: the mode reached chmod as 0, so the whole permission set was stripped (a string where a number was needed is silently read as the default)", got)
	}
	if runtime.GOOS != "windows" && got != 0644 {
		t.Fatalf("file mode = %#o, want 0644", got)
	}
}

// TestGuideListsCoreAndPointsAtTheRest pins the shape of the recovery call.
//
// The empty call is read by a caller that has not decided what to do yet, so it
// carries the directives needed to start — and a pointer to the rest. Both halves
// are load-bearing:
//
//   - Listing all 23 here cost ~1.2 KB of capability the caller has no use for at
//     that moment, on every wake-up (measured: 2187 → 953 bytes, −56%).
//   - Listing ONLY the core ones would make file/forward/extension work invisible
//     to anyone who never suspects there is more to see. That is the failure the
//     old "7 of 23 examples" had, and this test refuses to reintroduce it.
func TestGuideListsCoreAndPointsAtTheRest(t *testing.T) {
	s := newTestServer(t)
	body := termGuideBody(t, s)

	// Every core directive is listed with its usage line.
	var core int
	for _, op := range directive.Ops() {
		if !op.Core {
			continue
		}
		core++
		if !strings.Contains(body, op.Usage) {
			t.Errorf("the guide omits the core directive %q", op.Name)
		}
	}
	if core == 0 {
		t.Fatal("no directive is marked Core: the guide would be empty")
	}

	// The pointer must be there, must name `help`, and must say how many there
	// are — a caller that reads "core" with no count cannot tell whether it is
	// looking at a subset or at everything.
	if !strings.Contains(body, "help") {
		t.Error("the guide does not mention `help`, so the rest is undiscoverable")
	}
	if !strings.Contains(body, strconv.Itoa(len(directive.Ops()))) {
		t.Errorf("the guide does not say how many directives exist (%d)", len(directive.Ops()))
	}

	// And the non-core ones are NOT dumped here: the point of the split.
	for _, op := range directive.Ops() {
		if op.Core {
			continue
		}
		if strings.Contains(body, "  "+op.Usage+"\n") {
			t.Errorf("the guide lists the non-core directive %q; `help` is where it belongs", op.Name)
		}
	}
}

// TestHelpListsEveryDirective is the other half of the contract: whatever the
// guide filters out, `help` must still list. A core-only guide with a lossy help
// would hide capabilities rather than defer them.
func TestHelpListsEveryDirective(t *testing.T) {
	s := newTestServer(t)
	body := termText(t, s, map[string]any{"cmd": "help"})

	for _, op := range directive.Ops() {
		if !strings.Contains(body, op.Usage) {
			t.Errorf("`help` omits %q", op.Name)
		}
	}
}

// termGuideBody runs the empty call and returns its body.
func termGuideBody(t *testing.T, s *Server) string {
	t.Helper()
	reply := s.Term(t.Context(), "", map[string]any{})
	if len(reply.Slots) != 1 {
		t.Fatalf("the guide returned %d slots, want 1", len(reply.Slots))
	}
	return reply.Slots[0].Body
}

// TestAParseFailureSaysNothingRan pins the one thing a caller cannot see.
//
// A cmd is parsed in full before the first statement runs. So one typo in a
// batch means the statements before it did not run either — correct, because a
// batch that half-ran reports a partial success nobody can read. But the reply
// was a bare 400 with no slots, which is indistinguishable from a batch that ran
// and produced nothing. This is the sentence that separates the two, and it only
// appears when there WAS an earlier statement to be wrong about: on a single
// broken statement there is nothing else that could have run.
func TestAParseFailureSaysNothingRan(t *testing.T) {
	s := newTestServer(t)

	// A batch where the first statement parses: the note must name it.
	reply := s.Term(t.Context(), "ls; definitely-not-a-directive x", map[string]any{})
	if len(reply.Slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(reply.Slots))
	}
	slot := reply.Slots[0]
	if slot.Code != 400 {
		t.Fatalf("code = %d, want 400", slot.Code)
	}
	if !strings.Contains(slot.Body, "nothing was executed") {
		t.Errorf("the batch failure does not say nothing ran: %q", slot.Body)
	}
	if !strings.Contains(slot.Body, "1 statement") {
		t.Errorf("the note does not say how many parsed first: %q", slot.Body)
	}

	// A single broken statement: the note would be noise, and the body must stay
	// the error alone. This is the common case and it must not grow.
	reply = s.Term(t.Context(), "definitely-not-a-directive x", map[string]any{})
	if got := reply.Slots[0].Body; strings.Contains(got, "nothing was executed") {
		t.Errorf("a single broken statement got the batch note: %q", got)
	}
}

// TestABatchIsNotPartlyExecuted is the property the note describes. It uses a
// statement with an observable side effect (opening a session) so that a future
// "run what parsed" change cannot pass by only editing the message.
func TestABatchIsNotPartlyExecuted(t *testing.T) {
	s := newTestServer(t)
	const name = "must-not-exist-after-a-parse-failure"

	reply := s.Term(t.Context(), "pty internal --name "+name+"; definitely-not-a-directive", map[string]any{})
	if reply.Slots[0].Code != 400 {
		t.Fatalf("the batch was not refused: %+v", reply.Slots[0])
	}

	ps := s.Term(t.Context(), "ls --all", map[string]any{})
	if strings.Contains(ps.Slots[0].Body, name) {
		t.Error("the statement before the parse failure ran: parsing must precede every statement")
	}
}

// TestRunDetailStatesWhatAPtyCannotKnow pins the thing the reply cannot say.
//
// On a pty, `run` waits for the stream to go still and returns; there is no
// completion signal to report, so a reply that stops mid-output is indistinguishable
// from a finished command. That is invisible in the wire format, which makes
// `help run` the only place a caller can find it — along with the two things that
// do help: raising `timeout` for a command that has not spoken yet, and `read` for
// the bytes that arrive after the wait ended.
func TestRunDetailStatesWhatAPtyCannotKnow(t *testing.T) {
	s := newTestServer(t)
	body := termText(t, s, map[string]any{"cmd": "help run"})

	for _, want := range []string{"pty", "STILL", "timeout", "pipe"} {
		if !strings.Contains(body, want) {
			t.Errorf("`help run` does not mention %q:\n%s", want, body)
		}
	}
	// The advice that actually rescues the caller: the bytes are still there, and
	// read repeats the sample rather than continuing after it.
	if !strings.Contains(body, "its own cursor") {
		t.Errorf("`help run` does not say where the rest of the output is:\n%s", body)
	}
}

// quoteArgv formats an argv list as single-quoted shell words.
func quoteArgv(argv []any) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		str := fmt.Sprint(a)
		parts = append(parts, "'"+strings.ReplaceAll(str, "'", `'''`)+"'")
	}
	return strings.Join(parts, " ")
}

// internalSession opens a session on the built-in loopback and returns its id.
//
// Gateways need a session: `mcp open internal <url>` says which network the URL is
// resolved on. Tests that used to pass a bare URL now name `internal` and, to do that
// meaningfully, need the session to exist first — the same two steps a caller takes.
func internalSession(t *testing.T, s *Server) string {
	t.Helper()
	out := termText(t, s, map[string]any{"cmd": "connect internal"})
	// pty answers with a channel handle (<session>:<index>); the session half is what
	// a gateway needs, because `mcp open` tunnels through the link and not the shell.
	h, ok := headerValue(t, out, "Handle")
	if !ok {
		t.Fatalf("pty internal returned no Handle header:\n%s", out)
	}
	sid, _, _ := strings.Cut(h, ":")
	if sid == "" {
		t.Fatalf("pty internal returned a handle with no session part: %q", h)
	}
	return sid
}

// TestEveryFlagIsDiscoverableFromHelp pins that the derived flag list reaches the
// caller. The table declares the flags and `help <verb>` prints them from the same
// table, so the only way this fails is the wiring between the two — which is exactly
// the failure a caller would experience as "the capability does not exist".
func TestEveryFlagIsDiscoverableFromHelp(t *testing.T) {
	s := newTestServer(t)
	for _, op := range directive.Ops() {
		if len(op.Flags) == 0 {
			continue
		}
		body := termText(t, s, map[string]any{"cmd": "help " + op.Name})
		// The NAMES, not the declarations: `help` renders what a caller can pass, and
		// `--offset:number` is the table's annotation of `--offset`.
		for _, f := range op.FlagNames() {
			if !strings.Contains(body, "--"+f) {
				t.Errorf("help %s does not mention --%s", op.Name, f)
			}
		}
	}
}

// statusCode reads the code off a rendered slot's status line.
//
// SEARCHING THE WHOLE REPLY FOR "400" IS WRONG: an objectId, a byte count or a
// base64 blob can contain those digits, and the assertion then fails on a payload
// that is perfectly healthy. The status line is the only place the code is a code.
func statusCode(t *testing.T, text string) int {
	t.Helper()
	line, _, _ := strings.Cut(text, "\n")
	field := strings.Fields(line)
	if len(field) < 2 {
		t.Fatalf("no status line in:\n%s", text)
	}
	n, err := strconv.Atoi(field[1])
	if err != nil {
		t.Fatalf("status line is not a code: %q", line)
	}
	return n
}

// statusLineIs reports whether a rendered reply's FIRST LINE carries the given code.
//
// The whole-reply substring tests it replaces were flaky by construction: a reply echoes
// the command in `Path:`, and that echo contains the session and channel ids, which are
// random hex. `Path: call 548e1202-666:1` contains "202", so a test asserting "this did
// not answer 202" failed for whichever ids happened to spell it and passed for every
// other draw. It failed on CI's windows leg and never locally.
//
// A status code is a claim about the first line. Nothing else in a reply is one.
func statusLineIs(reply, code string) bool {
	first, _, _ := strings.Cut(reply, "\n")
	return strings.HasPrefix(strings.TrimSpace(first), "TERM/1 "+code)
}
