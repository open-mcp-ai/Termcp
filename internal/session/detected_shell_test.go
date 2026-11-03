package session

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
)

// TestInternalSessionRecordsDetectedShell pins issue #208.1:
//
// A session established against the internal loopback environment detects the local
// shell (e.g. pwsh/powershell/bash) and records it on the session metadata, so callers
// can inspect the shell family without having to blindly guess or run uname/echo $0.
func TestInternalSessionRecordsDetectedShell(t *testing.T) {
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	mgr := NewManager(nil, nil, srv)
	s, err := mgr.Create(Config{Mode: "pty", Name: "probe"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Delete(s.ID) })

	info := s.Info()
	if info.DetectedShell == "" {
		t.Fatalf("expected DetectedShell to be populated on internal session, got empty")
	}
	if !strings.Contains(info.DetectedShell, "(") || !strings.Contains(info.DetectedShell, ")") {
		t.Errorf("DetectedShell should have format 'family (binary)', got: %q", info.DetectedShell)
	}
}
