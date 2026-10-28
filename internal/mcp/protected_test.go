package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/capability"
)

// THE ACCESS DIRECTORY IS REFUSED, AND NO CONFIGURATION CAN LET IT THROUGH.
//
// The permission model caps what a token signed with the root key can do (§1.1 of
// docs/design/access-policy.md), so reading that key is not an escalation for a
// same-user process. It IS one for an agent reaching the daemon's filesystem
// through a session: the whitelist it would be checked against may be a whitelist
// it authored. So the refusal is fixed, and this test proves it fires even for an
// UNRESTRICTED session — the case where every other check in the guard returns
// nil by design.
func TestTheAccessDirectoryIsRefusedEvenForAnUnrestrictedSession(t *testing.T) {
	s := newTestServer(t)
	s.SetDataDir(t.TempDir())
	sid := internalSession(t, s)

	access := filepath.Join(s.dataDir, "access", "root.key")
	for _, tool := range []string{"file_read", "file_write", "file_stat", "file_delete"} {
		args := map[string]any{"session_id": sid, "remote_path": access}
		if tool == "file_write" {
			args["data"] = "x"
		}
		bad := (toolGuard{server: s}).check(context.Background(), tool, lookupGate(tool), makeRequest(args))
		if bad == nil {
			t.Fatalf("%s on the access directory was admitted", tool)
		}
		if bad.IsError == false {
			t.Fatalf("%s returned a non-error result", tool)
		}
	}
}

// Every spelling a caller reaches for is the same door. A guard that only matched
// the literal path would be walked around by `..`, by a trailing slash, or by the
// `~` form the resolver expands later.
func TestAccessDirectorySpellingsAreAllRefused(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.SetDataDir(dir)

	guard := toolGuard{server: s}
	c := gate{Capability: capability.Capability{ID: "file.read", PathArgs: []string{"remote_path"}}, known: true}

	cases := []string{
		filepath.Join(dir, "access", "root.key"),
		filepath.Join(dir, "access"),
		filepath.Join(dir, "access") + "/../access/root.key",
		filepath.Join(dir, "access") + "/./root.key",
	}
	for _, p := range cases {
		if bad := guard.refuseProtectedPath(c, map[string]any{"remote_path": p}); bad == nil {
			t.Errorf("%q was admitted", p)
		}
	}

	// The sibling that merely shares a prefix must NOT be refused: a component
	// boundary, not a string one. Otherwise `access-notes.txt` becomes unreachable.
	allowed := filepath.Join(dir, "access-notes.txt")
	if bad := guard.refuseProtectedPath(c, map[string]any{"remote_path": allowed}); bad != nil {
		t.Errorf("%q was refused but is outside the directory", allowed)
	}
}

// The ~ spelling has to be expanded before comparison, because the resolver does
// not run until after this check.
func TestTildeSpellingOfTheAccessDirectoryIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("~ expansion is a POSIX shell habit; the resolver on Windows takes native paths")
	}
	s := newTestServer(t)
	s.SetDataDir(t.TempDir())
	guard := toolGuard{server: s}
	c := gate{Capability: capability.Capability{ID: "file.read", PathArgs: []string{"remote_path"}}, known: true}

	// The guard's own directory is under a temp dir, so ~ does not reach it; what
	// is asserted here is that the expansion happens at all rather than the literal
	// three characters "~/.…" being compared to an absolute guard.
	if bad := guard.refuseProtectedPath(c, map[string]any{"remote_path": "~/does-not-matter"}); bad != nil {
		t.Fatalf("an unrelated home path must not be refused: %v", bad)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	guardDir := filepath.Join(home, ".termcp", "access")
	if !pathTouches("~/.termcp/access/root.key", guardDir) {
		t.Fatalf("the ~ spelling of %s must be recognised", guardDir)
	}
}

// A server that was never told its data directory protects nothing, and says so
// rather than refusing every path. This is the test-process case; main always
// sets it.
func TestNoDataDirectoryMeansNoProtection(t *testing.T) {
	s := newTestServer(t)
	guard := toolGuard{server: s}
	c := gate{Capability: capability.Capability{ID: "file.read", PathArgs: []string{"remote_path"}}, known: true}
	if bad := guard.refuseProtectedPath(c, map[string]any{"remote_path": "/anything"}); bad != nil {
		t.Fatalf("with no data directory the guard must stay out of the way: %v", bad)
	}
}

// The refusal has to explain itself. A bare "forbidden" sends an agent hunting for
// a whitelist setting that does not exist.
func TestTheRefusalSaysItCannotBeGranted(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.SetDataDir(dir)
	guard := toolGuard{server: s}
	c := gate{Capability: capability.Capability{ID: "file.read", PathArgs: []string{"remote_path"}}, known: true}

	bad := guard.refuseProtectedPath(c, map[string]any{"remote_path": filepath.Join(dir, "access", "root.key")})
	if bad == nil {
		t.Fatal("expected a refusal")
	}
	text := toolResultText(bad)
	if !strings.Contains(text, "cannot be granted") {
		t.Errorf("the refusal does not say the path is ungrantable, so a caller will look for a setting: %s", text)
	}
}
