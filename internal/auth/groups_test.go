package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/configstore"
)

type groupWire struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Enabled         bool        `json:"enabled"`
	Revision        uint64      `json:"revision"`
	Policy          AgentPolicy `json:"policy"`
	Warnings        []string    `json:"policy_warnings"`
	MemberCount     int         `json:"member_count"`
	CredentialCount int         `json:"credential_count"`
}

func createTestGroup(t *testing.T, m *Managed, cookie string, session sessionRecord, name, preset string) groupWire {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "preset_id": preset})
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "POST", "/auth/groups", string(body), cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("create group: %d %s", w.Code, w.Body)
	}
	var out struct {
		Group groupWire `json:"group"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Group
}
func TestPermissionGroupsSharePolicyWithoutSharingIdentity(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	group := createTestGroup(t, m, cookie, session, "shared observers", "viewer")
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"one","group_id":"`+group.ID+`"}`)
	b := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"two","group_id":"`+group.ID+`"}`)
	check := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFromContext(r.Context())
		if !ok || actor.Role != RoleAgent {
			t.Error("identity lost")
		}
		p, restricted, err := PolicyFromContext(r.Context())
		if err != nil || !restricted || !p.AllowsOperation("profile.list") || p.AllowsOperation("shell.input") {
			t.Error("wrong group policy")
		}
		_ = json.NewEncoder(w).Encode(actor)
	}))
	for _, a := range []issuedCredential{a, b} {
		w := bearerRequest(check, "POST", "/stream", a.Token, "")
		var actor Actor
		_ = json.Unmarshal(w.Body.Bytes(), &actor)
		if w.Code != 200 || actor.ID != a.Agent.ID || actor.ID == group.ID {
			t.Fatal("group replaced the subject identity")
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/stream", nil)
	r.Header.Set("Authorization", "Bearer "+a.Token)
	live, done, status := m.admitToken(r)
	if status != 0 {
		t.Fatal(status)
	}
	defer done()
	body, _ := json.Marshal(map[string]any{"revision": group.Revision, "name": "renamed observers", "policy": EmptyAgentPolicy()})
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "PATCH", "/auth/groups/"+group.ID, string(body), cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("update group: %d %s", w.Code, w.Body)
	}
	if live.Err() == nil {
		t.Fatal("group policy update left existing request running")
	}
	for _, client := range []issuedCredential{a, b} {
		h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _, err := PolicyFromContext(r.Context())
			if err != nil || p.AllowsOperation("profile.list") {
				t.Error("token retained the previous group grant")
			}
			w.WriteHeader(204)
		}))
		if w := bearerRequest(h, "POST", "/stream", client.Token, ""); w.Code != 204 {
			t.Fatal("policy save changed the identity or token", w.Code)
		}
	}
}

func TestGroupLifecyclePreviewConflictAndExplicitDeny(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	g := createTestGroup(t, m, cookie, session, "disposable", "viewer")
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"member","group_id":"`+g.ID+`"}`)
	w := managedRequest(t, h, "POST", "/auth/groups/"+g.ID+"/clone", `{"name":"copied"}`, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("clone: %d %s", w.Code, w.Body)
	}
	var copied struct {
		Group groupWire `json:"group"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &copied)
	if copied.Group.ID == g.ID || copied.Group.MemberCount != 0 || copied.Group.CredentialCount != 0 {
		t.Fatal("clone copied identity or credentials")
	}
	preview := managedRequest(t, h, "POST", "/auth/groups/"+g.ID+"/delete-preview", "", cookie, session.CSRF)
	if preview.Code != 200 {
		t.Fatal(preview.Code, preview.Body)
	}
	var plan struct {
		Revision     uint64 `json:"revision"`
		PlanRevision uint64 `json:"plan_revision"`
		MemberCount  int    `json:"member_count"`
	}
	_ = json.Unmarshal(preview.Body.Bytes(), &plan)
	if plan.MemberCount != 1 {
		t.Fatal("preview omitted bound members")
	}
	// Issuing another token invalidates the deletion inventory version.
	issueTestCredential(t, m, cookie, session, "/auth/tokens", `{"principal_id":"`+a.Agent.ID+`","label":"second"}`)
	body, _ := json.Marshal(map[string]any{"revision": plan.Revision, "plan_revision": plan.PlanRevision, "resolution": "deny"})
	if w := managedRequest(t, h, "DELETE", "/auth/groups/"+g.ID, string(body), cookie, session.CSRF); w.Code != 409 {
		t.Fatal("stale deletion preview accepted", w.Code)
	}
	preview = managedRequest(t, h, "POST", "/auth/groups/"+g.ID+"/delete-preview", "", cookie, session.CSRF)
	_ = json.Unmarshal(preview.Body.Bytes(), &plan)
	body, _ = json.Marshal(map[string]any{"revision": plan.Revision, "plan_revision": plan.PlanRevision, "resolution": "deny"})
	if w := managedRequest(t, h, "DELETE", "/auth/groups/"+g.ID, string(body), cookie, session.CSRF); w.Code != 200 {
		t.Fatal("confirmed delete failed", w.Code, w.Body)
	}
	if w := bearerRequest(checkNoBusinessHandler(t, m), "POST", "/stream", a.Token, ""); w.Code != 401 {
		t.Fatal("deleted group left an effective orphan credential", w.Code)
	}
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.state.Groups[g.ID]; ok {
		t.Fatal("deleted group came back after restart")
	}
	if loaded.state.Agents[a.Agent.ID].ID != a.Agent.ID {
		t.Fatal("group deletion erased its subject identity")
	}
}
func checkNoBusinessHandler(t *testing.T, m *Managed) http.Handler {
	return m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("business handler reached") }))
}

func TestGroupDefaultScopeAndAdminOnlyManagement(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	g := createTestGroup(t, m, cookie, session, "new default", "viewer")
	w := managedRequest(t, h, "GET", "/auth/groups", "", cookie, "")
	var listing struct {
		AccessRevision uint64 `json:"access_revision"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listing)
	body, _ := json.Marshal(map[string]any{"group_id": g.ID, "revision": listing.AccessRevision})
	if w := managedRequest(t, h, "PUT", "/auth/default-group", string(body), cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"defaulted"}`)
	if p := stateAgentPolicy(m.state, a.Agent.ID); !p.AllowsOperation("profile.list") {
		t.Fatal("principal did not inherit configured default")
	}
	for _, path := range []string{"/auth/groups", "/auth/group-presets", "/auth/principals", "/auth/default-group"} {
		if w := bearerRequest(h, "GET", path, a.Token, ""); w.Code != 403 {
			t.Fatal("Agent reached group management", path, w.Code)
		}
	}
	if w := managedRequest(t, h, "POST", "/auth/groups", `{"name":"owner","preset_id":"owner"}`, cookie, session.CSRF); w.Code != 400 {
		t.Fatal("Agent group accepted a system administrator preset")
	}
	if w := managedRequest(t, h, "POST", "/auth/groups", `{"name":"new default"}`, cookie, session.CSRF); w.Code != 409 {
		t.Fatal("duplicate group name accepted")
	}
	if w := managedRequest(t, h, "PATCH", "/auth/groups/"+g.ID, `{"revision":0,"name":"changed"}`, cookie, session.CSRF); w.Code != 409 {
		t.Fatal("stale group revision accepted")
	}
	presets := managedRequest(t, h, "GET", "/auth/group-presets", "", cookie, "")
	if presets.Code != 200 || !strings.Contains(presets.Body.String(), "diagnostics") {
		t.Fatal("preset catalog missing")
	}
}

func TestGroupMutationFailureLeavesLivePolicyAndRequests(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	g := createTestGroup(t, m, cookie, session, "durability observers", "viewer")
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"member","group_id":"`+g.ID+`"}`)
	req := httptest.NewRequest("POST", "http://127.0.0.1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	live, finish, status := m.admitToken(req)
	if status != 0 {
		t.Fatal(status)
	}
	defer finish()
	cipher := configstore.LocalStatePath(filepath.Dir(m.path))
	if err := os.Remove(cipher); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cipher, 0700); err != nil {
		t.Fatal(err)
	}
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "PATCH", "/auth/groups/"+g.ID, `{"revision":1,"policy":{"schema_version":2,"host_mode":"all","actions":{}}}`, cookie, session.CSRF)
	if w.Code != 500 {
		t.Fatal("write failure did not surface", w.Code)
	}
	if !stateAgentPolicy(m.state, a.Agent.ID).AllowsOperation("profile.list") || live.Err() != nil {
		t.Fatal("failed save changed live group access")
	}
}

