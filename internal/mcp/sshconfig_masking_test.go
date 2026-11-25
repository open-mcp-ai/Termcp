package mcp

import (
	"context"
	"strings"
	"testing"

	sdkmcp "github.com/open-mcp-ai/termcp/mcp"
)

/* Masking reaches every surface that prints an address (§10.2). The WebUI list is
 * covered by its own handler test; these two lock the MCP side, because a switch
 * that hides an address in the browser and prints it in `ls host` would teach the
 * AI exactly the value the human asked not to spread.
 */

func maskTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	/* Synthetic values only: this is display-layer masking, so the profile needs
	   nothing more than an address and a credential that satisfies validation. */
	entry := `
kind = "remote"
host = "mask-host.example"
user = "maskuser"
port = 2222
password = "not-a-real-password"
`
	if err := s.sshConfigs.Save("masked-one", []byte(entry)); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	return s
}

func TestListingMasksTheTargetWhenTheStoreIsMasked(t *testing.T) {
	s := maskTestServer(t)

	unmasked := parseResult(t, mustCall(t, func(ctx context.Context) (*sdkmcp.CallToolResult, error) { return s.handleListSSHConfigs(ctx, makeRequest(nil)) }))
	where := findWhere(t, unmasked, "masked-one")
	if !strings.Contains(where, "mask-host.example") || !strings.Contains(where, "maskuser") {
		t.Fatalf("with masking off the listing should show the target, got %q", where)
	}

	if err := s.sshConfigs.SetMasked(true); err != nil {
		t.Fatalf("set masked: %v", err)
	}
	masked := parseResult(t, mustCall(t, func(ctx context.Context) (*sdkmcp.CallToolResult, error) { return s.handleListSSHConfigs(ctx, makeRequest(nil)) }))
	where = findWhere(t, masked, "masked-one")
	/* EVERY part of the address, the port included. Masking the host and printing
	   `:2222` narrows down which machine this is through the one field the mask
	   did not cover — a row that only looks masked is worse than none. */
	if strings.Contains(where, "mask-host.example") || strings.Contains(where, "maskuser") || strings.Contains(where, "2222") {
		t.Fatalf("masked listing leaked part of the target: %q", where)
	}
}

func TestDetailViewMasksTheWholeTargetIncludingPort(t *testing.T) {
	s := maskTestServer(t)
	if err := s.sshConfigs.SetMasked(true); err != nil {
		t.Fatalf("set masked: %v", err)
	}

	out := parseResult(t, mustCall(t, func(ctx context.Context) (*sdkmcp.CallToolResult, error) {
		return s.handleGetSSHConfig(ctx, makeRequest(map[string]any{"name": "masked-one"}))
	}))
	if out["host"] != maskFixedWidth {
		t.Fatalf("host = %v, want the fixed-width mask", out["host"])
	}
	if out["user"] != maskFixedWidth {
		t.Fatalf("user = %v, want the fixed-width mask", out["user"])
	}
	/* Zero, not 2222. A caller that needs to tell "masked" from "nothing written
	   down" reads the flag, because both cases legitimately report port 0. */
	if out["port"] != float64(0) {
		t.Fatalf("port = %v, want 0 while masked", out["port"])
	}
	if out["masked"] != true {
		t.Fatalf("masked = %v, want true so a port of 0 is not misread as absent", out["masked"])
	}
}

func mustCall(t *testing.T, fn func(context.Context) (*sdkmcp.CallToolResult, error)) *sdkmcp.CallToolResult {
	t.Helper()
	res, err := fn(context.Background())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return res
}

func findWhere(t *testing.T, payload map[string]any, name string) string {
	t.Helper()
	list, _ := payload["ssh_configs"].([]any)
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		if item["name"] == name {
			where, _ := item["where"].(string)
			return where
		}
	}
	t.Fatalf("profile %q missing from listing: %v", name, payload)
	return ""
}
