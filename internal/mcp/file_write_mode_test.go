package mcp

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE FILE HANDLER WRITES --data AND NOTHING ELSE.
//
// It used to also accept local_path, and read that path on the daemon's own host.
// Nothing checked whether the caller could touch that host: --no-internal hides the
// loopback profile so an MCP client cannot reach the machine termcp runs on, and this
// argument walked around the refusal with the file's contents arriving as the reply.
//
// The argument is gone rather than deprecated — there is no message to read about it,
// and nothing to migrate: an AI re-reads the reference each turn. This test pins the
// part that matters, which is that no spelling of it reaches the handler.
func TestTheFileHandlerCannotReachTheDaemonFilesystem(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)

	for _, cmd := range []string{
		"write " + sid + " file /tmp/out.bin --local-path /etc/hosts",
		"write " + sid + " file /tmp/out.bin --local-path=/etc/hosts",
		"write " + sid + " file /tmp/out.bin --data hi --mode file",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": cmd})
			if !statusLineIs(got, "400") {
				t.Fatalf("reached the handler instead of being refused:\n%s", got)
			}
		})
	}
}

// STAT HAS TO ANSWER "IS THIS EXECUTABLE" (#191). A script that had just been written
// failed to run, and every surface that could have said "mode 664" reported size and
// mtime instead: the caller went looking at mounts, users and paths while the fact sat
// in the stat response nobody had asked for.
func TestStatReportsTheModeOfAFile(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	// THE PATH IS THE TEST'S OWN TEMP DIR, not /tmp. On a Windows runner /tmp resolves to
	// D:\tmp, which does not exist, so the write was refused with "the directory /tmp does
	// not exist" — a green local suite (this machine has C:\tmp) and a red windows leg.
	// t.TempDir is the repo's rule for exactly this reason: never hardcode a shared
	// absolute path.
	path := filepath.Join(t.TempDir(), "stat-mode-probe.sh")
	if got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + path + " --data hi",
	}); !statusLineIs(got, "200") {
		t.Fatalf("write:\n%s", got)
	}
	defer termText(t, s, map[string]any{"cmd": "rm " + sid + " file " + path})

	got := termText(t, s, map[string]any{
		"cmd": "get " + sid + " file " + path + " --op stat",
	})
	if !statusLineIs(got, "200") {
		t.Fatalf("stat:\n%s", got)
	}
	// Four octal digits, e.g. "0644": the spelling a caller passes to chmod.
	if !strings.Contains(got, "mode: 0") {
		t.Fatalf("stat did not report the file's mode:\n%s", got)
	}
}
