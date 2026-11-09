package session

import (
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestSessionNoShell pins the shell-less container: an extension that exists to
// carry SFTP and forwards starts no primary channel, and the first shell opened
// on it later becomes channel 1 — the number a locator refers to.
func TestSessionNoShell(t *testing.T) {
	srv := startTestServer(t)

	cfg := testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "shell-less")
	cfg.NoShell = true
	s, err := New(srv, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if s.PrimaryShellID() != "" {
		t.Errorf("PrimaryShellID = %q, want empty for a shell-less container", s.PrimaryShellID())
	}
	if s.PrimaryShell() != nil {
		t.Error("PrimaryShell() should be nil when no primary shell was created")
	}
	if n := s.nextShellIndex.Load(); n != 0 {
		t.Errorf("nextShellIndex = %d, want 0 so the first child shell becomes channel 1", n)
	}
	if count := liveShellCount(s); count != 0 {
		t.Errorf("live shells = %d, want 0", count)
	}
	if s.Info().Status != api.SessionRunning {
		t.Errorf("status = %q, want running", s.Info().Status)
	}

	// Opening a shell on the shell-less container still works and takes index 1,
	// so the extension's channel numbering matches what a locator refers to.
	cs, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
	if err != nil {
		// The loopback test server multiplexes over a real SSH connection, so this
		// should succeed; a failure here means the shell-less path dropped
		// something the child channel needs.
		t.Fatalf("CreateChildShell on a shell-less container: %v", err)
	}
	defer s.CloseChildShell(cs.ID)
	if cs.Index != 1 {
		t.Errorf("first child shell Index = %d, want 1", cs.Index)
	}
	if s.PrimaryShellID() != "" {
		t.Error("opening a child must not retroactively mint a primary shell id")
	}
}

// TestSessionNoShellKeepsRestriction pins that a shell-less extension container
// still carries the whitelist its template defined.
func TestSessionNoShellKeepsRestriction(t *testing.T) {
	srv := startTestServer(t)

	cfg := testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "restricted")
	cfg.NoShell = true
	r, err := NewRestriction([]string{"file_read", "file_stat"}, []string{"~/skills/foo/"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Restriction = r
	s, err := New(srv, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	got := s.Restriction()
	if !got.AllowsTool("file_read") {
		t.Error("the session should carry its whitelist")
	}
	if got.AllowsTool("file_write") {
		t.Error("file_write should be outside the whitelist")
	}
	if !got.AllowsPath("~/skills/foo/SKILL.md") {
		t.Error("a path inside the tree should be allowed")
	}
	if got.AllowsPath("/etc/passwd") {
		t.Error("a path outside the tree should be denied")
	}
}

// TestSessionDefaultIsUnrestricted pins the backward-compatibility contract:
// every existing caller builds a Config without a Restriction and must keep the
// full tool surface.
func TestSessionDefaultIsUnrestricted(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "plain"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	r := s.Restriction()
	if !r.AllowsTool("session_delete") || !r.AllowsPath("/etc/shadow") {
		t.Error("a session created without a Restriction must be unrestricted")
	}
	if s.PrimaryShellID() == "" {
		t.Error("a normal session must still create its primary shell")
	}
}

func liveShellCount(s *Session) int {
	n := 0
	s.shells.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
