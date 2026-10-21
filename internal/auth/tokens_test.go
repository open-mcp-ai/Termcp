package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenStore_CreateResolveRoundTrip(t *testing.T) {
	s := OpenTokenStore(t.TempDir())
	token, rec, err := s.Create("agent", []string{"shell.input", "shell.output"}, []string{"sess-1"}, true, time.Hour, "operator")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(token, tokenPrefix+rec.ID+".") {
		t.Fatalf("token %q does not carry its id %q", token, rec.ID)
	}
	p, err := s.Resolve(token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Kind != KindAgent || p.ID != rec.ID || !p.ApprovalRequired {
		t.Fatalf("principal: %+v", p)
	}
	if len(p.Tools) != 2 || len(p.Sessions) != 1 {
		t.Fatalf("scopes lost: %+v", p)
	}

	// The plaintext must not be on disk; the verifier must be.
	data, err := os.ReadFile(filepath.Join(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("plaintext token persisted")
	}
	if info, _ := os.Stat(s.path); info.Mode().Perm() != 0600 {
		t.Fatalf("tokens.toml mode %o, want 0600", info.Mode().Perm())
	}
}

func TestTokenStore_ResolveRejects(t *testing.T) {
	s := OpenTokenStore(t.TempDir())
	token, _, err := s.Create("agent", nil, nil, false, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "garbage", tokenPrefix + "nope", token + "tampered"} {
		if _, err := s.Resolve(bad); !errors.Is(err, ErrTokenUnknown) {
			t.Errorf("Resolve(%q): got %v, want ErrTokenUnknown", bad, err)
		}
	}
}

func TestTokenStore_ExpiryAndRevocation(t *testing.T) {
	s := OpenTokenStore(t.TempDir())

	expired, _, err := s.Create("agent", nil, nil, false, time.Nanosecond, "operator")
	if err != nil {
		t.Fatal(err)
	}
	// Poll rather than sleep a fixed span: the boundary is time.Now-based.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := s.Resolve(expired)
		if errors.Is(err, ErrTokenExpired) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("token never expired: last err %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	token, rec, err := s.Create("agent", nil, nil, false, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(rec.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.Resolve(token); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("after revoke: got %v, want ErrTokenRevoked", err)
	}
	// Idempotent and survives a reload (the store is per-process-opened).
	if err := s.Revoke(rec.ID); err != nil {
		t.Fatalf("second revoke must be a no-op: %v", err)
	}
	fresh := OpenTokenStore(filepath.Dir(s.path))
	if _, err := fresh.Resolve(token); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("revocation did not persist: %v", err)
	}
	if err := s.Revoke("no-such-id"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("revoke unknown: got %v, want ErrTokenNotFound", err)
	}
}

func TestTokenStore_ListRedactsVerifiers(t *testing.T) {
	s := OpenTokenStore(t.TempDir())
	if _, _, err := s.Create("agent", nil, nil, false, 0, "operator"); err != nil {
		t.Fatal(err)
	}
	recs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("List: %d records, want 1", len(recs))
	}
	if recs[0].Verifier != "" {
		t.Fatal("List must not carry verifiers")
	}
	if recs[0].CreatedBy != "operator" || recs[0].CreatedAt.IsZero() {
		t.Fatalf("metadata lost: %+v", recs[0])
	}
}

func TestTokenStore_EmptyStoreResolvesNothing(t *testing.T) {
	s := OpenTokenStore(t.TempDir())
	if _, err := s.Resolve("tcptok_aaaaaaaaaaa.bbbb"); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("empty store: got %v", err)
	}
	recs, err := s.List()
	if err != nil || len(recs) != 0 {
		t.Fatalf("empty List: %v %v", recs, err)
	}
}
