package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	// Rotating the active token changes the credential inventory version.
	rotated := rotateTestCredential(t, m, cookie, session, a)
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
	if w := bearerRequest(checkNoBusinessHandler(t, m), "POST", "/stream", rotated.Token, ""); w.Code != 401 {
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
		!gotOperator.Policy.AllowsOperation("shell.input") ||
		!gotOperator.Policy.AllowsOperation("session.start") ||
		!gotOperator.Policy.AllowsOperation("file.write") {
		t.Fatalf("host operator was not refreshed: %+v", gotOperator)
	}
	for _, id := range []string{"file.perm", "file.link", "file.fs", "shell.detect"} {
		if gotOperator.Policy.AllowsOperation(id) {
			t.Errorf("v1 host operator gained unapproved action %s", id)
		}
	}
	if gotOperator.Revision != operator.Revision+1 || state.AccessRevision != 13 {
		t.Fatal("preset refresh did not advance policy revisions")
	}
	gotV2 := state.Groups[v2Operator.ID]
	if gotV2.PresetVersion != presets["diagnostics"].Version ||
		!gotV2.Policy.AllowsOperation("shell.output") ||
		!gotV2.Policy.AllowsOperation("file.read") ||
		gotV2.Policy.AllowsOperation("file.write") {
		t.Fatalf("v2 built-in preset was not refreshed: %+v", gotV2)
	}
	gotMaintainer := state.Groups[maintainer.ID]
	if !reflect.DeepEqual(gotMaintainer, maintainer) {
		t.Fatal("preset refresh overwrote an administrator-edited group")
	}
}

func TestUpgradePresetGroupsRefreshesRetiredV3Vocabulary(t *testing.T) {
	legacy := groupPresetsV3()["file-maintainer"]
	group := permissionGroup{
		ID: "preset-" + legacy.ID, Name: legacy.Name, Description: legacy.Description,
		Enabled: true, Revision: 7, PresetID: legacy.ID, PresetVersion: legacy.Version,
		Policy: clonePolicy(legacy.Policy),
	}
	state := managedState{
		Groups:         map[string]permissionGroup{group.ID: group},
		AccessRevision: 12,
	}

	if !upgradePresetGroups(&state) {
		t.Fatal("retired v3 preset was not upgraded")
	}
	current := groupPresets()
	var currentMaintainer groupPreset
	for _, preset := range current {
		if preset.ID == legacy.ID {
			currentMaintainer = preset
			break
		}
	}
	got := state.Groups[group.ID]
	if got.PresetVersion != currentMaintainer.Version ||
		!got.Policy.AllowsOperation("file.write") ||
		!got.Policy.AllowsOperation("file.mkdir") {
		t.Fatalf("retired v3 preset was not refreshed: %+v", got)
	}
	for _, id := range []string{"file.realpath", "file.readlink", "file.statvfs", "file.perm", "file.link", "file.fs"} {
		if _, exists := got.Policy.Actions[id]; exists {
			t.Errorf("retired or widening operation %s survived the preset upgrade", id)
		}
	}
	if got.Revision != group.Revision+1 || state.AccessRevision != 13 {
		t.Fatal("preset upgrade did not advance policy revisions")
	}
}

func TestUpgradePresetGroupsTranslatesPreRebaseV3HostOperator(t *testing.T) {
	legacy := groupPresetsV3()["host-operator"]
	group := permissionGroup{
		ID: "preset-" + legacy.ID, Name: legacy.Name, Description: legacy.Description,
		Enabled: true, Revision: 7, PresetID: legacy.ID, PresetVersion: legacy.Version,
		Policy: clonePolicy(legacy.Policy),
	}
	state := managedState{
		Groups:         map[string]permissionGroup{group.ID: group},
		AccessRevision: 12,
	}
	if !upgradePresetGroups(&state) {
		t.Fatal("pre-rebase v3 host operator was not upgraded")
	}
	got := state.Groups[group.ID]
	for _, id := range []string{"file.perm", "file.link", "file.fs", "forward.open", "ext.get"} {
		if !got.Policy.AllowsOperation(id) {
			t.Errorf("equivalent translated operation %s was lost", id)
		}
		if !slices.Contains(got.PolicyWarnings, id) {
			t.Errorf("translated operation %s lacks a review warning", id)
		}
	}
	for _, retired := range []string{"file.chmod", "file.chown", "file.chtimes", "file.truncate", "file.symlink", "file.readlink", "ext.use", "ext.release"} {
		if _, exists := got.Policy.Actions[retired]; exists {
			t.Errorf("retired operation %s survived", retired)
		}
	}
}

