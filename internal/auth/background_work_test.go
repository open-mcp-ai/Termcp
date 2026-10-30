package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBackgroundWorkSurvivesResponseAndStopsOnAccessChanges(t *testing.T) {
	for _, change := range []string{"group-policy", "group-disable", "token-revoke", "principal-disable", "group-delete", "group-move"} {
		t.Run(change, func(t *testing.T) {
			m, cookie, session := configuredManaged(t)
			group := createTestGroup(t, m, cookie, session, "background owners", "viewer")
			a := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"worker","group_id":"`+group.ID+`"}`)
			req := httptest.NewRequest("POST", "http://127.0.0.1/stream", nil)
			req.Header.Set("Authorization", "Bearer "+a.Token)
			ctx, finish, status := m.admitToken(req)
			if status != 0 {
				t.Fatal(status)
			}
			work, cancel := context.WithCancel(context.Background())
			defer cancel()
			release, err := RegisterBackgroundWork(ctx, cancel)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			finish()
			if work.Err() != nil {
				t.Fatal("ordinary response completion cancelled background work")
			}
			path, body, method := "", "", "PATCH"
			switch change {
			case "group-policy":
				path = "/auth/groups/" + group.ID
				body = `{"revision":1,"policy":{"schema_version":2,"host_mode":"all","actions":{}}}`
			case "group-disable":
				path = "/auth/groups/" + group.ID
				body = `{"revision":1,"enabled":false}`
			case "token-revoke":
				path = "/auth/tokens/" + a.Credential.ID
				method = "DELETE"
			case "group-delete":
				preview := managedRequest(t, m.Wrap(http.NotFoundHandler()), "POST", "/auth/groups/"+group.ID+"/delete-preview", "", cookie, session.CSRF)
				var plan struct {
					Revision     uint64 `json:"revision"`
					PlanRevision uint64 `json:"plan_revision"`
				}
				if err := json.Unmarshal(preview.Body.Bytes(), &plan); err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(map[string]any{"revision": plan.Revision, "plan_revision": plan.PlanRevision, "resolution": "deny"})
				path = "/auth/groups/" + group.ID
				method = "DELETE"
				body = string(payload)
			case "group-move":
				path = "/auth/principals/" + a.Agent.ID + "/group"
				method = "PUT"
				body = `{"revision":1,"group_id":"preset-no-access"}`
			case "principal-disable":
				path = "/auth/agents/" + a.Agent.ID
				body = `{"revision":1,"enabled":false}`
			}
			if w := managedRequest(t, m.Wrap(http.NotFoundHandler()), method, path, body, cookie, session.CSRF); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if work.Err() == nil {
				t.Fatal("access change left detached work running")
			}
		})
	}
}

func TestBackgroundRegistrationRefusesAlreadyRevokedIntent(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	group := createTestGroup(t, m, cookie, session, "revoked owners", "viewer")
	a := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"worker","group_id":"`+group.ID+`"}`)
	req := httptest.NewRequest("POST", "http://127.0.0.1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	ctx, finish, status := m.admitToken(req)
	if status != 0 {
		t.Fatal(status)
	}
	defer finish()
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "PATCH", "/auth/groups/"+group.ID, `{"revision":1,"enabled":false}`, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if release, err := RegisterBackgroundWork(ctx, func() {}); err == nil {
		release()
		t.Fatal("revoked request registered new background authority")
	}
}
