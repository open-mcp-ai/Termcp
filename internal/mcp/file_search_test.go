package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A FILE GREP THAT SILENTLY DID NOTHING (issue #357 §1.1).
//
// `read <session> file <path> --grep RE` parsed the flag, never passed it to the handler,
// and answered with the whole file. The measured reply before this was the file's own bytes
// with no indication that the filter had been dropped — the "confidently correct answer
// about something else" this project refuses to ship.
func writeSearchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	must(os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nbeta NEEDLE\ngamma\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("x\nNEEDLE here\ny\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "deep", "c.txt"), []byte("nothing\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "deep", "d.log"), []byte("NEEDLE in log\n"), 0o644))
	return root
}

func TestAFileGrepFiltersThatFile(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := writeSearchTree(t)

	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + filepath.Join(root, "a.txt") + " --grep NEEDLE", "timeout": 10,
	})
	if strings.Contains(got, "alpha") || strings.Contains(got, "gamma") {
		t.Fatalf("the file came back unfiltered:\n%s", got)
	}
	if !strings.Contains(got, "2:beta NEEDLE") {
		t.Fatalf("the match is not reported with its line number:\n%s", got)
	}
}

// A TREE SEARCH WALKS BY DEFAULT, and answers in grep's own shape.
//
// IT WALKS DEEPER THAN A LISTING ON PURPOSE. `read <dir>` lists one level, which is the
// right default for "what is in this directory"; a search that stopped there would answer
// `0 matches` about a tree that has them, and a caller would have to know to pass --depth.
func TestATreeGrepFindsMatchesAtDepth(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := writeSearchTree(t)

	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --grep NEEDLE", "timeout": 15,
	})
	for _, want := range []string{"a.txt:2:beta NEEDLE", "b.go:2:NEEDLE here", "d.log:1:NEEDLE in log"} {
		if !strings.Contains(got, want) {
			t.Errorf("a match at depth is missing (%q):\n%s", want, got)
		}
	}
	// THE TRAILER IS WHAT MAKES A SHORT ANSWER TRUSTWORTHY: it names how many files were
	// opened, so "no match" and "did not look" cannot arrive in the same shape.
	if !strings.Contains(got, "file(s) under") {
		t.Errorf("the search does not report what it walked:\n%s", got)
	}
}

func TestGlobLimitsWhichFilesAreSearched(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := writeSearchTree(t)

	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --grep NEEDLE --glob '*.go'", "timeout": 15,
	})
	if !strings.Contains(got, "b.go:2:NEEDLE here") {
		t.Fatalf("the glob excluded the file it names:\n%s", got)
	}
	if strings.Contains(got, "a.txt:2") || strings.Contains(got, "d.log:1") {
		t.Errorf("--glob did not exclude the other files:\n%s", got)
	}
}

// FIND IS THE OTHER QUESTION, and it is answered without reading any file.
func TestNameFindsPathsWithoutReadingThem(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := writeSearchTree(t)

	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --name '*.log'", "timeout": 15,
	})
	if !strings.Contains(got, "d.log") {
		t.Fatalf("--name did not find the path:\n%s", got)
	}
	if strings.Contains(got, "NEEDLE in log") {
		t.Errorf("--name read the file's contents instead of its name:\n%s", got)
	}
}

func TestContextKeepsTheLinesAroundAMatch(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := writeSearchTree(t)

	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + filepath.Join(root, "a.txt") +
			" --grep NEEDLE --context 1", "timeout": 10,
	})
	if !strings.Contains(got, "1-alpha") || !strings.Contains(got, "3-gamma") {
		t.Fatalf("--context did not keep the surrounding lines:\n%s", got)
	}
}

// A SEARCH READS FILES THE CALLER NEVER NAMED, so each one passes the SAME admission a
// single `read <session> file <path>` passes. Without that, a session whose paths rule
// names one file could search its parent directory and read every sibling: the named path
// was admitted, the paths the walk chose were never asked about (the same class as #358).
func TestASearchCannotReadOutsideTheWhitelist(t *testing.T) {
	s := newExtensionTestServer(t)
	root := writeSearchTree(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "ro357", "target": "internal",
		"paths": []any{root}, "enabled": true,
	})
	got := useExtension(t, s, "ro357")
	sid, _ := got["session_id"].(string)
	if sid == "" {
		t.Fatalf("extension has no session: %v", got)
	}
	// INSIDE THE RULE: the search works and finds what is there.
	inside := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --grep NEEDLE", "timeout": 15,
	})
	if !strings.Contains(inside, "a.txt:2:beta NEEDLE") {
		t.Fatalf("a search inside the whitelist did not run:\n%s", inside)
	}
	// AND EVERY MATCH IS INSIDE IT: the tree's other files are in subdirectories, so a
	// rule naming the root admits them. A second extension naming ONE FILE is what proves
	// the per-file gate below.
	callExtension(t, s, map[string]any{
		"action": "create", "name": "ro357b", "target": "internal",
		"paths": []any{filepath.Join(root, "sub", "b.go")}, "enabled": true,
	})
	gotB := useExtension(t, s, "ro357b")
	sidB, _ := gotB["session_id"].(string)
	if sidB == "" {
		t.Fatalf("second extension has no session: %v", gotB)
	}
	// The named file is reachable...
	ok := termText(t, s, map[string]any{
		"cmd": "read " + sidB + " file " + filepath.Join(root, "sub", "b.go") + " --grep NEEDLE", "timeout": 15,
	})
	if strings.Contains(ok, "TERM/1 403") {
		t.Fatalf("the file the rule names was refused:\n%s", ok)
	}
	// ...and a search of the parent directory must not become the way around it.
	searched := termText(t, s, map[string]any{
		"cmd": "read " + sidB + " file " + root + " --grep NEEDLE", "timeout": 15,
	})
	if strings.Contains(searched, "beta NEEDLE") || strings.Contains(searched, "NEEDLE in log") {
		t.Fatalf("a search read files the paths rule refuses:\n%s", searched)
	}
}

// AN EMPTY PATTERN MATCHES EVERY LINE, so it is refused rather than returning the file by
// another name.
func TestAnEmptyOrInvalidPatternIsRefused(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := writeSearchTree(t)

	empty := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --grep ''", "timeout": 10,
	})
	if !strings.Contains(empty, "TERM/1 400") {
		t.Errorf("an empty --grep was not refused:\n%s", empty)
	}
	bad := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --grep '('", "timeout": 10,
	})
	if !strings.Contains(bad, "TERM/1 400") || !strings.Contains(bad, "does not compile") {
		t.Errorf("an invalid pattern was not refused as one:\n%s", bad)
	}
}

// A SYMLINK INSIDE THE TREE IS NOT FOLLOWED, and that is a whitelist property rather than
// a convention: the link's own path is inside the directory the caller named, so admission
// admits it, and the READ is what follows it out. Following links would make a symlink the
// one way around the per-file check.
func TestASearchDoesNotFollowALinkOutOfTheTree(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET-NEEDLE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("REAL-NEEDLE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + root + " --grep NEEDLE", "timeout": 15,
	})
	if strings.Contains(got, "SECRET-NEEDLE") {
		t.Fatalf("the search followed a link out of the tree:\n%s", got)
	}
	if !strings.Contains(got, "REAL-NEEDLE") {
		t.Fatalf("the search did not find the file that is there:\n%s", got)
	}
	if !strings.Contains(got, "link") {
		t.Errorf("the reply does not say the link was skipped:\n%s", got)
	}
}
