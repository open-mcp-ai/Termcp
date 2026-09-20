package mcp

import (
	"encoding/json"
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
