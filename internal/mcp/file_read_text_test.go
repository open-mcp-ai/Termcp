package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startDelayedPipe opens a pipe channel that prints nothing for `delay` seconds and
// then one line, and returns its handle. That is the shape #245 describes: a deploy
// script whose output arrives after the caller has already asked for it.
func startDelayedPipe(t *testing.T, s *Server, delay int, marker string) string {
	t.Helper()
	var argv string
	if runtime.GOOS == "windows" {
		// Start-Sleep's positional parameter is a number of seconds: a Go duration
		// ("1s") fails to parse there.
		argv = `powershell.exe -NoLogo -NoProfile -Command "Start-Sleep ` + fmt.Sprint(delay) +
			`; Write-Output ` + marker + `"`
	} else {
		argv = `sh -c "sleep ` + fmt.Sprint(delay) + `; echo ` + marker + `"`
	}
	out := termText(t, s, map[string]any{"cmd": "connect internal --argv " + argv})
	h := firstHandle(out)
	if h == "" {
		t.Fatalf("could not open the pipe channel:\n%s", out)
	}
	return h
}

// `read <session> file <path>` ANSWERED A QUESTION NOBODY ASKED (issue #248).
//
// Text mode escapes newlines so the data fits in one value that can be pasted back
// into `--data`. That is the right contract for a value and the wrong one for
// reading: 946 bytes of nginx.conf arrived as one line, and a missed line in a config
// review is a missed setting. `--raw` says "this reply is text"; `--lines` numbers it,
// because "change line 14" needs an address the next read or replace can use.
func TestAFileReadCanBeTextInsteadOfAValue(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	body := "server {\n    listen 80;\n    root /var/www;\n}\n"
	p := filepath.Join(t.TempDir(), "site.conf")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	// The default is unchanged: a value, with the newlines escaped.
	escaped := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p})
	if !strings.Contains(escaped, `server {\n    listen 80;`) {
		t.Errorf("the default text mode must still return a single-line value:\n%s", escaped)
	}

	// --raw keeps the file's own lines, and escapes nothing.
	raw := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p + " --raw"})
	if !strings.Contains(raw, "server {\n    listen 80;\n") {
		t.Errorf("--raw must preserve the file's newlines:\n%s", raw)
	}
	if strings.Contains(raw, `\n `) {
		t.Errorf("--raw still escaped a newline:\n%s", raw)
	}

	// --lines numbers them from the file's first line.
	numbered := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p + " --lines"})
	for _, want := range []string{"1\tserver {", "2\t    listen 80;", "4\t}"} {
		if !strings.Contains(numbered, want) {
			t.Errorf("--lines must number each line (%q missing):\n%s", want, numbered)
		}
	}
	// The newline that ends the file terminates line 4; it is not a fifth line.
	if strings.Contains(numbered, "5\t") {
		t.Errorf("--lines invented a line for the trailing newline:\n%s", numbered)
	}

	// A numbered page that starts at an offset counts the lines it skipped, or the
	// number is an address that points at the wrong line. Line 2 begins at byte 9.
	page := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p + " --lines --offset 9"})
	if !strings.Contains(page, "2\t    listen 80;") {
		t.Errorf("a numbered page must carry the FILE's line numbers, not 1..n:\n%s", page)
	}

	// --lines counts LINES, so it means nothing for hex bytes or a download.
	for _, mode := range []string{"hex", "file"} {
		got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p + " --lines --mode " + mode})
		if !strings.HasPrefix(got, "TERM/1 400") {
			t.Errorf("--lines with --mode %s was accepted and not applied:\n%s", mode, got)
		}
	}
}

// A BINARY FILE IS NOT A PAGE OF ESCAPES.
//
// Inline text mode renders bytes it cannot decode as \xHH, so a PNG arrived as a
// confident wall of escapes that reads like content. The refusal names the modes that
// do work instead of leaving the caller to notice.
func TestABinaryFileReadAsTextSaysSo(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	p := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(p, []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, 0644); err != nil {
		t.Fatal(err)
	}

	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("a NUL byte in a text read must be refused, not rendered:\n%s", got)
	}
	if !strings.Contains(got, "binary") || !strings.Contains(got, "--mode hex") {
		t.Errorf("the refusal must say it is binary and name a mode that works:\n%s", got)
	}

	// Hex is the byte-exact door and still answers on the same file.
	if hexed := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + p + " --mode hex"}); !strings.Contains(hexed, "89504e47") {
		t.Errorf("--mode hex must still return the bytes:\n%s", hexed)
	}
}

