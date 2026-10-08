package session

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestLostTransportArchivesSessionWithNoLiveShell locks the rule that the
// archive reflects the CONNECTION, not the shells: a session stays running while
// its SSH transport is up (even with every shell exited), and must reach DEAD as
// soon as that transport is gone.
//
// The per-shell exit watcher cannot report this case: it belongs to a shell, and
// here every shell has already ended. Without a watcher on the transport itself
// the session kept claiming "running" forever -- listed as live, refusing to move
// to the archive, while every operation on it failed with "new session: EOF".
//
// The shape matters: killing the server with a LIVE shell would pass on the old
// code too (that path is the per-shell watcher's), so this test exits the shell
// first and only then takes the transport away.
func TestLostTransportArchivesSessionWithNoLiveShell(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "lost-transport"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	// The user ends the only shell. The container must survive that: the SSH
	// connection still carries forwards, SFTP and new channels.
	sh := s.GetChildShell(s.PrimaryShellID())
	if sh == nil {
		t.Fatal("primary shell missing")
	}
	_ = sh.SendTerminalBytes([]byte(testShellInput("exit")), false)
	if !waitFor(t, 10*time.Second, func() bool {
		cur := s.GetChildShell(s.PrimaryShellID())
		return cur != nil && cur.Status != api.SessionRunning
	}, "the shell to exit") {
		t.Fatal("shell never exited after `exit`")
	}
	if got := s.Info().Status; got != api.SessionRunning {
		t.Fatalf("a shell ending must leave the container running, got %q", got)
	}

	// Now the transport dies while nothing is left to notice it.
	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}

	if !waitFor(t, 20*time.Second, func() bool {
		return s.Info().Status != api.SessionRunning
	}, "the session to reach DEAD once its transport is gone") {
		t.Fatalf("session still %q after its SSH transport died with no live shell: "+
			"it will never move to the archive", s.Info().Status)
	}
}
