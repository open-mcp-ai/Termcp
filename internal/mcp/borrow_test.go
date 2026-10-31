package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// TestShortCircuitFileOperationsPinsIssue237 verifies that:
// 1. read/write with a host target (e.g. "internal", "`use internal`") operates directly over a pooled transport;
// 2. NO user-visible Session is created in the manager;
// 3. The archive/session list remains completely clean.
func TestShortCircuitFileOperationsPinsIssue237(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)

	testFilePath := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(testFilePath, []byte("short-circuit success"), 0644); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		sessMgr:    sessMgr,
		sshConfigs: sshconfig.NewStore(dir),
	}

	// 1. Read using bare host "internal"
	reply := s.executeTerm(context.Background(), "read internal file "+filepath.ToSlash(testFilePath), termArgs{BytesLimit: 1 << 20})
	if len(reply.slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(reply.slots))
	}
	slot := reply.slots[0]
	if slot.Code != 200 {
		t.Fatalf("read internal file returned HTTP %d: %s", slot.Code, slot.Body)
	}
	if !strings.Contains(slot.Body, "short-circuit success") {
		t.Fatalf("read output %q does not contain expected file content", slot.Body)
	}

	// CRITICAL ASSERTION: No session was created in the manager
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Fatalf("expected 0 sessions in registry, got %d: %+v", len(all), all)
	}

	// 2. Read using backtick "use internal" form
	reply2 := s.executeTerm(context.Background(), "read `use internal` file "+filepath.ToSlash(testFilePath), termArgs{BytesLimit: 1 << 20})
	if len(reply2.slots) != 1 {
		t.Fatalf("expected 1 slot for `use internal`, got %d", len(reply2.slots))
	}
	slot2 := reply2.slots[0]
	if slot2.Code != 200 {
		t.Fatalf("read `use internal` file returned HTTP %d: %s", slot2.Code, slot2.Body)
	}
	if !strings.Contains(slot2.Body, "short-circuit success") {
		t.Fatalf("slot2 output %q missing expected content", slot2.Body)
	}

	// Verify the session list is STILL completely empty!
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Fatalf("session list leaked a session: %+v", all)
	}

	// 3. Write using short-circuit borrowed host
	writeTargetFile := filepath.Join(dir, "written.txt")
	writeReply := s.executeTerm(context.Background(), "write internal file "+filepath.ToSlash(writeTargetFile)+" --data 'written by borrowed transport'", termArgs{BytesLimit: 1 << 20})
	if len(writeReply.slots) != 1 || writeReply.slots[0].Code != 200 {
		t.Fatalf("write failed: %+v", writeReply.slots)
	}
	content, err := os.ReadFile(writeTargetFile)
	if err != nil || string(content) != "written by borrowed transport" {
		t.Fatalf("written file content mismatch: %q, err: %v", string(content), err)
	}
	// `--dry-run` is a session-removal preview, not an ignored file flag. The rm
	// surface must refuse it before the deletion handler, and preserve the file.
	dryRun := s.executeTerm(context.Background(), "rm use internal file "+filepath.ToSlash(writeTargetFile)+" --dry-run", termArgs{BytesLimit: 1 << 20})
	if len(dryRun.slots) != 1 || dryRun.slots[0].Code != 400 {
		t.Fatalf("file rm --dry-run must refuse rather than delete, got %+v", dryRun.slots)
	}
	if _, err := os.Stat(writeTargetFile); err != nil {
		t.Fatalf("file rm --dry-run removed the file: %v", err)
	}

	// Session registry must STILL have zero sessions!
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Fatalf("session list leaked a session after write: %+v", all)
	}

	// 4. A SHORT-CIRCUIT COMMAND runs on the same borrowed transport, and leaves no
	// session behind either. This is the half that used to be missing: `run use:<host>
	// $1` answered shell_not_found because the target was only ever resolved as a
	// channel handle.
	runReply := s.executeTerm(context.Background(), "run use internal $1", termArgs{Args: []string{"echo borrowed-run-ok"}, BytesLimit: 1 << 20, TimeoutSeconds: 10})
	if len(runReply.slots) != 1 {
		t.Fatalf("expected 1 slot for run use internal, got %d", len(runReply.slots))
	}
	runSlot := runReply.slots[0]
	if runSlot.Code != 200 {
		t.Fatalf("run use internal returned HTTP %d: %s", runSlot.Code, runSlot.Body)
	}
	if !strings.Contains(runSlot.Body, "borrowed-run-ok") {
		t.Errorf("run output %q missing the command's output", runSlot.Body)
	}
	if runSlot.Headers["Exit-Code"] != "0" {
		t.Errorf("Exit-Code header = %q, want 0", runSlot.Headers["Exit-Code"])
	}
	if runSlot.Headers["Host"] != "internal" {
		t.Errorf("Host header = %q, want internal", runSlot.Headers["Host"])
	}
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Fatalf("a short-circuit run registered a session: %+v", all)
	}

	// 5. A NON-ZERO EXIT is this call's failure, exactly as `run` reports one on a channel.
	command := "sh -c 'exit 3'"
	if runtime.GOOS == "windows" {
		command = "cmd.exe /c exit 3"
	}
	fail := s.runWithArgs("run use internal $1", command)
	if len(fail.slots) != 1 || fail.slots[0].Code != 500 {
		t.Fatalf("a failing command should answer 500, got %+v", fail.slots)
	}
	if fail.slots[0].Headers["Exit-Code"] != "3" {
		t.Errorf("Exit-Code header = %q, want 3; reply: %+v", fail.slots[0].Headers["Exit-Code"], fail.slots[0])
	}

	// 6. `use <host>` UNQUOTED is one target, not a target plus a stray word.
	spaced := s.runWithArgs("run use internal $1", "echo spaced-form")
	if len(spaced.slots) != 1 || spaced.slots[0].Code != 200 || !strings.Contains(spaced.slots[0].Body, "spaced-form") {
		t.Fatalf("`run use internal $1` did not reach the host: %+v", spaced.slots)
	}

	// 7. --grep filters the command's own output rather than a transcript it never had.
	// Two lines, one matching: the reply is the matching LINE, not the whole output.
	twoLines := `sh -c 'printf "one\nmatch-me\n"'`
	if runtime.GOOS == "windows" {
		twoLines = `cmd.exe /c "echo one & echo match-me"`
	}
	grepped := s.runWithArgs("run use internal --grep match-me $1", twoLines)
	if len(grepped.slots) != 1 {
		t.Fatalf("expected 1 slot for a grepped run, got %d", len(grepped.slots))
	}
	if grepped.slots[0].Code != 200 || !strings.Contains(grepped.slots[0].Body, "match-me") {
		t.Fatalf("grepped run reply = %+v", grepped.slots[0])
	}
	if strings.Contains(grepped.slots[0].Body, "one\n") || strings.Contains(grepped.slots[0].Body, "one ") {
		t.Errorf("grep did not filter: %q", grepped.slots[0].Body)
	}
	if grepped.slots[0].Headers["Matches"] != "1" {
		t.Errorf("Matches header = %q, want 1", grepped.slots[0].Headers["Matches"])
	}

	// A search that finds nothing is 204 with the count it scanned, not a 200 with an
	// empty body: "no match" and "no output" are different answers.
	none := s.runWithArgs("run use internal --grep no-such-line $1", twoLines)
	if len(none.slots) != 1 || none.slots[0].Code != 204 {
		t.Fatalf("a run whose grep matched nothing should answer 204, got %+v", none.slots)
	}
	if none.slots[0].Headers["Matches"] != "0" {
		t.Errorf("Matches header = %q, want 0", none.slots[0].Headers["Matches"])
	}

	// 8. DEFAULT APPROVAL IS ENFORCED AT THE ENTRANCE.
	// A host configured with default_approval cannot be targeted by short-circuit
	// execution, because a short-circuit call has no session to hold an approval queue.
	// Bypassing review by choosing `use <host>` instead of `connect <host>` is forbidden.
	profile := "kind = \"internal\"\ndescription = \"test\"\ndefault_approval = true\n"
	if err := s.sshConfigs.Save("gated-box", []byte(profile)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.sshConfigs.Delete("gated-box") }()

	gatedRun := s.runWithArgs("run use gated-box $1", "echo should-not-run")
	if len(gatedRun.slots) != 1 || gatedRun.slots[0].Code != 403 {
		t.Fatalf("expected 403 Forbidden for short-circuit run on gated host, got %+v", gatedRun.slots)
	}
	if !strings.Contains(gatedRun.slots[0].Body, "default_approval") || !strings.Contains(gatedRun.slots[0].Body, "connect gated-box") {
		t.Errorf("refusal missing rationale and next step: %s", gatedRun.slots[0].Body)
	}

	// Gated write is ALSO refused at the entrance
	gatedWrite := s.executeTerm(context.Background(), "write use gated-box file /tmp/x --data test", termArgs{})
	if len(gatedWrite.slots) != 1 || gatedWrite.slots[0].Code != 403 {
		t.Fatalf("expected 403 for write on gated host, got %+v", gatedWrite.slots)
	}
	if !strings.Contains(gatedWrite.slots[0].Body, "default_approval") {
		t.Errorf("write refusal missing default_approval notice: %s", gatedWrite.slots[0].Body)
	}

	// 9. BARE HOST NAME as short-circuit target without `use` prefix
	bareRun := s.runWithArgs("run internal $1", "echo bare-host-ok")
	if len(bareRun.slots) != 1 || bareRun.slots[0].Code != 200 || !strings.Contains(bareRun.slots[0].Body, "bare-host-ok") {
		t.Fatalf("run internal $1 failed: %+v", bareRun.slots)
	}

	// Still zero sessions
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Fatalf("gated checks leaked a session: %+v", all)
	}

	// 10. A COMMAND THAT OUTLIVES THE WINDOW is 504 and says it was stopped: the
	// process did not exit, and a short-circuit run has nowhere to keep it, so it was
	// closed. A 200 here would read as "ran and printed nothing".
	slow := `sh -c 'sleep 5'`
	if runtime.GOOS == "windows" {
		slow = `cmd.exe /c ping -n 6 127.0.0.1`
	}
	timedOut := s.executeTerm(context.Background(), "run use internal $1", termArgs{Args: []string{slow}, BytesLimit: 1 << 20, TimeoutSeconds: 0.2})
	if len(timedOut.slots) != 1 || timedOut.slots[0].Code != 504 {
		t.Fatalf("a run that outlived its window should answer 504, got %+v", timedOut.slots)
	}
	if timedOut.slots[0].Headers["Status"] != "stopped" {
		t.Errorf("Status header = %q, want stopped", timedOut.slots[0].Headers["Status"])
	}
	// The caller has to learn WHY and WHAT to do, not only that the run stopped:
	// `Status: stopped` with no sentence left the reason to be guessed.
	if hint := timedOut.slots[0].Headers["Hint"]; !strings.Contains(hint, "timeout") {
		t.Errorf("a stopped run must name the window it ran out of; Hint = %q", hint)
	}
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Fatalf("a stopped short-circuit run registered a session: %+v", all)
	}

	// 11. A FAILURE WITH NO OUTPUT STILL SAYS SOMETHING. A 500 with an empty body and
	// an exit code is a question, not an answer.
	emptyFail := s.runWithArgs("run use internal $1", `sh -c 'exit 7'`)
	if runtime.GOOS == "windows" {
		emptyFail = s.runWithArgs("run use internal $1", "cmd.exe /c exit 7")
	}
	if len(emptyFail.slots) != 1 || emptyFail.slots[0].Code != 500 {
		t.Fatalf("a failing command should answer 500, got %+v", emptyFail.slots)
	}
	if strings.TrimSpace(emptyFail.slots[0].Body) == "" {
		t.Errorf("a failure with no output must explain itself; reply = %+v", emptyFail.slots[0])
	}

	// 12. A BAD FILTER IS REFUSED BEFORE THE COMMAND RUNS. The 400 used to arrive
	// with Output-Bytes set, i.e. the command had already run and no retry could undo
	// that.
	badGrep := s.runWithArgs("run use internal --grep '[' $1", "echo side-effect-marker")
	if len(badGrep.slots) != 1 || badGrep.slots[0].Code != 400 {
		t.Fatalf("an invalid --grep should answer 400, got %+v", badGrep.slots)
	}
	if got := badGrep.slots[0].Headers["Output-Bytes"]; got != "" && got != "0" {
		t.Errorf("--grep was validated AFTER the command ran: Output-Bytes = %q", got)
	}
	if _, ran := badGrep.slots[0].Headers["Status"]; ran {
		t.Errorf("an invalid --grep must not report a process status: %+v", badGrep.slots[0].Headers)
	}

	// 13. A MISSING PROFILE IS A 404 WITH A NEXT STEP, not a 500 that reads like an
	// outage. Both spellings must agree.
	missingRead := s.executeTerm(context.Background(), "read use:nosuchhost file /tmp/x", termArgs{BytesLimit: 1 << 20, TimeoutSeconds: 10})
	for _, tc := range []struct {
		cmd  string
		slot envelope.Slot
	}{
		{cmd: "run use:nosuchhost $1", slot: s.runWithArgs("run use:nosuchhost $1", "echo nope").slots[0]},
		{cmd: "read use:nosuchhost file /tmp/x", slot: missingRead.slots[0]},
	} {
		if tc.slot.Code != 404 {
			t.Fatalf("%s should answer 404 for an unknown profile, got %+v", tc.cmd, tc.slot)
		}
		if !strings.Contains(tc.slot.Body, "ls host") {
			t.Errorf("%s refusal must name the next step; body = %q", tc.cmd, tc.slot.Body)
		}
	}

	// A bare name that is not a profile keeps its 404 but now names the other road.
	bareMissing := s.runWithArgs("run truly-not-a-host $1", "echo nope")
	if len(bareMissing.slots) != 1 || bareMissing.slots[0].Code != 404 {
		t.Fatalf("an unknown bare target should answer 404, got %+v", bareMissing.slots)
	}
	if !strings.Contains(bareMissing.slots[0].Body, "ls host") {
		t.Errorf("unknown bare target must mention host profiles; body = %q", bareMissing.slots[0].Body)
	}
}

// runWithArgs runs one cmd with a single positional value, which is the only way a
// payload verb accepts a command line.
func (s *Server) runWithArgs(cmd, arg string) termReply {
	return s.executeTerm(context.Background(), cmd, termArgs{Args: []string{arg}, BytesLimit: 1 << 20, TimeoutSeconds: 10})
}
