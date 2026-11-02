package sshconfig

import (
	"errors"
	"testing"
)

func TestAuthorizationBindingTracksIdentityAndContent(t *testing.T) {
	s := NewStore(t.TempDir())
	first := []byte("kind = \"remote\"\nhost = \"first.example\"\nuser = \"synthetic\"\npassword = \"synthetic\"\n")
	if err := s.Save("bound", first); err != nil {
		t.Fatal(err)
	}
	binding, err := s.Binding("bound")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Identity == "" || binding.Version == "" {
		t.Fatalf("incomplete binding: %+v", binding)
	}
	second := []byte("kind = \"remote\"\nhost = \"second.example\"\nuser = \"synthetic\"\npassword = \"synthetic\"\n")
	if err := s.Save("bound", second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadBound("bound", binding); !errors.Is(err, ErrAuthorizationBindingChanged) {
		t.Fatalf("changed content loaded: %v", err)
	}
}

func TestAuthorizationBindingSurvivesRenameButNotNameReuse(t *testing.T) {
	s := NewStore(t.TempDir())
	body := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"synthetic\"\npassword = \"synthetic\"\n")
	if err := s.Save("old-name", body); err != nil {
		t.Fatal(err)
	}
	binding, err := s.Binding("old-name")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("old-name", "new-name"); err != nil {
		t.Fatal(err)
	}
	canonical, _, err := s.LoadBound("old-name", binding)
	if err != nil || canonical != "new-name" {
		t.Fatalf("rename lost stable identity: %q %v", canonical, err)
	}
	if err := s.Delete("new-name"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("old-name", body); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadBound("old-name", binding); err == nil {
		t.Fatal("same-name replacement inherited the prior binding")
	}
}
