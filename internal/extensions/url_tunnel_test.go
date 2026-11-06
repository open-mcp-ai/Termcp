package extensions

import (
	"testing"
)

// TestURLOnAnyTargetAllowed pins the egress-machine semantics of issue #233:
//
// A url template's `target` names the machine the connection LEAVES from, not
// where the process runs. The in-process relay dials its requests inside that
// host's SSH connection, so:
//   - target = "internal" dials from this host (loopback = this machine)
//   - target = "prod-box" dials from prod-box (loopback = prod-box's loopback)
//
// No manual port forward is required, no local port is bound, and two extensions
// reaching the same remote service cannot collide.
func TestURLOnAnyTargetAllowed(t *testing.T) {
	// 1. Remote target with NO forward -> now perfectly valid (target is the egress host)
	directRemote := Extension{
		Name:   "direct-remote-mcp",
		Target: "prod-box",
		Mode:   "pipe",
		URL:    "http://127.0.0.1:8080/mcp",
	}
	if err := directRemote.Validate(); err != nil {
		t.Fatalf("remote target without forward should validate, got: %v", err)
	}

	// 2. Internal target -> valid (egress is loopback)
	internal := Extension{
		Name:   "local-mcp",
		Target: "internal",
		Mode:   "pipe",
		URL:    "http://127.0.0.1:8080/mcp",
	}
	if err := internal.Validate(); err != nil {
		t.Fatalf("internal target should validate, got: %v", err)
	}

	// 3. Multi-target with remote hosts -> also valid
	multi := Extension{
		Name: "multi-remote-mcp",
		Targets: []TargetDecl{
			{Host: "cluster-bj"},
			{Host: "cluster-hk"},
		},
		Mode: "pipe",
		URL:  "http://127.0.0.1:8080/mcp",
	}
	if err := multi.Validate(); err != nil {
		t.Fatalf("multi-target remote url should validate, got: %v", err)
	}
}
