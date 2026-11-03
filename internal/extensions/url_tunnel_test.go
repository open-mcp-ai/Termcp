package extensions

import (
	"strings"
	"testing"
)

// TestURLOnRemoteTargetAllowedWithLocalForward pins issue #233 (decision B-1):
//
// An HTTP MCP relay runs inside termcp (on this host). It used to reject every
// non-"internal" target outright, which made "remote MCP reached through an SSH
// tunnel" impossible to write in a single template.
//
// It is now accepted when the template declares a local forward (which opens a
// listener ON THIS HOST reaching the target), and still rejected when it does not
// or when it only declares a dynamic (SOCKS5) forward.
func TestURLOnRemoteTargetAllowedWithLocalForward(t *testing.T) {
	// 1. Remote target with local forward -> VALID
	valid := Extension{
		Name:   "remote-mcp",
		Target: "prod-box",
		Mode:   "pipe",
		URL:    "http://127.0.0.1:18080/mcp",
		Forwards: []ForwardDecl{
			{Kind: ForwardLocal, LocalPort: 18080, RemoteHost: "127.0.0.1", RemotePort: 8080},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("remote target with local forward should validate: %v", err)
	}

	// 2. Remote target with NO forward -> REFUSED
	noForward := Extension{
		Name:   "no-forward",
		Target: "prod-box",
		Mode:   "pipe",
		URL:    "http://127.0.0.1:18080/mcp",
	}
	err := noForward.Validate()
	if err == nil {
		t.Fatal("remote target with no forward should be refused")
	}
	if !strings.Contains(err.Error(), "declare a local forward") {
		t.Errorf("refusal should teach declaring a local forward, got: %v", err)
	}

	// 3. Remote target with ONLY dynamic (-D) forward -> REFUSED
	// SOCKS5 does not listen for HTTP; the bridge would fail at dial time without an explanation.
	dynamicOnly := Extension{
		Name:   "dynamic-only",
		Target: "prod-box",
		Mode:   "pipe",
		URL:    "http://127.0.0.1:18080/mcp",
		Forwards: []ForwardDecl{
			{Kind: ForwardDynamic, LocalPort: 18080},
		},
	}
	err = dynamicOnly.Validate()
	if err == nil {
		t.Fatal("remote target with only dynamic forward should be refused")
	}
	if !strings.Contains(err.Error(), "declare a local forward") {
		t.Errorf("refusal should explain that a local forward is needed, got: %v", err)
	}
}
