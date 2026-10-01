package mcp

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestServerInfoVersion verifies the build version passed to New is reported in
// the initialize response (serverInfo.version). main.go derives that value from
// the same metadata `termcp --version` prints, so MCP clients see the release
// tag instead of a hard-coded placeholder.
func TestServerInfoVersion(t *testing.T) {
	s := New(nil, nil, nil, nil, "1.2.3-test")
	res := callMCP(t, s, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	var init struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatal(err)
	}
	if init.ServerInfo.Name != "termcp" {
		t.Errorf("serverInfo.name = %q, want termcp", init.ServerInfo.Name)
	}
	if init.ServerInfo.Version != "1.2.3-test" {
		t.Errorf("serverInfo.version = %q, want 1.2.3-test", init.ServerInfo.Version)
	}

	// An empty version must fall back to a non-empty default, never "".
	s2 := New(nil, nil, nil, nil, "")
	res2 := callMCP(t, s2, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	if err := json.Unmarshal(res2, &init); err != nil {
		t.Fatal(err)
	}
	if init.ServerInfo.Version == "" {
		t.Fatal("serverInfo.version must not be empty when New is called with \"\"")
	}
}

// TestInstructionsDoNotMentionRemovedTools guards the initialize instructions
// against resurrecting tool names that no longer exist (the removed `history`
// tool regressed this once already).
func TestInstructionsDoNotMentionRemovedTools(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	res := callMCP(t, s, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	var init struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatal(err)
	}
	if init.Instructions == "" {
		t.Fatal("initialize must carry server instructions")
	}
	for _, removed := range []string{"history(", "local_forward", "message_list", "file_chmod"} {
		if strings.Contains(init.Instructions, removed) {
			t.Errorf("instructions mention removed tool %q", removed)
		}
	}
}

// TestServerJSONMatchesChangelog keeps the MCP Registry manifest in step with
// the release the changelog announces. Every other trace of the version is
// derived (ldflags, the module version, /api/version), but server.json is a
// checked-in file that only the publish workflow rewrites - from the tag, after
// the checkout, never back into the repo. So a stale one is caught by nobody:
// it sat at 0.2.0 through four releases. This is the cheap local check that
// closes that gap; it lives here because `go test ./internal/...` is what CI
// actually runs.
//
// The heading compared against is the newest release in the changelog, which is
// the version server.json must name. It is not necessarily the build under
// development: bumping the heading is part of cutting a release here, so the
// first `## vX.Y.Z` is the release being prepared or just shipped, and a commit
// after it (an unreleased fix) does not change what the manifest must say.
func TestServerJSONMatchesChangelog(t *testing.T) {
	changelog, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		// A test run outside the repository root (or a source tarball without the
		// docs) must not fail the suite: the manifest check is repo hygiene, not a
		// property of the package under test. TestSyncedDocsMatchSource skips for
		// the same reason.
		t.Skipf("read CHANGELOG.md: %v", err)
	}
	// The newest `## vX.Y.Z` heading is the release server.json must name.
	heading := regexp.MustCompile(`^## v(\d+\.\d+\.\d+)\b`)
	var version string
	for _, line := range strings.Split(string(changelog), "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			version = m[1]
			break
		}
	}
	if version == "" {
		t.Fatal("CHANGELOG.md has no '## vX.Y.Z' release heading")
	}

	raw, err := os.ReadFile("../../server.json")
	if err != nil {
		t.Skipf("read server.json: %v", err)
	}
	var manifest struct {
		Version  string `json:"version"`
		Packages []struct {
			Identifier string `json:"identifier"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse server.json: %v", err)
	}
	if manifest.Version != version {
		t.Errorf("server.json version = %q, CHANGELOG announces %q (bump both when releasing)", manifest.Version, version)
	}
	// The OCI identifier must carry the same version: the publish workflow
	// rewrites it together with `version`, so one drifting alone means a
	// manifest pointing at an image tag that was never built.
	if len(manifest.Packages) == 0 {
		t.Fatal("server.json declares no packages")
	}
	want := ":" + version
	if got := manifest.Packages[0].Identifier; !strings.HasSuffix(got, want) {
		t.Errorf("server.json packages[0].identifier = %q, want it to end in %q", got, want)
	}
}
