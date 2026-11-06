package mcp

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestARefusedReadKeepsTheBytesItRefused pins issue #200.
//
// The stream is read BEFORE the reply is rendered, and reading commits: the reader's
// cursor moves to the end of the window. So a refusal that came after the read answered
// `the window contains bytes that are not valid UTF-8 … pass --encoding gbk` and had
// already thrown those bytes away — the caller did what the error said and got 204,
// with the byte count still visible on `get`.
//
// That is a rejection whose own remedy cannot work, and on a Chinese Windows it is the
// DEFAULT path: powershell.exe writes the console code page, not UTF-8.
//
// What must hold: the refusal leaves the window where it was, so the same call with the
// charset the error named returns the bytes.
func TestARefusedReadKeepsTheBytesItRefused(t *testing.T) {
	// GBK for 中文: D6 D0 CE C4. Written as raw bytes on both platforms, because a
	// quoting layer that turned them into text would be testing something else.
	// ONE PROGRAM PER --argv, so the shell syntax goes through `sh -c` / `-Command`.
	write := "sh -c " + `"printf '\326\320\316\304'; sleep 30"`
	if runtime.GOOS == "windows" {
		write = "powershell -NoProfile -Command " +
			`"[Console]::OpenStandardOutput().Write([byte[]](0xD6,0xD0,0xCE,0xC4),0,4); Start-Sleep 30"`
	}
	s := newTestServer(t)
	// THE COMMAND ARRIVES AS ONE VALUE. Written into `cmd` it would be split at the `;`,
	// which is this language's statement separator — the reason a shell line is a VALUE
	// here and not something the parser reads.
	opened := termText(t, s, map[string]any{
		"cmd":     "connect internal --argv $1",
		"args":    []any{write},
		"timeout": float64(3),
	})
	ch := ""
	for _, line := range strings.Split(opened, "\n") {
		if h, ok := strings.CutPrefix(line, "Handle: "); ok {
			ch = strings.TrimSpace(h)
		}
	}
	if ch == "" {
		t.Fatalf("could not open a pipe channel; reply was:\n%s", opened)
	}

	// WAIT FOR THE BYTES AS A STEP OF ITS OWN, AND GIVE IT ITS OWN DEADLINE
	// (see docs/agents/testing.md, "Waiting is a state, not a duration").
	//
	// `read` answers 204 when nothing was available inside ITS window. On a loaded
	// Windows runner, PowerShell takes seconds to start and write those four bytes,
	// so one 2-second read says "nothing yet" — and the older version of this test
	// read that as "the charset refusal is broken". That is the flake: a bounded
	// probe was being asked to carry an unbounded process start.
	deadline := time.Now().Add(60 * time.Second)
	var first string
	for {
		probe := termText(t, s, map[string]any{"cmd": "read " + ch, "timeout": float64(1)})
		if !strings.Contains(probe, "204 No Content") {
			first = probe
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command produced no output within 60s, so the refusal cannot be observed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The refusal has to name the remedy, or there is nothing to test.
	if !strings.Contains(first, "not valid UTF-8") {
		t.Fatalf("the read should have been refused for its charset, got:\n%s", first)
	}
	if !strings.Contains(first, "gbk") {
		t.Fatalf("the refusal should name the charset it recognised, got:\n%s", first)
	}

	second := termText(t, s, map[string]any{"cmd": "read " + ch + " --encoding gbk", "timeout": float64(60)})
	if strings.Contains(second, "204 No Content") {
		t.Fatalf("the refused window was consumed, so the remedy the refusal named cannot work:\n%s", second)
	}
	if !strings.Contains(second, "中文") {
		t.Fatalf("the second read should deliver the bytes the first one refused, got:\n%s", second)
	}
}
