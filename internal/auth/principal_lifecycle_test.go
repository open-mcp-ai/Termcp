package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentDeletePreviewCascadePersistenceAndRecreate(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	first := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"delete-me"}`)
	second := rotateTestCredential(t, m, cookie, session, first)

	stream := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18765/sse", nil)
	stream.Header.Set("Authorization", "Bearer "+second.Token)
	ctx, done, code := m.admitToken(stream)
	if code != 0 {
		t.Fatalf("admit token: %d", code)
	}
	defer done()
	m.SetMCPConnectionCounts(map[string]int{first.Agent.ID: 2})

	sink := &captureAuditSink{}
	m.SetAuditSink(sink)
	h := m.Wrap(http.NotFoundHandler())
	previewResponse := managedRequest(t, h, http.MethodPost, "/auth/principals/"+first.Agent.ID+"/delete-preview", "", cookie, session.CSRF)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("delete preview: %d %s", previewResponse.Code, previewResponse.Body)
	}
	var preview principalDeletePreview
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.PrincipalID != first.Agent.ID || preview.CredentialCount != 2 || preview.ActiveMCPConnections != 2 || preview.ActiveRequests != 1 || preview.ActiveWork != 0 {
		t.Fatalf("unexpected preview: %+v", preview)
	}

	listResponse := managedRequest(t, h, http.MethodGet, "/auth/principals", "", cookie, "")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list Agents: %d %s", listResponse.Code, listResponse.Body)
	}
	var listed struct {
		Principals []struct {
			ID                   string `json:"id"`
			ActiveMCPConnections int    `json:"active_mcp_connections"`
		} `json:"principals"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Principals) != 1 || listed.Principals[0].ID != first.Agent.ID || listed.Principals[0].ActiveMCPConnections != 2 {
		t.Fatalf("active MCP connection count missing from Agent list: %+v", listed.Principals)
	}

	stale := `{"revision":` + "99" + `,"plan_revision":` + jsonNumber(preview.PlanRevision) + `}`
	if w := managedRequest(t, h, http.MethodDelete, "/auth/principals/"+first.Agent.ID, stale, cookie, session.CSRF); w.Code != http.StatusConflict {
		t.Fatalf("stale Agent revision: %d %s", w.Code, w.Body)
	}
	stale = `{"revision":` + jsonNumber(preview.Revision) + `,"plan_revision":` + jsonNumber(preview.PlanRevision+1) + `}`
	if w := managedRequest(t, h, http.MethodDelete, "/auth/principals/"+first.Agent.ID, stale, cookie, session.CSRF); w.Code != http.StatusConflict {
		t.Fatalf("stale deletion inventory: %d %s", w.Code, w.Body)
	}

	body := `{"revision":` + jsonNumber(preview.Revision) + `,"plan_revision":` + jsonNumber(preview.PlanRevision) + `}`
	if w := managedRequest(t, h, http.MethodDelete, "/auth/principals/"+first.Agent.ID, body, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("delete Agent: %d %s", w.Code, w.Body)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Agent deletion did not cancel its live request")
	}
	for _, token := range []string{first.Token, second.Token} {
		if w := bearerRequest(h, http.MethodGet, "/auth/mcp", token, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("deleted Agent token accepted: %d", w.Code)
		}
	}
	for _, path := range []string{"/auth/principals", "/auth/tokens"} {
		w := managedRequest(t, h, http.MethodGet, path, "", cookie, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), first.Agent.ID) {
			t.Fatalf("deleted Agent remains in %s: %d %s", path, w.Code, w.Body)
		}
	}

	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	cookie2, session2, err := reopened.loginWithOptions("admin", testPassword, testPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	h2 := reopened.Wrap(http.NotFoundHandler())
	if w := bearerRequest(h2, http.MethodGet, "/auth/mcp", first.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("restart restored deleted Agent token: %d", w.Code)
	}
	if w := managedRequest(t, h2, http.MethodPatch, "/auth/principals/"+first.Agent.ID, `{"revision":1,"enabled":false}`, cookie2, session2.CSRF); w.Code != http.StatusNotFound {
		t.Fatalf("restart restored deleted Agent: %d %s", w.Code, w.Body)
	}

	recreated := issueTestCredential(t, reopened, cookie2, session2, "/auth/principals", `{"name":"delete-me"}`)
	if recreated.Agent.ID == first.Agent.ID || recreated.Agent.Name != first.Agent.Name {
		t.Fatalf("recreated Agent did not receive a new stable identity: old=%s new=%s", first.Agent.ID, recreated.Agent.ID)
	}

	events := sink.snapshot()
	actions := make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action)
	}
	for _, want := range []string{"principal.delete_preview", "principal.delete"} {
		if !slicesContains(actions, want) {
			t.Fatalf("missing audit action %q: %v", want, actions)
		}
	}
}

func TestAgentGroupBindingRejectsDisabledGroup(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	group := createTestGroup(t, m, cookie, session, "temporarily disabled", "viewer")
	agent := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"worker"}`)
	h := m.Wrap(http.NotFoundHandler())
	body := `{"revision":` + jsonNumber(group.Revision) + `,"enabled":false}`
	if w := managedRequest(t, h, http.MethodPatch, "/auth/groups/"+group.ID, body, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("disable group: %d %s", w.Code, w.Body)
	}
	body = `{"revision":` + jsonNumber(agent.Agent.Revision) + `,"group_id":"` + group.ID + `"}`
	if w := managedRequest(t, h, http.MethodPut, "/auth/principals/"+agent.Agent.ID+"/group", body, cookie, session.CSRF); w.Code != http.StatusBadRequest {
		t.Fatalf("bound Agent to disabled group: %d %s", w.Code, w.Body)
	}
}

func jsonNumber(value uint64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
