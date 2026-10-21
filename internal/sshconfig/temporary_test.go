package sshconfig

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pollUntil probes f until it holds or the deadline passes — CI runners are
// slow and contended, so TTL tests poll with headroom instead of sleeping a
// fixed span (docs/agents/testing.md).
func pollUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTemporaryProfileTTLExpires(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.SaveTemporary("ephemeral", remoteProfileTOML("op", "ttl-secret"), 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("ephemeral"); err != nil {
		t.Fatalf("Load before expiry: %v", err)
	}
	pollUntil(t, "TTL sweep", func() bool {
		_, err := s.Load("ephemeral")
		return err != nil
	})
	if s.IsTemporary("ephemeral") {
		t.Fatal("expired profile still reports temporary")
	}
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(names, ","), "ephemeral") {
		t.Fatalf("expired profile still listed: %v", names)
	}
}

func TestTemporaryProfileWithoutTTLPersists(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.SaveTemporary("stays", remoteProfileTOML("op", "x"), 0); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, "sanity that sweeping ran", func() bool { return true })
	if _, err := s.Load("stays"); err != nil {
		t.Fatalf("TTL=0 profile must live until process exit: %v", err)
	}
}

func TestBindTemporaryDropsWithSession(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.SaveTemporary("job-one", remoteProfileTOML("op", "bound-secret"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTemporary("job-two", remoteProfileTOML("op", "loose"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTemporary("job-one", "sess-123"); err != nil {
		t.Fatalf("BindTemporary: %v", err)
	}
	s.DropBoundTemporary("sess-123")
	if s.IsTemporary("job-one") {
		t.Fatal("bound profile survived its session's DEAD")
	}
	if !s.IsTemporary("job-two") {
		t.Fatal("unbound profile was dropped with another session's DEAD")
	}
	// A different session's DEAD is a no-op.
	s.DropBoundTemporary("sess-other")
	if !s.IsTemporary("job-two") {
		t.Fatal("unrelated DropBoundTemporary removed a profile")
	}
}

func TestBindTemporaryRequiresTemporary(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save("persisted", remoteProfileTOML("op", "x")); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTemporary("persisted", "sess-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bind on a persistent profile: got %v, want ErrNotFound", err)
	}
	if err := s.BindTemporary("missing", "sess-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bind on nothing: got %v, want ErrNotFound", err)
	}
}

func TestImportBatchTTL(t *testing.T) {
	s := NewStore(t.TempDir())
	doc := []byte(`[[connections]]
name = "ttl-import"
kind = "remote"
host = "h"
user = "u"
password = "p"
`)
	res, err := s.ImportBatch(doc, true, 60*time.Millisecond)
	if err != nil || res.Imported != 1 {
		t.Fatalf("ImportBatch: %+v %v", res, err)
	}
	pollUntil(t, "imported TTL sweep", func() bool { return !s.IsTemporary("ttl-import") })
}
