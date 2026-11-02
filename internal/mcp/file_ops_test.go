package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestEveryWriteOpReachesTheFile drives each `write … file --op` and checks the file
// system actually changed.
//
// The ops were almost entirely untested through the directive: chmod, chown, chtimes,
// truncate and upload had no test that named them, so a flag dropped between the table
// and the handler would have gone unnoticed — which is exactly how `--op urls` and
// `create … file` were both dead while the suite was green.
func TestEveryWriteOpReachesTheFile(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]
	dir := t.TempDir()

	write := func(cmd string) string {
		t.Helper()
		got := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(got, "TERM/1 2") {
			t.Fatalf("%q was refused:\n%s", cmd, got)
		}
		return got
	}

	t.Run("data", func(t *testing.T) {
		p := filepath.Join(dir, "data.txt")
		write("write " + sid + " file " + p + " --data hello")
		if b, _ := os.ReadFile(p); string(b) != "hello" {
			t.Errorf("content = %q, want hello", b)
		}
	})

	t.Run("truncate", func(t *testing.T) {
		p := filepath.Join(dir, "trunc.txt")
		if err := os.WriteFile(p, []byte("0123456789"), 0644); err != nil {
			t.Fatal(err)
		}
		write("write " + sid + " file " + p + " --op truncate --size 4")
		if b, _ := os.ReadFile(p); string(b) != "0123" {
			t.Errorf("after truncate = %q, want 0123", b)
		}
	})

	t.Run("append-via-offset", func(t *testing.T) {
		p := filepath.Join(dir, "off.txt")
		if err := os.WriteFile(p, []byte("abc"), 0644); err != nil {
			t.Fatal(err)
		}
		write("write " + sid + " file " + p + " --data XYZ --offset 3")
		if b, _ := os.ReadFile(p); string(b) != "abcXYZ" {
			t.Errorf("after offset write = %q, want abcXYZ", b)
		}
	})

	t.Run("chmod", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permissions do not exist on Windows")
		}
		p := filepath.Join(dir, "mode.txt")
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		// --mode is DECIMAL permission bits, the way the handler documents it
		// (493 = 0755): 416 = 0o640. Writing `--mode 640` asks for 0o1200, which the
		// kernel masks down to nothing — a wrong test, not a wrong flag.
		write("write " + sid + " file " + p + " --op chmod --mode 416")
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0640 {
			t.Errorf("mode = %o, want 640", got)
		}
	})

	t.Run("chown", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no uid/gid on Windows")
		}
		// The op must reach the handler even when the kernel refuses the change: an
		// unprivileged user cannot chown, and what this test pins is that --uid/--gid
		// are CARRIED, which shows in the reply naming them rather than in errno.
		p := filepath.Join(dir, "own.txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		got := termText(t, s, map[string]any{"cmd": "write " + sid + " file " + p + " --op chown --uid 0 --gid 0"})
		if strings.Contains(got, "unknown") || strings.Contains(got, "Usage:") {
			t.Fatalf("--uid/--gid were not understood:\n%s", got)
		}
	})

	t.Run("chtimes", func(t *testing.T) {
		p := filepath.Join(dir, "times.txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		got := termText(t, s, map[string]any{"cmd": "write " + sid + " file " + p + " --op chtimes --atime 1600000000 --mtime 1600000001"})
		if strings.Contains(got, "unknown") || strings.Contains(got, "Usage:") {
			t.Fatalf("--atime/--mtime were not understood:\n%s", got)
		}
		if !strings.HasPrefix(got, "TERM/1 2") && !strings.Contains(got, "Error-Code") {
			t.Fatalf("unexpected reply:\n%s", got)
		}
	})

	t.Run("copy-via-pipeline", func(t *testing.T) {
		src := filepath.Join(dir, "src.txt")
		if err := os.WriteFile(src, []byte("uploaded body"), 0644); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, "dst.txt")
		// A PIPELINE NOW, NOT --local-path. The flag read the daemon's disk without a
		// capability check, which is what --no-internal exists to stop; the pipeline
		// goes through a session, so the isolation flag can refuse it.
		res := write("read " + sid + " file " + src + " | write " + sid + " file " + dst)
		// A COPY ANSWERS WITH A RECEIPT (202 Accepted, #158). Wait for the task
		// using `get task <id>` — the way a caller does.
		if taskID := headerValueOr(res, "Task", ""); taskID != "" {
			termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 5})
		}
		if b, _ := os.ReadFile(dst); string(b) != "uploaded body" {
			t.Errorf("copied content = %q, want %q", b, "uploaded body")
		}
	})
}

// TestSizeFlagSetsTheTerminalGeometry covers --size on both verbs that accept it.
//
// `--size 30x100` is rows x cols — the order a terminal is described in, not the order
// a screenshot is. Nothing tested it, so a transposition would have been invisible:
// both numbers are valid, and the geometry would simply be wrong.
func TestSizeFlagSetsTheTerminalGeometry(t *testing.T) {
	s := newTestServer(t)

	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	resized := termText(t, s, map[string]any{"cmd": "open " + sid + " --size 30x100"})
	if !strings.HasPrefix(resized, "TERM/1 2") {
		t.Fatalf("--size on open was refused:\n%s", resized)
	}

	// connect takes the same flag, on a session of its own.
	fresh := termText(t, s, map[string]any{"cmd": "connect internal --size 24x80"})
	if !strings.HasPrefix(fresh, "TERM/1 2") {
		t.Fatalf("--size on connect was refused:\n%s", fresh)
	}
	if id := firstHandle(fresh); id != "" {
		defer termText(t, s, map[string]any{"cmd": "close " + id})
	}
}

// firstHandle pulls the handle out of a reply header.
func firstHandle(reply string) string {
	for _, line := range strings.Split(reply, "\n") {
		if v, ok := strings.CutPrefix(line, "Handle:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// TestGrepLimitCapsTheMatches pins that --grep-limit is not silently ignored.
//
// A limit that is accepted and dropped looks exactly like a result set that happened
// to be small enough, so the only way to see it work is to ask for fewer matches than
// exist and count what comes back.
func TestGrepLimitCapsTheMatches(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// Five matching lines. A plain while loop, not `seq`: the test runs on Windows CI
	// too, where the shell has no seq and the command would fail having printed
	// nothing — which the wait below would report as a timeout, not as the cause.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{
		"i=0; while [ $i -lt 5 ]; do i=$((i+1)); echo marker-line-$i; done",
	}})

	// Poll with --grep, not the whole transcript: reading from line 1 on every
	// iteration made this test spend seconds in the wait on a slow shell.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + h + " --grep marker-line-5"}), "marker-line-5") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command's output never appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// COUNT THE MATCHES, NOT THE WORD. The pty echoes the command that was typed, and
	// that line contains the string "marker-line-$i" — so counting "marker-line-" finds
	// the echo as well and reports one match too many.
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --grep marker-line --grep-limit 2"})
	if n := len(grepMatch.FindAllString(got, -1)); n > 2 {
		t.Errorf("--grep-limit 2 returned %d matches:\n%s", n, got)
	}
}

// grepMatch is a real match: the marker followed by a digit, which the echoed command
// line (marker-line-$i) is not.
var grepMatch = regexp.MustCompile(`marker-line-[0-9]`)
