package mcp

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sftp"
)

// TestWireSpellingIsNarrow pins the boundary of the rewrite, case by case.
//
// The table is the contract: what gets rewritten and — more importantly — what does
// NOT. `\` is an ordinary byte in a POSIX filename, so a rule that rewrote every
// backslash would silently read a different file, and a silent wrong answer costs far
// more than an error (issue #186).
func TestWireSpellingIsNarrow(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		adapts bool
		why    string
	}{
		{`C:\tc\a.txt`, `/C:/tc/a.txt`, true, "the spelling a caller copies out of Explorer"},
		{`c:\tc\a.txt`, `/c:/tc/a.txt`, true, "the drive letter is not case-sensitive"},
		{`C:\tc`, `/C:/tc`, true, "a bare drive root"},
		{`/C:\tc\a.txt`, `/C:/tc/a.txt`, true, "absolute and wire-shaped, separators still wrong"},
		{`C:\tc/a.txt`, `/C:/tc/a.txt`, true, "mixed separators are one spelling"},
		{`C:/tc/a.txt`, `C:/tc/a.txt`, false, "no backslash: a relative path on a POSIX host is plausible"},
		{`/C:/tc/a.txt`, `/C:/tc/a.txt`, false, "already the wire form"},
		{`./C:\tc\a.txt`, `./C:\tc\a.txt`, false, "the escape hatch: this one really is a filename"},
		{`/tmp/x/C:\tc.txt`, `/tmp/x/C:\tc.txt`, false, "a name containing a backslash, reached by absolute path"},
		{`C:tc\a.txt`, `C:tc\a.txt`, false, "drive-relative means a different path"},
		{`dir\file.txt`, `dir\file.txt`, false, "no drive: cannot be told from a POSIX filename"},
		{`\\host\share\a.txt`, `\\host\share\a.txt`, false, "a UNC path is not a drive path"},
		{``, ``, false, "the empty path is not a path"},
	}
	for _, tc := range cases {
		got, adapted := sftp.WireSpelling(tc.in)
		if got != tc.want || adapted != tc.adapts {
			t.Errorf("sftp.WireSpelling(%q) = %q, %v; want %q, %v (%s)", tc.in, got, adapted, tc.want, tc.adapts, tc.why)
		}
	}
}

// A LOCAL PATH IS NOT A REMOTE PATH.
//
// `local_path` is opened by this process — a download's destination, an upload's source
// — so a Windows path there is already correct and rewriting it to the wire form would
// point at a file that does not exist.
func TestALocalPathIsNotAdapted(t *testing.T) {
	args := map[string]any{"remote_path": `C:\tc\a.txt`, "local_path": `C:\tmp\out.bin`}
	adapted := adaptCallPaths(lookupGate("file_read"), args)
	if got := args["remote_path"]; got != "/C:/tc/a.txt" {
		t.Errorf("remote_path = %v, want the wire form", got)
	}
	if got := args["local_path"]; got != `C:\tmp\out.bin` {
		t.Errorf("local_path = %v, want it untouched: it names a path on the caller's machine", got)
	}
	// Exactly one adaptation — the remote path. The note names the two spellings, not
	// the argument key, so the count is what this asserts.
	if len(adapted) != 1 {
		t.Errorf("adapted = %v, want exactly one entry (the remote path)", adapted)
	}
}

