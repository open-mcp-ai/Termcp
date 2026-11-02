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
			a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"worker","group_id":"`+group.ID+`"}`)
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
				path = "/auth/principals/" + a.Agent.ID
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
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"worker","group_id":"`+group.ID+`"}`)
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

func TestDetachedMessageCanRegisterWorkAfterHTTPResponse(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"detached-worker"}`)
	req := httptest.NewRequest("POST", "http://127.0.0.1/message", nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	ctx, finish, status := m.admitToken(req)
	if status != 0 {
		t.Fatal(status)
	}
	// The SSE library sends 202 and handles the message on a detached context.
	message, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	finish()
	release, err := RegisterBackgroundWork(message, func() {})
	if err != nil {
		t.Fatal("HTTP response completion revoked the live message", err)
	}
	release()
	stop()
	if release, err = RegisterBackgroundWork(message, func() {}); err == nil {
		release()
		t.Fatal("cancelled message registered new work")
	}
}

func TestDetachedMessageCannotReuseAuthorityAfterPermissionRestore(t *testing.T) {
	for _, change := range []string{"principal", "role"} {
		t.Run(change, func(t *testing.T) {
			m, cookie, session := configuredManaged(t)
			a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"stale-detached-worker"}`)
			req := httptest.NewRequest("POST", "http://127.0.0.1/message", nil)
			req.Header.Set("Authorization", "Bearer "+a.Token)
			ctx, finish, status := m.admitToken(req)
			if status != 0 {
				t.Fatal(status)
			}
			message := context.WithoutCancel(ctx)
			finish()
			h := m.Wrap(http.NotFoundHandler())
			for i, enabled := range []bool{false, true} {
				path, method := "/auth/principals/"+a.Agent.ID, "PATCH"
				input := map[string]any{"revision": i + 1, "enabled": enabled}
				if change == "role" {
					path, method = "/auth/permissions", "PUT"
					policy := defaultPolicy()
					if !enabled {
						policy[RoleAgent] = []string{}
					}
					input = map[string]any{"revision": i + 1, "permissions": policy}
				}
				body, _ := json.Marshal(input)
				if w := managedRequest(t, h, method, path, string(body), cookie, session.CSRF); w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			}
			if release, err := RegisterBackgroundWork(message, func() {}); err == nil {
				release()
				t.Fatal("stale message reused authority after a disable/enable cycle")
			}
			ctx, finish, status = m.admitToken(req)
			if status != 0 {
				t.Fatal("restored permission did not admit a fresh request", status)
			}
			defer finish()
			if release, err := RegisterBackgroundWork(ctx, func() {}); err != nil {
				t.Fatal("fresh request retained stale authority", err)
			} else {
				release()
			}
		})
	}
}
