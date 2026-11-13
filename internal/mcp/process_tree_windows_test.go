//go:build windows

package mcp

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestClosingAChannelEndsItsProcessTree pins #293 at the surface the report used.
//
// `close <channel>` answered 204 while the remote program kept running. A closed SSH
// channel gives the session's streams an EOF and nothing else, and Windows has no
// SIGHUP, so a program that does not read stdin never learns — the reported case was
// `--argv ping.exe -n 300 127.0.0.1`, and the port it still held made the next task
// fail with `os error 10048`. Killing the session's DIRECT child does not reach it
// either: on Windows the shell never execs, so the session owns the shell and the
// shell owns the program.
//
// Two halves are pinned together, because either one alone leaves the tree alive: the
// client ASKS before closing (a TERM request, RFC 4254 §6.9) and the server answers by
// ending a job object every process in the tree belongs to. The observation is the
// helper's own pid: it must be gone after `close`.
//
// Windows-only, and that is not a CI gap — .github/workflows/test.yml runs the suite on
// windows-latest too.
func TestClosingAChannelEndsItsProcessTree(t *testing.T) {
	s := newTestServer(t)

	// Through cmd.exe, so the helper is a GRANDCHILD: the shape the report describes
	// and the one a direct-child kill cannot reach.
	out := termText(t, s, map[string]any{
		"cmd":  `open internal --argv cmd.exe /c $1 $2`,
		"args": []any{helperExecutable(t), treeHelperArg},
	})
	handle, ok := headerValue(t, out, "Handle")
	if !ok {
		t.Fatalf("no channel handle:\n%s", out)
	}

	text := awaitPtyReadyText(t, s, handle, "PID=")
	pid := parseTreeHelperPID(t, text)

	if got := termText(t, s, map[string]any{"cmd": "close " + handle}); !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("close did not report a closed channel:\n%s", got)
	}

	// A POLL WITH A DEADLINE, not a sleep: the state asked about is "the pid is gone",
	// and the first observation that sees it ends the loop. The deadline only bounds
	// how long a FAILURE takes to report, so a loaded runner costs iterations.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if !processAlive(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still running after the channel closed: the session's tree outlived it", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitPtyReadyText reads the channel until marker appears, with a deadline that bounds
// a hang rather than a budget.
func awaitPtyReadyText(t *testing.T, s *Server, handle, marker string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var text string
	for {
		text += termText(t, s, map[string]any{"cmd": "read " + handle})
		if strings.Contains(text, marker) {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never reported itself (waiting for %q):\n%s", marker, text)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func parseTreeHelperPID(t *testing.T, text string) int {
	t.Helper()
	for _, field := range strings.Fields(text) {
		if v, ok := strings.CutPrefix(field, "PID="); ok {
			pid, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("helper pid %q: %v", v, err)
			}
			return pid
		}
	}
	t.Fatalf("no PID= in the channel output:\n%s", text)
	return 0
}

// helperExecutable is this test binary, spelled with forward slashes because the
// payload is handed to a login shell where a backslash is an escape character.
func helperExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(exe, `\`, "/")
}

// processAlive reports whether pid names a running process.
//
// STILL_ACTIVE as an exit code is the documented way to ask this about a pid whose
// handle the caller does not own; a process that has exited reports its real code, so
// a killed helper is never mistaken for a running one.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE, from processthreadsapi.h
	return code == stillActive
}
