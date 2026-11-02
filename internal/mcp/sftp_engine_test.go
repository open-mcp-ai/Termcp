package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestAFileOperationReusesTheSessionsEngine pins the fix for issue #244: the SFTP
// subsystem is opened once per session, not once per call.
//
// The assertion is on ENGINE IDENTITY, not elapsed time. Loopback latency is
// microseconds, so a timing test would pass either way — the 526 ms handshake the issue
// pays only exists over a real link. Comparing what the helper hands back is the same
// assertion with the network divided out: if a later call gets a different engine, the
// handshake was paid again.
func TestAFileOperationReusesTheSessionsEngine(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	dir := t.TempDir()
	path := filepath.Join(dir, "engine.txt")
	if got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + path + " --data hello",
	}); !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("write refused:\n%s", got)
	}

	first, bad := s.sftpClient(sid)
	if bad != nil {
		t.Fatalf("first client: %v", bad)
	}
	second, bad := s.sftpClient(sid)
	if bad != nil {
		t.Fatalf("second client: %v", bad)
	}
	if first != second {
		t.Error("two file operations on one session got two SFTP engines: the handshake " +
			"(channel open + subsystem + INIT) is being paid on every call (issue #244)")
	}

	// Close is a no-op on a shared engine, so every handler's existing
	// `defer sftpCli.Close()` cannot kill the next call. That is the contract the whole
	// change rests on — a dozen handlers still say Close, and this is where "still says
	// Close, still correct" is pinned.
	if err := first.Close(); err != nil {
		t.Errorf("Close on a shared engine returned %v; it must be a no-op", err)
	}
	if _, err := second.ReadFile(path, 0, 0, "text", "", "", true, false); err != nil {
		t.Errorf("the engine died after a caller closed its view: %v", err)
	}
}

// TestASessionDeathEvictsItsEngine keeps the cache bounded: a daemon that has served many
// sessions must not hold a subsystem and its SSH channel for each one for ever.
//
// The session is TERMINATED, not merely closed. `close <channel>` closes one channel and
// leaves the session running, so it never fires the DEAD hook — a test written against
// that event would wait for something that does not happen and prove nothing.
func TestASessionDeathEvictsItsEngine(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	sid := strings.SplitN(h, ":", 2)[0]

	if _, bad := s.sftpClient(sid); bad != nil {
		t.Fatalf("client: %v", bad)
	}
	s.sftpEngines.mu.Lock()
	_, cached := s.sftpEngines.m[sid]
	s.sftpEngines.mu.Unlock()
	if !cached {
		t.Fatal("the engine was not cached, so this test proves nothing")
	}

	if _, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": sid, "force": true,
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the engine to be evicted when its session died", func() bool {
		s.sftpEngines.mu.Lock()
		defer s.sftpEngines.mu.Unlock()
		_, still := s.sftpEngines.m[sid]
		return !still
	})
}

// TestTheEngineCacheIsSingleFlight pins that concurrent calls share one handshake rather
// than racing two — the reason an entry carries a `ready` channel instead of being a
// plain map assignment.
//
// It needs a REAL ssh.Client, because a nil one panics inside pkg/sftp before the sharing
// can be observed. Every caller asks for the same session at once and must come back with
// the same engine pointer: one handshake, eight waiters.
func TestTheEngineCacheIsSingleFlight(t *testing.T) {
	srv := startTestSSH(t)
	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, "inmem", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cli := ssh.NewClient(conn, chans, reqs)
	defer cli.Close()

	e := newSFTPEngines(nil)
	const callers = 8
	var wg sync.WaitGroup
	got := make([]any, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client, err := e.get("s1", cli)
			errs[i] = err
			if client != nil {
				got[i] = client
			}
		}(i)
	}
	wg.Wait()

	for i := range got {
		if errs[i] != nil {
			t.Fatalf("caller %d failed: %v", i, errs[i])
		}
		if got[i] != got[0] {
			t.Errorf("caller %d got a different engine; the handshake was not shared", i)
		}
	}
}
