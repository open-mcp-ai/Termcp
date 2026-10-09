package mcp

import (
	"context"
	"strings"
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

// Issue #81: the agent is the one asked to turn a pasted list of hosts into an
// importable file, and the file format is not the argument shape of this tool --
// a bastion is a nested table here while jump_host is an argument. Nothing in the
// tool surface said so, and a file written with the argument spelling parses
// cleanly and silently drops the bastion, so the mistake is invisible until the
// connection is dialled.
//
// The test drives the format the description advertises through the real store
// instead of matching the prose: a description that drifted away from what the
// parser accepts would still satisfy a string assertion. Every profile written
// here is the one the hint tells the model to produce, and the jump case is
// asserted by presence because a dropped bastion is the specific silent failure
// the hint exists to prevent.
func TestSSHConfigDescriptionAdvertisesTheFormatTheStoreAccepts(t *testing.T) {
	// What the model is told, in both listings: the read-only one and the
	// write-enabled one RegisterSSHConfigWriteTools installs, which REPLACES the
	// description rather than extending it. Only the second is on the wire when
	// -mcp-manage-ssh-configs is set, which is exactly the deployment where an
	// agent is allowed to write profiles at all.
	for _, tc := range []struct {
		name string
		w    bool
	}{
		{"read-only listing", false},
		{"write-enabled listing", true},
	} {
		s := newTestServer(t)
		if tc.w {
			s.RegisterSSHConfigWriteTools()
		}
		desc := ""
		for _, tool := range listTools(t, s, context.Background()) {
			if tool.Name == "ssh_config" {
				desc = tool.Description
			}
		}
		if desc == "" {
			t.Fatalf("%s: ssh_config missing from tools/list", tc.name)
		}
		for _, want := range []string{
			"[[connections]]",
			`kind="remote"`,
			"password or private_key",
			// The trap: the nested spelling the parser reads, and the flat one it
			// ignores.
			"[connections.jump]",
			"jump_host",
			// The two import rules an agent-generated file depends on.
			"internal\" is reserved",
			"name-2",
		} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s: ssh_config description misses %q:\n%s", tc.name, want, desc)
			}
		}
	}

	// The advertised shape must be the shape the store accepts, including the
	// nested bastion, and it must survive an export/import round trip.
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	doc := []byte(`
[[connections]]
name = "host-one"
kind = "remote"
host = "host-one.example"
user = "tester"
password = "placeholder"

[connections.jump]
host = "bastion.example"
user = "jumper"
password = "placeholder2"
`)
	if _, err := store.ImportBatch(doc, false); err != nil {
		t.Fatalf("the format the description advertises was rejected: %v", err)
	}
	entry, err := store.Load("host-one")
	if err != nil {
		t.Fatalf("loading the imported profile: %v", err)
	}
	if entry.Jump == nil || entry.Jump.Host != "bastion.example" {
		t.Fatalf("the nested bastion was dropped: %+v", entry.Jump)
	}

	// Export must emit something that imports back, or "export emits this shape"
	// in the description is false.
	exported, err := store.ExportBatch(nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	second := sshconfig.NewStore(t.TempDir())
	if _, err := second.ImportBatch(exported, false); err != nil {
		t.Fatalf("an exported file did not import back: %v\n%s", err, exported)
	}
	again, err := second.Load("host-one")
	if err != nil {
		t.Fatalf("loading the re-imported profile: %v", err)
	}
	if again.Jump == nil || again.Jump.Host != "bastion.example" {
		t.Errorf("the round trip lost the bastion: %+v", again.Jump)
	}
}
