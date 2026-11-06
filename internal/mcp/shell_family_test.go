package mcp

import (
	"strings"
	"testing"
)

// TestGetSessionNamesTheShellFamily pins issue #208.1.
//
// A caller writing its next command needs to know whether `ls -la` is a listing or a
// PowerShell alias failure, and the only way to find out used to be to run something
// and read the error. The session already detected this to decide its line ending, so
// the fact was there — it just was not said.
func TestGetSessionNamesTheShellFamily(t *testing.T) {
	s := newTestServer(t)
	out := termText(t, s, map[string]any{"cmd": "connect internal"})
	h := handleFrom(t, out)
	sid := strings.Split(h, ":")[0]
	defer termText(t, s, map[string]any{"cmd": "close " + sid})

	got := termText(t, s, map[string]any{"cmd": "get " + sid})
	if !strings.Contains(got, "shell_family:") {
		t.Errorf("get <session> does not name the shell family:\n%s", got)
	}
	// "family (binary)" — the family decides the syntax, the binary decides the flags.
	if !strings.Contains(got, "(") || !strings.Contains(got, ")") {
		t.Errorf("shell_family should read 'family (binary)':\n%s", got)
	}
}
