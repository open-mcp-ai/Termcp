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
