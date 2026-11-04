package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

func TestAgentEmptyAllowlistsSurviveAPIAndEncryptedRestart(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	body := `{"name":"empty-selections","policy":{"schema_version":2,"host_mode":"all","hosts":[],"actions":{"shell.key":{"effect":"allow","constraints":{"keys":[]}},"mcp.open":{"effect":"allow","constraints":{"tool_names":[]}},"cdp.open":{"effect":"allow","constraints":{"methods":[]}},"file.write":{"effect":"allow","constraints":{"forms":[]}}}}}`
	w := managedRequest(t, h, "POST", "/auth/groups", body, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var created struct {
		Group groupWire `json:"group"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	assertEmptySelectionsDeny(t, created.Group.Policy)
	raw, err := m.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var saved managedState
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	assertEmptySelectionsDeny(t, saved.Groups[created.Group.ID].Policy)
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	cookie, session, err = loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	group := readTestGroup(t, loaded, cookie, created.Group.ID)
	assertEmptySelectionsDeny(t, group.Policy)
	payload, _ := json.Marshal(map[string]any{"revision": group.Revision, "policy": group.Policy})
	w = managedRequest(t, loaded.Wrap(http.NotFoundHandler()), "PATCH", "/auth/groups/"+group.ID, string(payload), cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	assertEmptySelectionsDeny(t, loaded.state.Groups[group.ID].Policy)
}

func readTestGroup(t *testing.T, m *Managed, cookie, id string) groupWire {
	t.Helper()
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "GET", "/auth/groups", "", cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var list struct {
		Groups []groupWire `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, g := range list.Groups {
		if g.ID == id {
			return g
		}
	}
	t.Fatal("missing group", id)
	return groupWire{}
}

func assertEmptySelectionsDeny(t *testing.T, p AgentPolicy) {
	t.Helper()
	for _, req := range []authorization.Requirement{
		{Operation: "shell.key", Evidence: authorization.Evidence{Keys: []string{"enter"}, Repeat: 1}},
		{Operation: "mcp.open", Evidence: authorization.Evidence{ToolName: "execute"}},
		{Operation: "cdp.open", Evidence: authorization.Evidence{Method: "Runtime.evaluate"}},
		{Operation: "file.write", Evidence: authorization.Evidence{Forms: []string{"inline"}}},
	} {
		if d := authorization.CheckPlan(p.AuthorizationPolicy(), authorization.Plan{Requirements: []authorization.Requirement{req}}); d.Allowed {
			t.Errorf("empty %s allowlist widened", req.Operation)
		}
	}
}

func TestAgentAmbiguousStoredConditionsFailClosedAndCanBeReviewed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conditions map[string]any
	}{
		{"empty", map[string]any{}},
		{"lost_keys_with_repeat", map[string]any{"repeat_max": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {

			m, cookie, session := configuredManaged(t)
			a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"old-policy","policy":{"schema_version":2,"host_mode":"all","hosts":[],"actions":{"shell.key":{"effect":"allow","constraints":{"keys":["tab"]}},"file.fs":{"effect":"allow"}}}}`)
			raw, err := m.vault.ReadAuthentication()
			if err != nil {
				t.Fatal(err)
			}
			defer clear(raw)
			var state map[string]any
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			agents := state["agents"].(map[string]any)
			groupID := agents[a.Agent.ID].(map[string]any)["group_id"].(string)
			actions := state["groups"].(map[string]any)[groupID].(map[string]any)["policy"].(map[string]any)["actions"].(map[string]any)
			actions["shell.key"] = map[string]any{"effect": "allow", "constraints": tc.conditions}
			legacy, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(legacy)
			if err = m.vault.WriteAuthentication(legacy); err != nil {
				t.Fatal(err)
			}
			loaded, err := reopenManaged(t, m)
			if err != nil {
				t.Fatal(err)
			}
			p := stateAgentPolicy(loaded.state, a.Agent.ID)
			if p.AllowsOperation("shell.key") {
				t.Fatal("ambiguous persisted condition enabled an operation")
			}
			if !p.AllowsOperation("file.fs") {
				t.Fatal("explicit unconstrained allow changed")
			}
			cookie, session, err = loaded.login("admin", testPassword)
			if err != nil {
				t.Fatal(err)
			}
			h := loaded.Wrap(http.NotFoundHandler())
			group := readTestGroup(t, loaded, cookie, a.Agent.GroupID)
			if len(group.Warnings) != 1 || group.Warnings[0] != "shell.key" {
				t.Fatal("missing group review notice", group)
			}
			raw, err = loaded.vault.ReadAuthentication()
			if err != nil {
				t.Fatal(err)
			}
			defer clear(raw)
			var saved managedState
			if err = json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if stateAgentPolicy(saved, a.Agent.ID).AllowsOperation("shell.key") {
				t.Fatal("repair was not durably denied")
			}
			// An explicit human save can choose a new allow with no constraints.
			p.Actions["shell.key"] = authorization.Grant{Effect: authorization.EffectAllow}
			payload, _ := json.Marshal(map[string]any{"revision": group.Revision, "policy": p})
			w := managedRequest(t, h, "PATCH", "/auth/groups/"+group.ID, string(payload), cookie, session.CSRF)
			if w.Code != 200 {
				t.Fatalf("review save: %d %s", w.Code, w.Body)
			}
			var updated struct {
				Group struct {
					Warnings []string `json:"policy_warnings"`
				} `json:"group"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
				t.Fatal(err)
			}
			if len(updated.Group.Warnings) != 0 || !stateAgentPolicy(loaded.state, a.Agent.ID).AllowsOperation("shell.key") {
				t.Fatal("explicit review was not applied")
			}

		})
	}
}

func TestAgentRejectsAmbiguousEmptyConditionsAtWrite(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	for _, c := range []string{`{}`, `{"keys":null}`} {
		w := managedRequest(t, h, "POST", "/auth/groups", `{"name":"ambiguous","policy":{"schema_version":2,"host_mode":"all","actions":{"shell.key":{"effect":"allow","constraints":`+c+`}}}}`, cookie, session.CSRF)
		if w.Code != 400 {
			t.Errorf("ambiguous constraints accepted: %d", w.Code)
		}
	}
}
