package mcp

import (
	"strings"
	"testing"
)

func profilesTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	return s
}

// Deleting a profile that does not exist must say so.
//
// Store.Delete is idempotent ON PURPOSE — an internal cleanup that runs twice should
// not fail — but the surface inherited that: `profile --action delete --name typo`
// answered 200 success, so a mistyped name looked like a successful removal. Every
// other delete in this system answers 404 (`ext delete`, `rm <session>`,
// `fwd --action close`), which is what makes this one wrong rather than a style.
func TestProfileDeleteOnAMissingNameIsRefused(t *testing.T) {
	s := profilesTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "profile --action delete --name never-existed"})
	if strings.Contains(text, "success") {
		t.Fatalf("deleting a missing profile answered success:\n%s", text)
	}
	if !strings.Contains(strings.ToLower(text), "not found") {
		t.Errorf("the refusal does not say the profile is missing:\n%s", text)
	}
}

// And deleting one that DOES exist still works, so the check did not simply block
// the operation.
func TestProfileDeleteOnAnExistingNameSucceeds(t *testing.T) {
	s := profilesTestServer(t)
	if text := termText(t, s, map[string]any{
		"cmd": "profile --action create --name victim --host 192.0.2.1 --user u --password p",
	}); !strings.Contains(text, "success") {
		t.Fatalf("create failed:\n%s", text)
	}
	if text := termText(t, s, map[string]any{
		"cmd": "profile --action delete --name victim",
	}); !strings.Contains(text, "success") {
		t.Fatalf("deleting an existing profile failed:\n%s", text)
	}
	list := termText(t, s, map[string]any{"cmd": "profile --action list"})
	if strings.Contains(list, "victim") {
		t.Fatalf("the profile is still listed after delete:\n%s", list)
	}
}
