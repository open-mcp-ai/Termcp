package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

func TestAgentV2PolicyValidationPersistenceAndDowngrade(t *testing.T) {
	var unknown AgentPolicy
	if err := json.Unmarshal([]byte(`{"schema_version":2,"host_mode":"all","actions":{"shell.key":{"effect":"allow","constraints":{"future_condition":true}}}}`), &unknown); err == nil {
		t.Fatal("persistent policy ignored an unknown condition")
	}
	m, cookie, sess := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	created := issueTestCredential(t, m, cookie, sess, "/auth/agents", `{"name":"limited","policy":{"schema_version":2,"host_mode":"all","hosts":[],"actions":{"shell.key":{"effect":"allow","constraints":{"keys":["tab"],"repeat_max":1}},"file.realpath":{"effect":"allow"}}}}`)
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	p := stateAgentPolicy(loaded.state, created.Agent.ID)
	if !p.IsV2() || !p.AllowsOperation("file.realpath") || p.AllowsOperation("file.truncate") {
		t.Fatal("fine-grained grants did not round-trip", p)
	}
	if c := p.Actions["shell.key"].Constraints; c == nil || c.RepeatMax == nil || *c.RepeatMax != 1 || len(c.Keys) != 1 || c.Keys[0] != "tab" {
		t.Fatal("constraints did not round-trip")
	}
	// Reads of policies cannot mutate live nested constraints through shared pointers.
	clone := clonePolicy(p)
	*clone.Actions["shell.key"].Constraints.RepeatMax = 9
	clone.Actions["shell.key"].Constraints.Keys[0] = "enter"
	if *p.Actions["shell.key"].Constraints.RepeatMax != 1 || p.Actions["shell.key"].Constraints.Keys[0] != "tab" {
		t.Fatal("policy clone retained mutable constraint state")
	}
	w := managedRequest(t, h, "PATCH", "/auth/agents/"+created.Agent.ID, `{"revision":1,"policy":{"functions":["terminal_write"],"host_mode":"all","hosts":[]}}`, cookie, sess.CSRF)
	if w.Code != 409 {
		t.Fatalf("legacy client replaced fine policy: %d %s", w.Code, w.Body)
	}
	invalid := []string{
		`{"schema_version":2,"host_mode":"all","actions":{"file.unknown":{"effect":"allow"}}}`,
		`{"schema_version":2,"host_mode":"all","actions":{"shell.key":{"effect":"ask"}}}`,
		`{"schema_version":2,"host_mode":"all","actions":{"shell.key":{"effect":"allow","constraints":{"unknown":true}}}}`,
		`{"schema_version":2,"host_mode":"all","actions":{"session.start":{"effect":"allow","constraints":{"keys":["tab"]}}}}`,
		`{"schema_version":2,"host_mode":"all","actions":{"file.write":{"effect":"allow","constraints":{"forms":["imaginary"]}}}}`,
		`{"schema_version":2,"host_mode":"all","actions":{"file.replace":{"effect":"allow","constraints":{"max_bytes":4}}}}`,
		`{"schema_version":2,"host_mode":"selected","hosts":[],"actions":{"ext.create":{"effect":"allow"}}}`,
		`{"host_mode":"all","actions":{"shell.input":{"effect":"allow"}}}`,
		`{"schema_version":3,"host_mode":"all","actions":{}}`,
		`{"schema_version":2,"host_mode":"all","functions":["file_write"],"actions":{}}`,
	}
	for _, policy := range invalid {
		w = managedRequest(t, h, "POST", "/auth/agents", `{"name":"invalid","issue_token":false,"policy":`+policy+`}`, cookie, sess.CSRF)
		if w.Code != 400 {
			t.Errorf("invalid policy accepted: %s => %d %s", policy, w.Code, w.Body)
		}
	}
	cookie, _, err = loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	catalog := managedRequest(t, loaded.Wrap(http.NotFoundHandler()), "GET", "/auth/access", "", cookie, "")
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), `"id":"shell.marks"`) || !strings.Contains(catalog.Body.String(), `"approval_supported":false`) {
		t.Fatalf("missing action catalog: %d %s", catalog.Code, catalog.Body)
	}
}

func TestAgentV2UpgradePreviewIsExplicitAndConservative(t *testing.T) {
	m, cookie, sess := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, sess, "/auth/agents", `{"name":"legacy","policy":{"functions":["file_write","alias"],"host_mode":"selected","hosts":[]}}`)
	h := m.Wrap(http.NotFoundHandler())
	w := managedRequest(t, h, "POST", "/auth/agents/"+a.Agent.ID+"/policy-upgrade-preview", "", cookie, sess.CSRF)
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	var preview struct {
		Policy   AgentPolicy `json:"policy"`
		Revision uint64      `json:"revision"`
		Notes    []string    `json:"notes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Revision != 1 || len(preview.Notes) == 0 || !preview.Policy.IsV2() || !preview.Policy.AllowsOperation("file.write") || preview.Policy.AllowsOperation("file.realpath") || preview.Policy.AllowsOperation("alias.write") {
		t.Fatal("unsafe upgrade candidate", preview)
	}
	if stateAgentPolicy(m.state, a.Agent.ID).IsV2() {
		t.Fatal("preview changed live policy")
	}
	body, _ := json.Marshal(map[string]any{"revision": preview.Revision, "policy": preview.Policy})
	w = managedRequest(t, h, "PATCH", "/auth/agents/"+a.Agent.ID, string(body), cookie, sess.CSRF)
	if w.Code != 200 || !stateAgentPolicy(m.state, a.Agent.ID).IsV2() {
		t.Fatalf("explicit upgrade: %d %s", w.Code, w.Body)
	}
	for _, group := range FunctionGroups() {
		p := AgentPolicy{HostMode: "all", Functions: []string{group.ID}}
		if err := validateAgentPolicy(conservativeUpgrade(p)); err != nil {
			t.Fatalf("invalid upgrade for %s: %v", group.ID, err)
		}
	}
	for id := range conservativeUpgrade(FullAgentPolicy()).Actions {
		if err := authorization.ValidateGrant(id, authorization.Grant{Effect: authorization.EffectAllow}); err != nil {
			t.Fatal(err)
		}
	}
}
