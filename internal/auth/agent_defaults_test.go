package auth

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNewPrincipalUsesDefaultGroupWithoutIssuingCredential(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "POST", "/auth/principals", `{"name":"client"}`, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Principal agentRecord `json:"principal"`
		Token     string      `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token != "" || len(m.state.Tokens) != 0 {
		t.Fatal("principal creation implicitly issued a credential")
	}
	p := stateAgentPolicy(m.state, out.Principal.ID)
	if p.SchemaVersion != 2 || len(p.Actions) != 0 || p.HostMode != "all" {
		t.Fatal("principal did not inherit the empty default group", p)
	}
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"explicit-client"}`)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, restricted, err := PolicyFromContext(r.Context())
		if err != nil || !restricted || p.AllowsOperation("session.start") {
			t.Error("empty group received business authority")
		}
		w.WriteHeader(204)
	}))
	if w := bearerRequest(h, "POST", "/stream", a.Token, ""); w.Code != 204 {
		t.Fatal("empty policy lost authentication", w.Code)
	}
}

func TestRetiredPermissionShapesAndRoutesAreRejected(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	for _, policy := range []string{
		`{"host_mode":"all","functions":["terminal_write"]}`,
		`{"schema_version":1,"host_mode":"all","actions":{}}`,
		`{"schema_version":2,"host_mode":"all","functions":[],"actions":{}}`,
	} {
		w := managedRequest(t, h, "POST", "/auth/groups", `{"name":"retired","policy":`+policy+`}`, cookie, session.CSRF)
		if w.Code != 400 {
			t.Fatal("retired policy was accepted", w.Code, w.Body)
		}
	}
	if w := managedRequest(t, h, "POST", "/auth/principals", `{"name":"retired","policy":{"schema_version":2,"host_mode":"all","actions":{}}}`, cookie, session.CSRF); w.Code != 400 {
		t.Fatal("principal-local policy was accepted", w.Code)
	}
	if w := managedRequest(t, h, "POST", "/auth/groups/"+m.state.DefaultGroupID+"/policy-upgrade-preview", "", cookie, session.CSRF); w.Code != 404 {
		t.Fatal("retired upgrade route survived", w.Code)
	}
	if w := managedRequest(t, h, "GET", "/auth/agents", "", cookie, ""); w.Code != 404 {
		t.Fatal("retired management route survived", w.Code)
	}
	if w := managedRequest(t, h, "POST", "/auth/tokens", `{"agent_id":"retired","label":"legacy"}`, cookie, session.CSRF); w.Code != 400 {
		t.Fatal("retired token identity alias survived", w.Code)
	}
}

func TestRetiredPermissionStateIsRejectedWithoutMigration(t *testing.T) {
	t.Run("state version", func(t *testing.T) {
		m, _, _ := configuredManaged(t)
		state := credentialState(m.state)
		state.Version = 2
		writeManagedState(t, m, state)
		if loaded, err := reopenManaged(t, m); err == nil {
			_ = loaded
			t.Fatal("retired permission state was silently migrated")
		}
	})

	t.Run("group function groups", func(t *testing.T) {
		m, _, _ := configuredManaged(t)
		groupID := m.state.DefaultGroupID
		mutateManagedState(t, m, func(state map[string]any) {
			groups := state["groups"].(map[string]any)
			group := groups[groupID].(map[string]any)
			group["policy"] = map[string]any{
				"host_mode": "all",
				"functions": []any{"terminal_write"},
			}
		})
		if loaded, err := reopenManaged(t, m); err == nil {
			_ = loaded
			t.Fatal("retired group policy loaded as the current model")
		}
	})

	t.Run("principal policy", func(t *testing.T) {
		m, cookie, session := configuredManaged(t)
		a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"retired-policy"}`)
		mutateManagedState(t, m, func(state map[string]any) {
			principals := state["agents"].(map[string]any)
			principal := principals[a.Agent.ID].(map[string]any)
			principal["policy"] = map[string]any{
				"schema_version": 2,
				"host_mode":      "all",
				"actions":        map[string]any{},
			}
		})
		if loaded, err := reopenManaged(t, m); err == nil {
			_ = loaded
			t.Fatal("retired principal-local policy was accepted")
		}
	})
}

func writeManagedState(t *testing.T, m *Managed, state managedState) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if err := m.vault.WriteAuthentication(raw); err != nil {
		t.Fatal(err)
	}
}

func mutateManagedState(t *testing.T, m *Managed, mutate func(map[string]any)) {
	t.Helper()
	raw, err := json.Marshal(credentialState(m.state))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	mutate(state)
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if err := m.vault.WriteAuthentication(raw); err != nil {
		t.Fatal(err)
	}
}