// THE WHITELIST AND THE REQUEST AGREE ON ONE SPELLING.
//
// This is the whole reason the rewrite happens before admission rather than inside the
// handler: adapting one side only would either refuse a path the template granted, or
// authorise a spelling the rule never named. The extension here grants `/C:/tc` and the
// caller types `C:\tc\a.txt`; the assertion is about the ANSWER, not about the file,
// because what is being tested is which string the rule was matched against.
func TestAWindowsSpellingMatchesAWhitelistRule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rewrite is exercised against a POSIX wire form; Windows already spells it that way")
	}
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "spell", "target": "internal",
		"paths": []any{"/C:/tc"}, "enabled": true,
	})
	got := useExtension(t, s, "spell")
	sid, _ := got["session_id"].(string)
	if sid == "" {
		t.Fatalf("extension has no session: %v", got)
	}

	// The file is not there, so this ends in a 404 — which is the point: a 403 would
	// mean the whitelist compared the typed spelling and refused a path it granted.
	out := termText(t, s, map[string]any{"cmd": `read ` + sid + ` file 'C:\tc\a.txt'`, "timeout": 10})
	if strings.Contains(out, "TERM/1 403") {
		t.Fatalf("a granted tree was refused for its spelling:\n%s", out)
	}
	if !strings.Contains(out, `C:\tc\a.txt -> /C:/tc/a.txt`) {
		t.Errorf("the reply does not say the spelling was adapted:\n%s", out)
	}

	// AND THE ADAPTATION GRANTS NOTHING NEW: the same call against a tree the template
	// never declared is still refused.
	// AN EDIT REBUILDS THE CHANNEL: the template's fingerprint changed, so the old
	// session is not the one the new whitelist governs. Ask again for a handle rather
	// than reading through a stale one — that would test the fixture, not the rule.
	callExtension(t, s, map[string]any{
		"action": "update", "name": "spell", "paths": []any{"/srv/other"},
	})
	again := useExtension(t, s, "spell")
	sid, _ = again["session_id"].(string)
	if sid == "" {
		t.Fatalf("extension has no session after the edit: %v", again)
	}
	out = termText(t, s, map[string]any{"cmd": `read ` + sid + ` file 'C:\tc\a.txt'`, "timeout": 10})
	if !strings.Contains(out, "TERM/1 403") {
		t.Errorf("a path outside the declared tree was not refused:\n%s", out)
	}
}

// A POSIX NAME THAT CONTAINS A BACKSLASH IS STILL REACHABLE, by the spelling that says
// so. This is the case the narrow rule exists to protect, and it is a real one on this
// project's own platforms: a file named `C:\tc.txt` can be created and read.
func TestAPosixNameWithABackslashIsReachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a filename cannot contain a backslash on Windows")
	}
	s := newTestServer(t)
	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	dir := t.TempDir()
	name := dir + `/C:\tc.txt`
	if err := os.WriteFile(name, []byte("LITERAL-BACKSLASH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := termText(t, s, map[string]any{"cmd": "read " + h + " file " + name, "timeout": 10})
	if !strings.Contains(out, "LITERAL-BACKSLASH") {
		t.Fatalf("a file whose name contains a backslash became unreachable:\n%s", out)
	}
	if strings.Contains(out, "path_adapted") {
		t.Errorf("an absolute path with no drive prefix was rewritten:\n%s", out)
	}
}

// A PIPELINE DOES NOT GO THROUGH A TOOL CALL, so it needs the same rewrite by hand.
//
// Without it, `read … file C:\x | write … file C:\y` would be the one file door where
// the spelling is not adapted — the same path working as `read … file` and failing as a
// pipeline, which is the kind of difference a caller only finds out about after the
// bytes have gone to the wrong place.
func TestAPipelineAdaptsBothEnds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rewrite is exercised against a POSIX wire form")
	}
	s := newTestServer(t)
	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{
		"cmd":     `read ` + h + ` file 'C:\tc\a.txt' | write ` + h + ` file 'C:\tc\b.txt'`,
		"timeout": 10,
	})
	if !strings.HasPrefix(out, "TERM/1 202") {
		t.Fatalf("the pipeline did not start:\n%s", out)
	}
	for _, want := range []string{`C:\tc\a.txt -> /C:/tc/a.txt`, `C:\tc\b.txt -> /C:/tc/b.txt`} {
		if !strings.Contains(out, want) {
			t.Errorf("the pipeline receipt is missing %q:\n%s", want, out)
		}
	}
}