// A TAIL READ ON A LIVE PIPE USED TO ANSWER BEFORE THE COMMAND DID (issue #245).
//
// `read <pipe>` waits for the process, while `read <pipe> --tail N` scanned the
// buffer and returned at once. So one channel reported one command two ways, and the
// immediate answer — `204 No Content`, `Waited: 0ms` — is byte-identical to "the
// command printed nothing". The issue's workaround was a fixed sleep before the read.
func TestATailReadWaitsForALivePipe(t *testing.T) {
	s := newTestServer(t)
	h := startDelayedPipe(t, s, 1, "LATE_TAIL")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	began := time.Now()
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 20", "timeout": 10})
	elapsed := time.Since(began)

	if !strings.Contains(got, "LATE_TAIL") {
		t.Fatalf("a tail read must wait for the command it is tailing:\n%s", got)
	}
	// The command sleeps a second; the old answer snapped in microseconds.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("the tail read returned after %v, before the command printed", elapsed)
	}
	// The wait observed the exit, so the reply says so — the fact the caller wanted.
	if v, ok := headerValue(t, got, "Status"); !ok || v != "exited" {
		t.Errorf("a wait that observed the exit must report it (Status=%q, present=%v):\n%s", v, ok, firstLine(got))
	}
}

// A PTY TAIL IS STILL A GLANCE, NOT A WAIT.
//
// A pty's shell outlives every command, so "wait for completion" has no ending there:
// making this branch wait would charge the caller's whole window for looking at the
// screen. The pipe fix must not reintroduce that cost.
func TestAPtyTailDoesNotWait(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo PTY_TAIL"}})

	// POLL, BUT NEVER WAIT. A pty's bytes arrive when the shell gets round to flushing
	// them, so on a loaded machine one glance can land before the echo — which is a
	// timing artefact, not the defect this test is about. Each read is still timed: the
	// assertion is that looking never costs the caller's window.
	var got string
	for i := 0; i < 20; i++ {
		began := time.Now()
		got = termText(t, s, map[string]any{"cmd": "read " + h + " --tail 5", "timeout": 5})
		if elapsed := time.Since(began); elapsed > 2*time.Second {
			t.Fatalf("a pty tail waited %v with a 5s window; a glance is not a completion wait:\n%s", elapsed, got)
		}
		if strings.Contains(got, "PTY_TAIL") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("the pty tail lost its content:\n%s", got)
}

// timeout:0 KEEPS MEANING "DO NOT WAIT" ON EVERY READ PATH.
//
// The wait was added to a branch that never blocked before, so the one way it could
// regress is by ignoring the caller's own zero window.
func TestATailReadWithZeroTimeoutDoesNotWait(t *testing.T) {
	s := newTestServer(t)
	h := startDelayedPipe(t, s, 3, "NEVER_MIND")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	began := time.Now()
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 5", "timeout": 0})
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("timeout:0 waited %v:\n%s", elapsed, got)
	}
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Errorf("nothing has been printed yet, so the answer is 204:\n%s", got)
	}
}

// TestAChannelReadRefusesAFileOrFrameFlag is the guard against the pattern this
// repository forbids: a verb that declares a flag for one of its two targets and
// silently ignores it for the other.
//
// `read` has exactly that shape — the file form gained --mode and --lines, the peer
// form has --since/--at/--bytes — so one refusal table decides whether
// `read <channel> --mode hex` means "give me bytes" or "give me the default text
// projection and let the caller find out later". The first two cases are the
// regression: without the table they succeed with the wrong answer.
func TestAChannelReadRefusesAFileOrFrameFlag(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	for _, tc := range []struct{ flag, mustName string }{
		{"--mode hex", "read <session> file <path> --mode"},
		{"--lines", "read <session> file <path> --lines"},
		{"--since 3", "cdp|mcp|jrpc --since N"},
	} {
		got := termText(t, s, map[string]any{"cmd": "read " + h + " " + tc.flag})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("%s on a channel must be refused, not ignored:\n%s", tc.flag, got)
			continue
		}
		// A refusal that only says "no" sends the caller guessing; it names the form
		// where the flag does mean something.
		if !strings.Contains(got, tc.mustName) {
			t.Errorf("%s refusal does not name the form that uses it (%q):\n%s", tc.flag, tc.mustName, got)
		}
		if !strings.Contains(got, "not a channel") {
			t.Errorf("%s refusal does not say what it was applied to:\n%s", tc.flag, got)
		}
	}
}
