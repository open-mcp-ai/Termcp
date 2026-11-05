package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
)

func TestMCPClientCredentialNeverBecomesDaemonCredential(t *testing.T) {
	t.Setenv(config.EnvMCPToken, "synthetic-client-credential")
	cfg := config.Default()
	cfg.MCPToken = "synthetic-client-credential"
	if !cfg.ManagedAuth() || ManagementCredential(cfg) != "" {
		t.Fatal("client token selected server legacy mode")
	}
	if BridgeCredential(cfg) != cfg.MCPToken {
		t.Fatal("bridge did not select the MCP client token")
	}
	for _, kv := range childEnv(cfg) {
		if strings.HasPrefix(strings.ToUpper(kv), config.EnvMCPToken+"=") || strings.Contains(kv, cfg.MCPToken) {
			t.Fatal("client credential leaked into daemon environment")
		}
	}
	if strings.Contains(strings.Join(ChildArgs(cfg, "30s"), " "), cfg.MCPToken) {
		t.Fatal("client token leaked into daemon argv")
	}
}

func TestExplicitLegacyCredentialWinsDaemonManagement(t *testing.T) {
	cfg := config.Default()
	cfg.AuthToken = "synthetic-legacy-credential"
	cfg.MCPToken = "synthetic-mcp-credential"
	if got := ManagementCredential(cfg); got != cfg.AuthToken {
		t.Fatalf("daemon credential = %q, want the explicit legacy credential", got)
	}
}

func TestMCPProbeHasNoDaemonManagementDependency(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/mcp" {
			t.Errorf("management entrance requested: %s", r.URL.Path)
			http.Error(w, "forbidden", 403)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-probe-credential" {
			http.Error(w, "unauthorized", 401)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"idle_timeout_ms":6000}`))
	}))
	defer ts.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	idle, err := ProbeMCP(context.Background(), host, port, "synthetic-probe-credential")
	if err != nil || idle != 6*time.Second {
		t.Fatalf("probe: %v %v", idle, err)
	}
	if _, err = ProbeMCP(context.Background(), host, port, "wrong"); err == nil {
		t.Fatal("probe accepted wrong token")
	}
}
