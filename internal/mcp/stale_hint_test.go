package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADirectoryReadListsAndNamesNoDeadFlag is issue #146's pin, rewritten by #340.
//
// ORIGINAL CONTRACT (still true): a message a caller is given must name a spelling
// this build actually answers. The old refusal pointed at `--action stat`, a flag the
// command-surface rewrite had deleted, so following the hint produced a second error.
//
// WHAT CHANGED: `read <session> file <dir>` no longer refuses at all — it lists the
// directory (#340). A caller that does not know whether its path is a file or a
// directory does not have to find out, and the refusal's two sentences (with the
// `run ls` detour they implied) are gone with it. So this test now pins the listing
// AND keeps the part that must never regress: no message names `--action`, and the
// `--op stat` spelling a caller may still reach for keeps working.
func TestADirectoryReadListsAndNamesNoDeadFlag(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	listed := termText(t, s, map[string]any{"cmd": "read " + h + " file " + dir})
	if code := statusCode(t, listed); code >= 400 {
		t.Fatalf("reading a directory must list it, got %d:\n%s", code, listed)
	}
	for _, want := range []string{"(directory)", "a.txt", "sub/"} {
		if !strings.Contains(listed, want) {
			t.Errorf("the listing misses %q:\n%s", want, listed)
		}
	}
	// The removed spelling must not come back: it is the one word a caller cannot act on.
	if strings.Contains(listed, "--action") {
		t.Errorf("the reply still names --action, which no directive accepts:\n%s", listed)
	}

	// And the spelling a caller may still use for a stat has to work.
	stat := termText(t, s, map[string]any{"cmd": "get " + h + " file " + dir + " --op stat"})
	if code := statusCode(t, stat); code >= 400 {
		t.Errorf("`--op stat` answered %d; that spelling must keep working:\n%s", code, stat)
	}
}