func TestUpgradePresetGroupsRefreshesOnlyUnmodifiedShippedPolicy(t *testing.T) {
	presets := map[string]groupPreset{}
	for _, preset := range groupPresets() {
		presets[preset.ID] = preset
	}
	legacyOperator := groupPresetsV1()["host-operator"]
	legacyMaintainer := groupPresetsV1()["file-maintainer"]
	legacyV2Operator := groupPresetsV2()["host-operator"]
	operator := permissionGroup{
		ID: "preset-host-operator", Name: legacyOperator.Name, Description: legacyOperator.Description,
		Enabled: true, Revision: 7, PresetID: legacyOperator.ID, PresetVersion: legacyOperator.Version,
		Policy: clonePolicy(legacyOperator.Policy),
	}
	maintainer := permissionGroup{
		ID: "preset-file-maintainer", Name: legacyMaintainer.Name, Description: "operator reviewed",
		Enabled: true, Revision: 9, PresetID: legacyMaintainer.ID, PresetVersion: legacyMaintainer.Version,
		Policy: clonePolicy(legacyMaintainer.Policy),
	}
	maintainer.Policy.Actions["file.delete"] = authorization.Grant{Effect: authorization.EffectAsk}
	v2Operator := permissionGroup{
		ID: "preset-diagnostics", Name: "custom diagnostics", Description: legacyV2Operator.Description,
		Enabled: true, Revision: 4, PresetID: "diagnostics", PresetVersion: 2,
		Policy: clonePolicy(groupPresetsV2()["diagnostics"].Policy),
	}
	state := managedState{
		Groups:         map[string]permissionGroup{operator.ID: operator, maintainer.ID: maintainer, v2Operator.ID: v2Operator},
		AccessRevision: 12,
	}

	if !upgradePresetGroups(&state) {
		t.Fatal("v1 seeded presets were not upgraded")
	}
	gotOperator := state.Groups[operator.ID]
	if gotOperator.PresetVersion != presets["host-operator"].Version ||
		!gotOperator.Policy.AllowsOperation("shell.detect") ||
		!reflect.DeepEqual(gotOperator.Policy, presets["host-operator"].Policy) {
		t.Fatalf("host operator was not refreshed: %+v", gotOperator)
	}
	if gotOperator.Revision != operator.Revision+1 || state.AccessRevision != 13 {
		t.Fatal("preset refresh did not advance policy revisions")
	}
	gotV2 := state.Groups[v2Operator.ID]
	if gotV2.PresetVersion != presets["diagnostics"].Version || !reflect.DeepEqual(gotV2.Policy, presets["diagnostics"].Policy) {
		t.Fatalf("v2 built-in preset was not refreshed: %+v", gotV2)
	}
	gotMaintainer := state.Groups[maintainer.ID]
	if !reflect.DeepEqual(gotMaintainer, maintainer) {
		t.Fatal("preset refresh overwrote an administrator-edited group")
	}
}
