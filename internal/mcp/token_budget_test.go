package mcp

import (
	"encoding/json"
	"testing"
)

// TokenBudgetGuard prevents regression: the tools/list and instructions payloads
// must stay within defined limits. These values are injected into the model
// context every turn, so every byte matters.
func TestTokenBudgetGuard(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.RegisterSSHConfigWriteTools()

	tools := s.mcpServer.ListTools()
	if len(tools) != 31 {
		t.Fatalf("expected 31 tools, got %d", len(tools))
	}

	total := 0
	descBytes := 0
	propDescBytes := 0
	for _, st := range tools {
		b, err := json.Marshal(st.Tool)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
		descBytes += len(st.Tool.Description)
		props := st.Tool.InputSchema.Properties
		for _, pv := range props {
			pm, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			if d, ok := pm["description"].(string); ok {
				propDescBytes += len(d)
			}
		}
	}
	instructionsLen := len(mcpServerInstructions)

	t.Logf("tools/list total: %d B", total)
	t.Logf("tool descriptions: %d B", descBytes)
	t.Logf("property descriptions: %d B", propDescBytes)
	t.Logf("instructions: %d B", instructionsLen)

	// Budgets (bytes, rough 4:1 B:tokl ratio for English text)
	if total > 25000 {
		t.Errorf("tools/list total %d B exceeds 25000 B budget", total)
	}
	if descBytes > 4000 {
		t.Errorf("tool descriptions %d B exceeds 4000 B budget", descBytes)
	}
	if propDescBytes > 4000 {
		t.Errorf("property descriptions %d B exceeds 4000 B budget", propDescBytes)
	}
	if instructionsLen > 2400 {
		t.Errorf("instructions %d B exceeds 2400 B budget", instructionsLen)
	}
}

// TestDeferLoadingPolicy locks the split between always-loaded core tools and
// deferred (on-demand) tools. A model that must search before it can type a
// command pays a round trip on every interaction, so the core driving loop must
// never be deferred; the wide, low-frequency surfaces should be.
func TestDeferLoadingPolicy(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.RegisterSSHConfigWriteTools()

	tools := s.mcpServer.ListTools()
	byName := make(map[string]bool, len(tools))
	for _, st := range tools {
		byName[st.Tool.Name] = st.Tool.DeferLoading
	}

	core := []string{
		"session_start", "session_list", "session_info",
		"session_terminate", "session_delete",
		"shell_open", "shell_list", "shell_close", "shell_input", "shell_key", "shell_output",
		"notify_user",
	}
	for _, name := range core {
		deferred, ok := byName[name]
		if !ok {
			t.Errorf("core tool %q is not registered", name)
			continue
		}
		if deferred {
			t.Errorf("core tool %q must not be deferred (it is on the hot path)", name)
		}
	}

	for name := range deferredTools {
		got, ok := byName[name]
		if !ok {
			t.Errorf("deferredTools lists %q, which is not a registered tool (stale entry)", name)
			continue
		}
		if !got {
			t.Errorf("tool %q is listed in deferredTools but was not marked defer_loading", name)
		}
	}

	// Every registered tool must be classified, so a new tool cannot silently
	// default into the always-loaded set and inflate every client's context.
	for name := range byName {
		if deferredTools[name] {
			continue
		}
		found := false
		for _, c := range core {
			if c == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tool %q is neither in the core list nor deferredTools; classify it", name)
		}
	}
}