func TestUpgradePresetGroupsPreservesDevV3ConsolidatedPolicy(t *testing.T) {
	for _, presetID := range []string{"file-maintainer", "host-operator"} {
		t.Run(presetID, func(t *testing.T) {
			legacy := groupPresetsV3Dev()[presetID]
			group := permissionGroup{
				ID: "preset-" + legacy.ID, Name: legacy.Name, Description: legacy.Description,
				Enabled: true, Revision: 7, PresetID: legacy.ID, PresetVersion: legacy.Version,
				Policy: clonePolicy(legacy.Policy),
			}
			state := managedState{
				Groups:         map[string]permissionGroup{group.ID: group},
				AccessRevision: 12,
			}

			if !upgradePresetGroups(&state) {
				t.Fatal("dev v3 preset was not upgraded")
			}
			got := state.Groups[group.ID]
			if got.PresetVersion != currentPresetVersion(presetID) || got.PolicyVocabulary != permissionPolicyVocabularyVersion {
				t.Fatalf("preset markers = version %d vocabulary %d", got.PresetVersion, got.PolicyVocabulary)
			}
			for operation := range legacy.Policy.Actions {
				if !got.Policy.AllowsOperation(operation) {
					t.Errorf("dev v3 operation %s was lost", operation)
				}
			}
			if !got.Policy.AllowsOperation("file.link") {
				t.Fatal("dev v3 file.link grant was lost")
			}
		})
	}
}

func TestPresetUpgradeDoesNotRecreateDeletedGroups(t *testing.T) {
	legacy := groupPresetsV3Dev()["viewer"]
	group := permissionGroup{
		ID: "preset-" + legacy.ID, Name: legacy.Name, Description: legacy.Description,
		Enabled: true, Revision: 7, PresetID: legacy.ID, PresetVersion: legacy.Version,
		Policy: clonePolicy(legacy.Policy),
	}
	state := managedState{
		Groups:         map[string]permissionGroup{group.ID: group},
		AccessRevision: 12,
	}
	if !upgradePresetGroups(&state) {
		t.Fatal("retained preset was not upgraded")
	}
	if _, exists := state.Groups["preset-file-maintainer"]; exists {
		t.Fatal("preset upgrade recreated a group the administrator deleted")
	}
}

func TestEditedRetiredPresetDropsBroadenedOperations(t *testing.T) {
	legacy := groupPresetsV3()["host-operator"]
	legacy.Policy.Actions["profile.create"] = authorization.Grant{Effect: authorization.EffectAllow}
	for _, operation := range []string{"cdp.open", "jrpc.open", "mcp.open"} {
		legacy.Policy.Actions[operation] = authorization.Grant{Effect: authorization.EffectAllow}
	}
	group := permissionGroup{
		ID: "preset-" + legacy.ID, Name: legacy.Name, Description: legacy.Description,
		Enabled: true, Revision: 7, PresetID: legacy.ID, PresetVersion: legacy.Version,
		Policy: legacy.Policy,
	}
	state := managedState{
		Groups:         map[string]permissionGroup{group.ID: group},
		AccessRevision: 12,
	}

	if upgradePresetGroups(&state) {
		t.Fatal("edited preset was replaced wholesale")
	}
	if !repairRetiredAgentOperations(&state) {
		t.Fatal("edited retired preset was not repaired")
	}
	got := state.Groups[group.ID]
	for _, operation := range []string{"cdp.open", "ext.get", "file.link", "file.write", "jrpc.open", "mcp.open", "profile.list"} {
		if _, exists := got.Policy.Actions[operation]; exists {
			t.Errorf("broadened legacy operation %s survived", operation)
		}
	}
	if !got.Policy.AllowsOperation("profile.create") {
		t.Fatal("administrator's current-vocabulary edit was removed")
	}
	if got.PresetVersion != currentPresetVersion(legacy.ID) {
		t.Fatalf("preset version = %d, want current", got.PresetVersion)
	}
	for _, operation := range []string{"cdp.open", "ext.get", "file.link", "file.write", "jrpc.open", "mcp.open", "profile.list"} {
		if !slices.Contains(got.PolicyWarnings, operation) {
			t.Errorf("missing review warning for %s", operation)
		}
	}
}

