package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// The ssh_config(action=edit) handler overlays the arguments a caller sent onto
// a profile already on disk. That merge has two rules that a caller relies on
// and that neither tools/list nor the TOML round trip can state: an omitted
// field must stay untouched, and the two booleans must be decided by presence
// rather than by truth. These tests pin the rules, because the merge is the
// whole content of the handler — the marshalling and saving around it are
// plumbing.

// editProfile writes a starting profile and returns the server plus its store.
func editProfile(t *testing.T, body string) (*Server, *sshconfig.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	s := New(nil, nil, store, nil, "test")
	if err := store.Save("box", []byte(body)); err != nil {
		t.Fatalf("seeding the profile: %v", err)
	}
	return s, store, "box"
}

// edit calls the handler and fails the test on a tool-level error.
func edit(t *testing.T, s *Server, args map[string]any) {
	t.Helper()
	res, err := s.handleEditSSHConfig(context.Background(), makeRequest(args))
	if err != nil {
		t.Fatalf("handleEditSSHConfig returned a transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("edit was rejected: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
}

// TestEditSSHConfigLeavesOmittedFieldsAlone is the rule that makes the edit form
// usable: it sends only what the operator changed, so every other field must
// survive. A merge that assigned unconditionally would blank the password here.
func TestEditSSHConfigLeavesOmittedFieldsAlone(t *testing.T) {
	const seeded = `kind = "remote"
host = "old.example"
user = "alice"
password = "secret"
port = 2222
description = "production"
`
	s, store, name := editProfile(t, seeded)

	// Change one field, mention nothing else.
	edit(t, s, map[string]any{"name": name, "host": "new.example"})

	got, err := store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if got.Host != "new.example" {
		t.Errorf("host = %q, want new.example", got.Host)
	}
	if got.User != "alice" {
		t.Errorf("user = %q: an omitted field was cleared", got.User)
	}
	if got.Password != "secret" {
		t.Errorf("password was cleared by an edit that never mentioned it")
	}
	if got.Port != 2222 {
		t.Errorf("port = %d: an omitted field was cleared", got.Port)
	}
	if got.Description != "production" {
		t.Errorf("description = %q: an omitted field was cleared", got.Description)
	}
}

// TestEditSSHConfigEmptyStringDoesNotClear pins the flip side: the edit form
// cannot clear a field by sending "". That is a real limitation, and it is
// deliberate — it is what lets the form send every input's value without
// wiping the profile — so it belongs in a test rather than in a shrug.
func TestEditSSHConfigEmptyStringDoesNotClear(t *testing.T) {
	s, store, name := editProfile(t, "kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n")

	edit(t, s, map[string]any{"name": name, "user": ""})

	got, err := store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if got.User != "u" {
		t.Errorf("user = %q, want u: an empty string must not clear a field", got.User)
	}
}

// TestEditSSHConfigDefaultApprovalFollowsPresence covers the one field where
// presence and truth disagree. `default_approval: false` is how a profile is
// turned back off, so it must be acted on; the *absence* of the key is what has
// to leave the setting alone. Reading truth instead of presence would make
// turning it off impossible, and reading presence for every field would make
// the form clear everything it did not send.
func TestEditSSHConfigDefaultApprovalFollowsPresence(t *testing.T) {
	const seeded = "kind = \"internal\"\ndefault_approval = true\n"
	s, store, name := editProfile(t, seeded)

	// An unrelated edit must not turn review mode off.
	edit(t, s, map[string]any{"name": name, "default_shell": "bash"})
	got, err := store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if !got.DefaultApproval {
		t.Error("an edit that never mentioned default_approval turned review mode off")
	}

	// Sending it as false must turn it off even though false is the zero value.
	edit(t, s, map[string]any{"name": name, "default_approval": false})
	got, err = store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if got.DefaultApproval {
		t.Error("default_approval = false was ignored; the profile cannot be turned back off")
	}
}

// TestEditSSHConfigJumpNeedsHost pins the all-or-nothing rule for a bastion. The
// jump_* arguments are only meaningful next to jump_host, and acting on them
// alone would invent a bastion out of a half-filled form — which would then
// fail validation, or worse, dial somewhere unintended.
func TestEditSSHConfigJumpNeedsHost(t *testing.T) {
	s, store, name := editProfile(t, "kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n")

	// jump_user without jump_host: nothing to attach it to, so nothing happens.
	edit(t, s, map[string]any{"name": name, "jump_user": "bastion"})
	got, err := store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if got.Jump != nil {
		t.Fatalf("jump_user alone created a bastion: %+v", got.Jump)
	}

	// jump_host creates it and carries the rest of the arguments with it. The
	// bastion needs credentials of its own to pass validation, which is the point
	// of keeping the jump fields together under one host.
	edit(t, s, map[string]any{
		"name":             name,
		"jump_host":        "bastion.example",
		"jump_user":        "ops",
		"jump_password":    "bastion-secret",
		"jump_port":        float64(2200),
		"jump_known_hosts": "~/.ssh/known_hosts",
	})
	got, err = store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if got.Jump == nil {
		t.Fatal("jump_host did not create a bastion")
	}
	if got.Jump.Host != "bastion.example" || got.Jump.User != "ops" {
		t.Errorf("bastion host/user = %q/%q, want bastion.example/ops", got.Jump.Host, got.Jump.User)
	}
	if got.Jump.Port != 2200 {
		t.Errorf("bastion port = %d, want 2200", got.Jump.Port)
	}
	if got.Jump.Password != "bastion-secret" {
		t.Error("the bastion's credentials did not travel with jump_host")
	}
}

// TestEditSSHConfigTrimsWhitespace pins the trimming that the form relies on: a
// value pasted with a trailing newline must not become part of the profile, and
// in particular must not reach the TOML as a surprising name.
func TestEditSSHConfigTrimsWhitespace(t *testing.T) {
	s, store, name := editProfile(t, "kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n")

	edit(t, s, map[string]any{"name": name, "host": "  spaced.example\n", "user": "\talice\n"})

	got, err := store.Load(name)
	if err != nil {
		t.Fatalf("loading the edited profile: %v", err)
	}
	if got.Host != "spaced.example" {
		t.Errorf("host = %q, want the trimmed spaced.example", got.Host)
	}
	if got.User != "alice" {
		t.Errorf("user = %q, want the trimmed alice", got.User)
	}
}

// TestEditSSHConfigRejectsAMissingName covers the guard in front of the merge:
// an edit with no name has no profile to edit, and must be refused rather than
// creating one from whatever else the caller sent.
func TestEditSSHConfigRejectsAMissingName(t *testing.T) {
	s, _, _ := editProfile(t, "kind = \"internal\"\n")

	res, err := s.handleEditSSHConfig(context.Background(), makeRequest(map[string]any{"host": "h"}))
	if err != nil {
		t.Fatalf("handleEditSSHConfig returned a transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("an edit with no name was accepted")
	}
}
