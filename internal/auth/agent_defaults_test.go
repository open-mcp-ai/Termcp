package auth

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNewAgentDefaultsToEmptyV2Policy(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"new-client"}`)
	if a.Agent.Policy == nil || a.Agent.Policy.Actions == nil {
		t.Error("V2 API omitted the explicit empty actions object")
	}
	policy := stateAgentPolicy(m.state, a.Agent.ID)
	if !policy.IsV2() || len(policy.Actions) != 0 || len(policy.Functions) != 0 || policy.HostMode != "all" {
		t.Fatalf("new identity did not get the explicit minimum policy: %+v", policy)
	}
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, restricted, err := PolicyFromContext(r.Context())
		if err != nil || !restricted || !p.IsV2() || p.AllowsOperation("session.start") || p.Allows("terminal_write") {
			t.Error("new token received business permissions")
		}
		w.WriteHeader(204)
	}))
	if w := bearerRequest(h, "POST", "/stream", a.Token, ""); w.Code != 204 {
		t.Fatalf("minimum policy lost authentication: %d", w.Code)
	}
}

func TestNewAgentDefaultLeavesHistoricalNilPolicyUnchanged(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"new-client"}`)
	// Simulate an actual v2 ciphertext, rather than writing an invalid v3 subject.
	state := m.state
	state.Version = 2
	state.Groups = nil
	state.DefaultGroupID = ""
	state.AccessRevision = 0
	state.Agents = map[string]agentRecord{}
	const legacyID = "historical-principal"
	state.Agents[legacyID] = agentRecord{ID: legacyID, Name: "legacy-client", Enabled: true, CreatedAt: m.now().UTC()}
	policy := EmptyAgentPolicy()
	state.Agents[a.Agent.ID] = agentRecord{ID: a.Agent.ID, Name: "new-client", Enabled: true, CreatedAt: m.now().UTC(), Revision: 1, Policy: &policy}
	wire, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(wire)
	if err = m.vault.WriteAuthentication(wire); err != nil {
		t.Fatal(err)
	}
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !stateAgentPolicy(loaded.state, legacyID).Allows("terminal_write") {
		t.Fatal("restart changed historical nil policy")
	}
	if p := stateAgentPolicy(loaded.state, a.Agent.ID); !p.IsV2() || p.AllowsOperation("shell.input") {
		t.Fatal("new default did not persist")
	}
	raw, err := loaded.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var saved managedState
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Agents[a.Agent.ID].GroupID == "" {
		t.Fatal("new policy binding was not persisted")
	}
}
