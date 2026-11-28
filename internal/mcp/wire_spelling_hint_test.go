package mcp

import (
	"errors"
	"strings"
	"testing"

	filesftp "github.com/open-mcp-ai/termcp/internal/sftp"
	pkgsftp "github.com/pkg/sftp"
)

// A WINDOWS PATH HAS TWO SPELLINGS AND ONLY ONE WORKS ON THE WIRE (#186).
//
// SFTP carries POSIX-shaped paths, so the absolute form of `C:/tc/x` is `/C:/tc/x`.
// A server that resolves relative paths against a working directory applies
// path.IsAbs — POSIX, so `C:/tc/x` is RELATIVE — and joins the two. Measured on a
// relay-backed Windows session:
//
//	C:/tc/Diana/go.mod -> open C:\Users\Administrator\C:\tc\Diana\go.mod: ...
//	                      The filename, directory name, or volume label syntax is incorrect.
//
// The caller reads that as "my file name is bad". Nothing in it says the path is
// fine and the SPELLING is not, and it arrives after the extension whitelist has
// already admitted the path — so it looks like a fault in the layer above.
func TestADrivePathFailureNamesTheWireSpelling(t *testing.T) {
	got := toolResultText(fileErr(errors.New(
		`open remote file: sftp: "open C:\Users\Administrator\C:\tc\Diana\go.mod: The filename, `+
			`directory name, or volume label syntax is incorrect." (SSH_FX_FAILURE)`), "C:/tc/Diana/go.mod"))
	if !strings.Contains(got, "/C:/tc/Diana/go.mod") {
		t.Errorf("the failure did not name the spelling that works:\n%s", got)
	}
	if !strings.Contains(got, "paths") {
		t.Errorf("the hint did not mention the extension whitelist, so a caller who fixes only "+
			"the request gets a 403 next:\n%s", got)
	}
}

// AND IT IS OFFERED ONLY WHERE IT COULD BE THE ANSWER. A 404 or a 403 is a verdict
// about a path the server COULD resolve; appending "maybe your spelling" to it would
// be noise on the common case (a file that really is not there).
func TestTheSpellingHintIsNotAddedToAPlainMiss(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not found", pkgsftp.ErrSSHFxNoSuchFile},
		{"permission", pkgsftp.ErrSSHFxPermissionDenied},
		{"a directory", filesftp.ErrIsDirectory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolResultText(fileErr(tc.err, "C:/tc/Diana/go.mod"))
			if strings.Contains(got, "Hint:") {
				t.Errorf("a resolvable-path verdict carries no spelling advice:\n%s", got)
			}
		})
	}
}

// A PATH THAT IS ALREADY IN WIRE FORM GETS NOTHING: it is not the mistake, and a
// hint that fires on correct input teaches the caller to ignore hints.
func TestAnAlreadyCorrectPathIsNotHintedAt(t *testing.T) {
	for _, p := range []string{"/C:/tc/Diana/go.mod", "/tmp/x", "relative/x", "Diana/go.mod", ""} {
		if h := wireSpellingHint(p); h != "" {
			t.Errorf("wireSpellingHint(%q) = %q, want nothing", p, h)
		}
	}
}

// The shape test itself, because the hint is only as good as what it recognises.
func TestWhatLooksLikeADrivePath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"C:/tc/x", true},
		{`C:\tc\x`, true},
		{"c:/x", true},
		{"Z:", true},
		{"/C:/tc/x", false}, // already the wire form
		{"/tmp/x", false},
		{"relative/x", false},
		{"", false},
		{"C", false},
		{"1:/x", false},
	} {
		if got := looksLikeWindowsDrivePath(tc.in); got != tc.want {
			t.Errorf("looksLikeWindowsDrivePath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// THE FIX IS THE FIRST LINE (#357 §1.5).
//
// The hint used to be appended, so the top of the failure — the line a log shows and a
// reader skims — was the server's error text, and the sentence that says what to write
// instead came after it. A caller who read only the top concluded the file NAME was bad.
func TestTheSpellingHintIsTheFirstLine(t *testing.T) {
	got := toolResultText(fileErr(errors.New(
		`open remote file: sftp: "open C:\Users\Administrator\C:\tc\Diana\go.mod: The filename, `+
			`directory name, or volume label syntax is incorrect." (SSH_FX_FAILURE)`), "C:/tc/Diana/go.mod"))
	first, _, _ := strings.Cut(got, "\n")
	if !strings.Contains(first, "/C:/tc/Diana/go.mod") {
		t.Fatalf("the spelling that works is not on the first line:\n%s", got)
	}
	if !strings.Contains(first, "Hint:") {
		t.Errorf("the first line does not read as advice:\n%s", got)
	}
}
