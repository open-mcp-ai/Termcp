package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

type captureAuditSink struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (s *captureAuditSink) Record(_ context.Context, event AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *captureAuditSink) snapshot() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEvent(nil), s.events...)
}

type previewStub struct {
	input PolicyPreviewInput
}

func (p *previewStub) PreviewPolicy(_ context.Context, _ AgentPolicy, input PolicyPreviewInput) PolicyPreview {
	p.input = input
	return PolicyPreview{Allowed: false, Reason: "missing shell.key", Executed: true}
}

func TestPermissionPresetsKeepRiskyActionsExplicit(t *testing.T) {
	presets := groupPresets()
	if len(presets) != 5 {
		t.Fatalf("preset count = %d", len(presets))
	}
	byID := map[string]groupPreset{}
	for _, preset := range presets {
		byID[preset.ID] = preset
		for id, grant := range preset.Policy.Actions {
			if err := authorization.ValidateGrant(id, grant); err != nil {
				t.Errorf("%s/%s: %v", preset.ID, id, err)
			}
		}
	}
	noAccess := byID["no-access"].Policy
	if len(noAccess.Actions) != 0 || noAccess.HostMode != "all" {
		t.Fatal("initial no-access preset grants an operation", noAccess)
	}
	viewer := byID["viewer"].Policy
	for _, denied := range []string{"shell.output", "shell.input", "shell.key", "file.read", "file.write", "mcp.open"} {
		if viewer.AllowsOperation(denied) {
			t.Errorf("viewer grants %s", denied)
		}
	}
	diagnostics := byID["diagnostics"].Policy
	if !diagnostics.AllowsOperation("shell.output") || !diagnostics.AllowsOperation("file.read") ||
		diagnostics.AllowsOperation("shell.input") || diagnostics.AllowsOperation("file.write") {
		t.Fatal("diagnostics preset is not read-only")
	}
	operator := byID["host-operator"].Policy
	for _, id := range []string{"shell.detect", "shell.input", "shell.key", "session.start", "file.write", "forward.open", "session.delete"} {
		if !operator.AllowsOperation(id) {
			t.Errorf("host operator does not include %s", id)
		}
	}
	for _, id := range []string{"profile.create", "profile.edit", "profile.delete", "ext.create", "ext.update", "mcp.open", "cdp.open", "jrpc.open"} {
		for _, preset := range presets {
			if preset.Policy.AllowsOperation(id) {
				t.Errorf("preset %s implicitly grants high-risk action %s", preset.ID, id)
			}
		}
	}
	for _, id := range []string{"mcp.open", "cdp.open", "jrpc.open", "profile.create", "profile.edit", "profile.delete", "ext.create", "ext.update", "ext.delete"} {
		if operator.AllowsOperation(id) {
			t.Errorf("host operator grants global or raw action %s", id)
		}
	}
}

func TestAccessPreviewRequiresAdminAndNeverExecutes(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	p := EmptyAgentPolicy()
	p.Actions["shell.input"] = authorization.Grant{Effect: authorization.EffectAllow}
	body, _ := json.Marshal(map[string]any{
		"policy": p,
		"cmd":    "run placeholder",
	})
	h := m.Wrap(http.NotFoundHandler())
	if w := managedRequest(t, h, "POST", "/auth/access/preview", string(body), cookie, session.CSRF); w.Code != http.StatusNotImplemented {
		t.Fatalf("missing previewer: %d %s", w.Code, w.Body)
	}
	preview := &previewStub{}
	m.SetPolicyPreviewer(preview)
	w := managedRequest(t, h, "POST", "/auth/access/preview", string(body), cookie, session.CSRF)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	var got PolicyPreview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Executed || got.Allowed || got.Reason != "missing shell.key" || preview.input.Cmd != "run placeholder" {
		t.Fatalf("preview result/input = %+v / %+v", got, preview.input)
	}
	agent := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"preview-agent"}`)
	if w := bearerRequest(h, "POST", "/auth/access/preview", agent.Token, string(body)); w.Code != http.StatusForbidden {
		t.Fatalf("Agent reached preview: %d", w.Code)
	}
}

func TestPermissionManagementAuditIsStructuredAndSecretFree(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	sink := &captureAuditSink{}
	m.SetAuditSink(sink)
	h := m.Wrap(http.NotFoundHandler())
	w := managedRequest(t, h, "POST", "/auth/groups", `{"name":"audited group","preset_id":"viewer"}`, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var created struct {
		Group groupWire `json:"group"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"revision": created.Group.Revision, "policy": EmptyAgentPolicy()})
	if w := managedRequest(t, h, "PATCH", "/auth/groups/"+created.Group.ID, string(body), cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := managedRequest(t, h, "PATCH", "/auth/groups/"+created.Group.ID, string(body), cookie, session.CSRF); w.Code != 409 {
		t.Fatal("stale update was not refused")
	}
	events := sink.snapshot()
	if len(events) != 3 {
		t.Fatalf("audit events = %d: %+v", len(events), events)
	}
	if events[0].Action != "group.create" || events[1].Action != "group.update" ||
		events[2].Result != "error" || events[2].Reason == "" {
		t.Fatalf("unexpected audit sequence: %+v", events)
	}
	if events[0].TargetID != created.Group.ID || events[0].AfterRevision == 0 || events[0].ActorID == "" || events[0].ActorRole != RoleAdmin {
		t.Fatalf("incomplete create audit: %+v", events[0])
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{testPassword, session.CSRF, "MCP raw"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("audit leaked %q: %s", secret, raw)
		}
	}
}
