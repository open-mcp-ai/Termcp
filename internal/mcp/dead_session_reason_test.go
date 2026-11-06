package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// aWindow is a window big enough to hand back a generated reply: the zero value is the
// schema's "consume nothing" window, which answers 204 with an empty body on purpose.
func aWindow() termArgs { return termArgs{BytesLimit: 4096, LinesLimit: 200} }

// aServerOverADirectory builds the same server newTestServer does, but hands back the
// store so a test can write a manifest this process never held in its registry.
//
// That state is what issue #176 reported and what no ordinary test setup produces: the
// record is on disk, the registry is not holding it, and the caller asks about it.
func aServerOverADirectory(t *testing.T) (*Server, *storage.Store) {
	t.Helper()
	srv := startTestSSH(t)
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	cleanupTestRuntime(t, sessMgr, srv)
	return New(sessMgr, msgMgr, sshconfig.NewStore(dir), nil, "test"), store
}

// A 404 THAT SAYS WHICH 404 IT IS.
//
// "Session 'x' not found" is the same sentence for a typo, for an id this process never
// loaded, and for an id that lives in another instance's data directory. The three call
// for different next moves, and the reply could not tell them apart (issue #176). When
// the manifest IS on disk the message now says so, which rules out the typo.
func TestANotFoundSessionSaysWhenItsRecordIsOnDisk(t *testing.T) {
	s, store := aServerOverADirectory(t)

	const id = "7c1f0a2b-4d3e-4f5a-8b9c-0d1e2f3a4b5c"
	if err := store.SaveSession(api.Session{
		ID: id, Name: "on-disk", Status: api.SessionExited, Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	slot := s.directiveInfo(context.Background(), directive.Statement{Op: "get", Target: id}, aWindow())
	if slot.Code != 404 {
		t.Fatalf("code = %d, want 404; body: %s", slot.Code, slot.Body)
	}
	if !strings.Contains(slot.Body, "not found") {
		t.Fatalf("the reply does not say it is missing: %q", slot.Body)
	}
	// THE FACT THAT MATTERS: the id is not a typo.
	if !strings.Contains(slot.Body, "record is on disk") {
		t.Fatalf("the 404 does not mention the manifest it could see, so a typo and an "+
			"id belonging to another instance read the same: %q", slot.Body)
	}

	// And an id with no record anywhere keeps the plain sentence: the diagnosis is only
	// worth printing when it is true.
	plain := s.directiveInfo(context.Background(),
		directive.Statement{Op: "get", Target: "0badf00d-0000-0000-0000-000000000000"}, aWindow())
	if plain.Code != 404 {
		t.Fatalf("code = %d, want 404; body: %s", plain.Code, plain.Body)
	}
	if strings.Contains(plain.Body, "record is on disk") {
		t.Fatalf("the 404 claims a manifest for an id that has none: %q", plain.Body)
	}
}

// `get` SAYS WHY THE SESSION IS NOT RUNNING.
//
// The field exists on the session, is persisted, and is restored — but a fact no surface
// prints is a fact the caller does not have. This pins the rendering, not the storage.
func TestGetPrintsWhyTheSessionEnded(t *testing.T) {
	s, store := aServerOverADirectory(t)

	const id = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	if err := store.SaveSession(api.Session{
		ID: id, Name: "died", Status: api.SessionExited, Command: "/bin/sh",
		ExitReason: "❌ SSH connection lost: read tcp: connection reset by peer",
	}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	// The registry does not hold it, so go through the restore the daemon performs at
	// boot — the same road that puts a dead session back in front of a caller.
	if err := s.sessMgr.RestoreDead(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	slot := s.directiveInfo(context.Background(), directive.Statement{Op: "get", Target: id}, aWindow())
	if slot.Code != 200 {
		t.Fatalf("code = %d, want 200; body: %s", slot.Code, slot.Body)
	}
	if !strings.Contains(slot.Body, "status: exited") {
		t.Fatalf("the reply does not report the state: %q", slot.Body)
	}
	if !strings.Contains(slot.Body, "exit_reason: ❌ SSH connection lost: read tcp") {
		t.Fatalf("the reply does not say why the session ended, which is the one fact a "+
			"caller asking about a dead session needs: %q", slot.Body)
	}
}
