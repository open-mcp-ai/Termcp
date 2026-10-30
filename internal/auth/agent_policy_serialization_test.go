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
	body := `{"name":"empty-selections","issue_token":false,"policy":{"schema_version":2,"host_mode":"all","hosts":[],"actions":{"shell.key":{"effect":"allow","constraints":{"keys":[]}},"mcp.tool.invoke":{"effect":"allow","constraints":{"tool_names":[]}},"cdp.method.invoke":{"effect":"allow","constraints":{"methods":[]}},"file.write":{"effect":"allow","constraints":{"forms":[]}}}}}`
	w := managedRequest(t, h, "POST", "/auth/agents", body, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		Agent agentRecord `json:"agent"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	assertEmptySelectionsDeny(t, *created.Agent.Policy)
	// A write/read through the real encrypted vault must retain the empty sets too.
	raw, err := m.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var saved managedState
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	assertEmptySelectionsDeny(t, stateAgentPolicy(saved, created.Agent.ID))
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	cookie, session, err = loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	w = managedRequest(t, loaded.Wrap(http.NotFoundHandler()), "GET", "/auth/agents", "", cookie, "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var list struct {
		Agents []agentRecord `json:"agents"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Agents) != 1 {
		t.Fatal("missing restored Agent")
	}
	assertEmptySelectionsDeny(t, *list.Agents[0].Policy)
	payload, _ := json.Marshal(map[string]any{"revision": list.Agents[0].Revision, "policy": list.Agents[0].Policy})
	w = managedRequest(t, loaded.Wrap(http.NotFoundHandler()), "PATCH", "/auth/agents/"+created.Agent.ID, string(payload), cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("resave: %d %s", w.Code, w.Body)
	}
	assertEmptySelectionsDeny(t, stateAgentPolicy(loaded.state, created.Agent.ID))
}

func assertEmptySelectionsDeny(t *testing.T, p AgentPolicy) {
	t.Helper()
	for _, req := range []authorization.Requirement{
		{Operation: "shell.key", Evidence: authorization.Evidence{Keys: []string{"enter"}, Repeat: 1}},
		{Operation: "mcp.tool.invoke", Evidence: authorization.Evidence{ToolName: "execute"}},
		{Operation: "cdp.method.invoke", Evidence: authorization.Evidence{Method: "Runtime.evaluate"}},
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
			a := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"old-policy","policy":{"schema_version":2,"host_mode":"all","hosts":[],"actions":{"shell.key":{"effect":"allow","constraints":{"keys":["tab"]}},"file.realpath":{"effect":"allow"}}}}`)
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
			if !p.AllowsOperation("file.realpath") {
				t.Fatal("explicit unconstrained allow changed")
			}
			cookie, session, err = loaded.login("admin", testPassword)
			if err != nil {
				t.Fatal(err)
			}
			h := loaded.Wrap(http.NotFoundHandler())
			w := managedRequest(t, h, "GET", "/auth/agents", "", cookie, "")
			var list struct {
				Agents []struct {
					ID       string   `json:"id"`
					Revision uint64   `json:"revision"`
					Warnings []string `json:"policy_warnings"`
				} `json:"agents"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Agents) != 1 || len(list.Agents[0].Warnings) != 1 || list.Agents[0].Warnings[0] != "shell.key" {
				t.Fatal("missing operation review notice", w.Body)
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
			payload, _ := json.Marshal(map[string]any{"revision": list.Agents[0].Revision, "policy": p})
			w = managedRequest(t, h, "PATCH", "/auth/agents/"+a.Agent.ID, string(payload), cookie, session.CSRF)
			if w.Code != 200 {
				t.Fatalf("review save: %d %s", w.Code, w.Body)
			}
			var updated struct {
				Agent struct {
					Warnings []string `json:"policy_warnings"`
				} `json:"agent"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
				t.Fatal(err)
			}
			if len(updated.Agent.Warnings) != 0 || !stateAgentPolicy(loaded.state, a.Agent.ID).AllowsOperation("shell.key") {
				t.Fatal("explicit review was not applied")
			}

		})
	}
}

func TestAgentRejectsAmbiguousEmptyConditionsAtWrite(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	for _, c := range []string{`{}`, `{"keys":null}`} {
		w := managedRequest(t, h, "POST", "/auth/agents", `{"name":"ambiguous","issue_token":false,"policy":{"schema_version":2,"host_mode":"all","actions":{"shell.key":{"effect":"allow","constraints":`+c+`}}}}`, cookie, session.CSRF)
		if w.Code != 400 {
			t.Errorf("ambiguous constraints accepted: %d", w.Code)
		}
	}
}