func TestCustomLegacyPolicyDropsWidenedOperationIDs(t *testing.T) {
	policy := EmptyAgentPolicy()
	for _, operation := range []string{"file.read", "file.write", "profile.list", "ext.get"} {
		policy.Actions[operation] = authorization.Grant{Effect: authorization.EffectAllow}
	}
	group := permissionGroup{
		ID: "custom-legacy", Name: "custom legacy", Description: "custom", Enabled: true,
		Revision: 3, CreatedAt: time.Unix(1, 0).UTC(), Policy: policy,
	}
	state := managedState{
		Groups:         map[string]permissionGroup{group.ID: group},
		AccessRevision: 12,
	}
	if !repairRetiredAgentOperations(&state) {
		t.Fatal("custom legacy policy was not repaired")
	}
	got := state.Groups[group.ID]
	if !got.Policy.AllowsOperation("file.read") {
		t.Fatal("unchanged operation was removed")
	}
	for _, operation := range []string{"file.write", "profile.list", "ext.get"} {
		if got.Policy.AllowsOperation(operation) {
			t.Errorf("widened operation %s survived", operation)
		}
		if !slices.Contains(got.PolicyWarnings, operation) {
			t.Errorf("missing warning for %s", operation)
		}
	}
	if got.PolicyVocabulary != permissionPolicyVocabularyVersion {
		t.Fatalf("policy vocabulary = %d, want current", got.PolicyVocabulary)
	}
}

func TestLegacyV3PresetLoadsAfterRebase(t *testing.T) {
	m, _, _ := configuredManaged(t)
	const groupID = "preset-file-maintainer"
	legacy := groupPresetsV3()["file-maintainer"]
	mutateManagedState(t, m, func(state map[string]any) {
		groups := state["groups"].(map[string]any)
		group := groups[groupID].(map[string]any)
		raw, err := json.Marshal(legacy.Policy)
		if err != nil {
			t.Fatal(err)
		}
		var policy map[string]any
		if err = json.Unmarshal(raw, &policy); err != nil {
			t.Fatal(err)
		}
		group["policy"] = policy
		group["preset_version"] = legacy.Version
	})

	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatalf("legacy v3 preset prevented managed startup: %v", err)
	}
	got := loaded.state.Groups[groupID]
	if got.PresetVersion != currentPresetVersion(legacy.ID) {
		t.Fatalf("preset version = %d, want current", got.PresetVersion)
	}
	if _, exists := got.Policy.Actions["file.realpath"]; exists {
		t.Fatal("retired v3 operation survived startup")
	}
	if !got.Policy.AllowsOperation("file.read") {
		t.Fatal("current file-maintainer grants were not loaded")
	}
}

func TestDevV3PresetLoadsAfterRebaseWithoutPermissionLoss(t *testing.T) {
	m, _, _ := configuredManaged(t)
	legacy := groupPresetsV3Dev()
	groupIDs := []string{"preset-file-maintainer", "preset-host-operator"}
	mutateManagedState(t, m, func(state map[string]any) {
		groups := state["groups"].(map[string]any)
		for _, groupID := range groupIDs {
			presetID := strings.TrimPrefix(groupID, "preset-")
			raw, err := json.Marshal(legacy[presetID].Policy)
			if err != nil {
				t.Fatal(err)
			}
			var policy map[string]any
			if err = json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
			group := groups[groupID].(map[string]any)
			group["policy"] = policy
			group["preset_version"] = legacy[presetID].Version
			group["policy_vocabulary"] = 0
		}
	})

	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatalf("dev v3 preset prevented managed startup: %v", err)
	}
	for _, groupID := range groupIDs {
		got := loaded.state.Groups[groupID]
		if got.PresetVersion != currentPresetVersion(got.PresetID) {
			t.Fatalf("%s preset version = %d, want current", groupID, got.PresetVersion)
		}
		if got.PolicyVocabulary != permissionPolicyVocabularyVersion {
			t.Fatalf("%s policy vocabulary = %d, want current", groupID, got.PolicyVocabulary)
		}
		if !got.Policy.AllowsOperation("file.link") {
			t.Fatalf("%s lost dev v3 file.link", groupID)
		}
	}
}
