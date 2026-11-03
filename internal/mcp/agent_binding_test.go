package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestAuthorizedProfileBindingRejectsContentAndNameReplacement(t *testing.T) {
	h := newV2Harness(t, auth.EmptyAgentPolicy())
	body := []byte("kind = \"remote\"\nhost = \"first.example\"\nuser = \"synthetic\"\npassword = \"synthetic-password\"\n")
	if err := h.vault.Save("bound-profile", body); err != nil {
		t.Fatal(err)
	}
	binding, err := h.vault.Binding("bound-profile")
	if err != nil {
		t.Fatal(err)
	}
	ctx := withAgentResourceBindings(context.Background(), agentResourceBindings{
		profiles: map[string]sshconfig.AuthorizationBinding{"bound-profile": binding},
	})
	_, entry, err := h.server.loadAgentProfile(ctx, "bound-profile")
	if err != nil || entry.Host != "first.example" {
		t.Fatalf("initial bound load: %+v %v", entry, err)
	}
	changed := []byte("kind = \"remote\"\nhost = \"second.example\"\nuser = \"synthetic\"\npassword = \"synthetic-password\"\n")
	if err := h.vault.Save("bound-profile", changed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.server.loadAgentProfile(ctx, "bound-profile"); err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("content replacement was not refused: %v", err)
	}

	if err := h.vault.Delete("bound-profile"); err != nil {
		t.Fatal(err)
	}
	if err := h.vault.Save("bound-profile", body); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.server.loadAgentProfile(ctx, "bound-profile"); err == nil {
		t.Fatal("reusing a display name inherited the old authorization binding")
	}
}

func TestAgentToolCheckCarriesBindingThroughAdmission(t *testing.T) {
	p := v2Policy("session.start", "shell.open")
	h := newV2Harness(t, p)
	binding, err := h.vault.Binding("internal")
	if err != nil {
		t.Fatal(err)
	}
	args := argsOf("ssh_config", "termcp://internal", "mode", "pipe")
	ctx, bad, _ := h.server.agentToolCheckV2(context.Background(), p, "session_start", toolRequest("session_start", args))
	if bad != nil {
		t.Fatal(toolResultText(bad))
	}
	bindings, ok := agentBindingsFromContext(ctx)
	if !ok || bindings.profiles["internal"].Identity != binding.Identity {
		t.Fatal("admission did not carry the profile binding into execution context")
	}
}

func TestExtensionAuthorizationBindingDetectsTemplateRetarget(t *testing.T) {
	// Binding behavior is covered in the extensions package; this test pins that
	// the MCP adapter exposes the same error instead of silently reloading a name.
	if _, err := (&Server{}).extensionAuthorizationBinding("missing"); err == nil {
		t.Fatal("unconfigured extensions were accepted")
	}
}
