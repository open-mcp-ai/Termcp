package mcpbridge

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
)

// TestViaNamesTheEgressMachine pins issue #233's `--via` contract.
func TestViaNamesTheEgressMachine(t *testing.T) {
	// 1. Both spellings parse, and the URL still lands where it did.
	for _, argv := range [][]string{
		{"http://127.0.0.1:8080/mcp", "--via", "prod-box"},
		{"http://127.0.0.1:8080/mcp", "--via=prod-box"},
	} {
		cfg, err := ParseArgs(argv)
		if err != nil {
			t.Fatalf("ParseArgs(%v): %v", argv, err)
		}
		if cfg.Via != "prod-box" {
			t.Errorf("ParseArgs(%v).Via = %q, want prod-box", argv, cfg.Via)
		}
		if cfg.URL != "http://127.0.0.1:8080/mcp" {
			t.Errorf("ParseArgs(%v).URL = %q", argv, cfg.URL)
		}
	}

	// 2. A missing value is refused rather than treated as "no --via".
	for _, argv := range [][]string{
		{"http://127.0.0.1:8080/mcp", "--via"},
		{"http://127.0.0.1:8080/mcp", "--via="},
		{"http://127.0.0.1:8080/mcp", "--via", "   "},
	} {
		if _, err := ParseArgs(argv); err == nil {
			t.Errorf("ParseArgs(%v) should refuse an empty profile", argv)
		}
	}

	// 3. WITHOUT A WIRED RESOLVER, --via is refused — never silently downgraded to
	// a local dial, which would reach a different server than the template asked for.
	var errOut bytes.Buffer
	code := CommandHandler(context.Background(),
		strings.NewReader(""), &bytes.Buffer{}, &errOut,
		[]string{"http://127.0.0.1:8080/mcp", "--via", "prod-box"}, nil)
	if code == 0 {
		t.Fatalf("--via with no resolver must fail, got exit 0")
	}
	if !strings.Contains(errOut.String(), "--via is not available") {
		t.Errorf("the refusal should name the missing resolver, got: %q", errOut.String())
	}
}

// TestViaResolverFailureIsReported pins that a resolver that cannot reach the
// profile says so, instead of the bridge starting and then failing at the first
// request with a connection error naming the wrong host.
func TestViaResolverFailureIsReported(t *testing.T) {
	var errOut bytes.Buffer
	code := CommandHandler(context.Background(),
		strings.NewReader(""), &bytes.Buffer{}, &errOut,
		[]string{"http://127.0.0.1:8080/mcp", "--via", "nosuchhost"},
		func(ctx context.Context, profile string) (func(network, addr string) (net.Conn, error), func(), error) {
			return nil, nil, net.ErrClosed
		})
	if code == 0 {
		t.Fatal("a resolver failure must fail the bridge")
	}
	if !strings.Contains(errOut.String(), "cannot dial through") {
		t.Errorf("the refusal should name the profile, got: %q", errOut.String())
	}
}
