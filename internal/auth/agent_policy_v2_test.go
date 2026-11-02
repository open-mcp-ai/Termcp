package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAgentV2PolicyValidationPersistenceAndDowngrade(t *testing.T) {
	var unknown AgentPolicy
	if err := json.Unmarshal([]byte(`{"schema_version":2,"host_mode":"all","actions":{"shell.key":{"effect":"allow","constraints":{"future_condition":true}}}}`), &unknown); err == nil {
		t.Fatal("persistent policy ignored an unknown condition")
	}
	m, cookie, sess := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	created := issueTestCredential(t, m, cookie, sess, "/auth/principals", `{"name":"limited","policy":{"schema_version":2,"host_mode":"all","hosts":[],"actions":{"shell.key":{"effect":"allow","constraints":{"keys":["tab"],"repeat_max":1}},"file.realpath":{"effect":"allow"}}}}`)
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	cookie, sess, err = loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	m = loaded
	h = loaded.Wrap(http.NotFoundHandler())
	p := stateAgentPolicy(loaded.state, created.Agent.ID)
	if p.SchemaVersion != 2 || !p.AllowsOperation("file.realpath") || p.AllowsOperation("file.truncate") {
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
	w := managedRequest(t, h, "PATCH", "/auth/groups/"+created.Agent.GroupID, `{"revision":1,"policy":{"functions":["terminal_write"],"host_mode":"all","hosts":[]}}`, cookie, sess.CSRF)
	if w.Code != 400 {
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
		w = managedRequest(t, h, "POST", "/auth/groups", `{"name":"invalid","policy":`+policy+`}`, cookie, sess.CSRF)
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
