//go:build windows

package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestManifestWriteSurvivesAReaderHoldingTheFile is the regression test for the
// flake this fix was extracted from: a shell's exit status stayed "running" on
// disk while memory said exited, so a restart resurrected a finished shell as a
// live one.
//
// The mechanism is Windows-specific. A manifest is published by renaming a temp
// file over the previous one, which on POSIX succeeds even while a reader has the
// old file open, but on Windows fails with "Access is denied". Publishing a
// manifest is exactly the shape that triggers it: the target always exists, and
// reading these manifests from outside the process is normal (they are plain JSON
// on disk; this project's own session test polls them to wait for a write).
//
// The reader here holds the manifest open for a bounded time - the duration of a
// read, not forever - and the write must wait it out and then publish. That makes
// the case deterministic rather than a race to be lucky in: with the retry
// removed the very first rename lands inside the hold and the test fails with the
// same "Access is denied" the flake showed, while with the retry it succeeds once
// the handle closes.
//
// The hold is longer than one backoff step but well inside the retry budget, so
// the test also pins that budget: a retry that gave up after one short sleep would
// fail here.
func TestManifestWriteSurvivesAReaderHoldingTheFile(t *testing.T) {
	st := newStore(t)
	sess := api.Session{ID: "reader-window-1", Name: "reader-window-1", Status: api.SessionRunning}
	if err := st.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.sessionDir(sess.ID), "manifest.json")

	// Hold the file open the way a reader does: open, then close after a bounded
	// delay. os.Open does not grant FILE_SHARE_DELETE, so while this handle is
	// open a rename over the file is refused.
	hold, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const holdFor = 10 * time.Millisecond
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(holdFor)
		hold.Close()
	}()
	defer func() {
		<-released
	}()

	sess.Status = api.SessionExited
	started := time.Now()
	if err := st.SaveSession(sess); err != nil {
		t.Fatalf("publishing the exited status while a reader holds the manifest failed: %v", err)
	}
	took := time.Since(started)

	onDisk, err := st.readManifest(st.sessionDir(sess.ID))
	if err != nil {
		t.Fatalf("read back the manifest: %v", err)
	}
	if onDisk.Status != api.SessionExited {
		t.Fatalf("the write reported success but disk still says %q; a lost update is the bug", onDisk.Status)
	}
	if took < holdFor {
		t.Errorf("the write finished in %v, faster than the %v the reader held the file; it cannot have been "+
			"refused and retried, so this test is not exercising the sharing window", took, holdFor)
	}
	t.Logf("published the exited status after %v (reader held the file for %v)", took, holdFor)
}

// TestDeleteSessionSurvivesAReaderHoldingALogFile pins the removal half of the
// sharing-violation fix: on Windows an open read handle on log.bin causes RemoveAll
// to fail with "The process cannot access the file because it is being used by another
// process", which previously surfaced as "删除失败".
//
// The reader holds log.bin open for a bounded time (shorter than removeAttempts'
// backoff budget), exactly as an in-flight output-range fetch, transcript preview,
// or file scanner does; DeleteSession must wait it out and remove the tree.
func TestDeleteSessionSurvivesAReaderHoldingALogFile(t *testing.T) {
	st := newStore(t)
	const sess, shell = "held-sess-1", "held-shell-1"
	if err := st.SaveSession(api.Session{ID: sess, Name: sess}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, []byte("log contents"), StdioOut); err != nil {
		t.Fatal(err)
	}

	logPath := st.LogPath(sess, shell)
	held, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	const holdFor = 25 * time.Millisecond
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(holdFor)
		held.Close()
	}()
	defer func() { <-released }()

	started := time.Now()
	if err := st.DeleteSession(sess); err != nil {
		t.Fatalf("DeleteSession while reader holds log failed: %v", err)
	}
	took := time.Since(started)

	if _, err := os.Stat(st.sessionDir(sess)); !os.IsNotExist(err) {
		t.Fatalf("session dir still present after DeleteSession: %v", err)
	}
	if took < holdFor {
		t.Errorf("delete took %v, faster than the %v hold; sharing window was not exercised", took, holdFor)
	}
}
