package auth

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestManagedAuthManagementRoutesRejectWrongCredentialShape(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	agent := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"route-contract-agent"}`)
	h := m.Wrap(http.NotFoundHandler())

	routes := []struct {
		method string
		path   string
	}{
		{"POST", "/auth/password"},
		{"POST", "/auth/unlock"},
		{"POST", "/auth/logout"},
		{"GET", "/auth"},
		{"GET", "/permissions"},
		{"GET", "/auth/access"},
		{"POST", "/auth/access/preview"},
		{"GET", "/auth/permissions"},
		{"PUT", "/auth/permissions"},
		{"GET", "/auth/groups"},
		{"POST", "/auth/groups"},
		{"PATCH", "/auth/groups/missing"},
		{"DELETE", "/auth/groups/missing"},
		{"POST", "/auth/groups/missing/clone"},
		{"POST", "/auth/groups/missing/delete-preview"},
		{"GET", "/auth/group-presets"},
		{"GET", "/auth/default-group"},
		{"PUT", "/auth/default-group"},
		{"GET", "/auth/principals"},
		{"POST", "/auth/principals"},
		{"PATCH", "/auth/principals/missing"},
		{"POST", "/auth/principals/missing/delete-preview"},
		{"DELETE", "/auth/principals/missing"},
		{"PUT", "/auth/principals/missing/group"},
		{"GET", "/auth/tokens"},
		{"POST", "/auth/tokens"},
		{"POST", "/auth/tokens/missing/rotate"},
		{"DELETE", "/auth/tokens/history"},
		{"DELETE", "/auth/tokens/missing"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			anonymous := managedRequest(t, h, route.method, route.path, "", "", "")
			wantAnonymous := http.StatusUnauthorized
			if route.method == http.MethodGet && (route.path == "/auth" || route.path == "/permissions") {
				wantAnonymous = http.StatusSeeOther
			}
			if anonymous.Code != wantAnonymous {
				t.Fatalf("anonymous status = %d, want %d: %s", anonymous.Code, wantAnonymous, anonymous.Body.String())
			}

			bearer := bearerRequest(h, route.method, route.path, agent.Token, "")
			if bearer.Code != http.StatusForbidden {
				t.Fatalf("MCP bearer reached management route: %d %s", bearer.Code, bearer.Body.String())
			}

			if unsafeMethod(route.method) {
				withoutCSRF := managedRequest(t, h, route.method, route.path, "{}", cookie, "")
				if withoutCSRF.Code != http.StatusForbidden {
					t.Fatalf("write without CSRF = %d, want 403: %s", withoutCSRF.Code, withoutCSRF.Body.String())
				}
			}
		})
	}
}

func TestManagedAuthReadAndWriteRoutesReachHandlersOnlyAfterAdmission(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{
		"/auth/status",
		"/auth/mcp",
		"/auth/access",
		"/auth/group-presets",
		"/auth/groups",
		"/auth/default-group",
		"/auth/principals",
		"/auth/tokens",
		"/auth/permissions",
		"/auth",
		"/permissions",
	} {
		t.Run("GET "+path, func(t *testing.T) {
			w := managedRequest(t, h, http.MethodGet, path, "", cookie, "")
			if w.Code != http.StatusOK {
				t.Fatalf("authenticated read = %d: %s", w.Code, w.Body.String())
			}
		})
	}

	writes := []struct {
		method string
		path   string
		body   string
	}{
		{"POST", "/auth/groups", `{}`},
		{"PATCH", "/auth/groups/missing", `{}`},
		{"DELETE", "/auth/groups/missing", `{}`},
		{"POST", "/auth/groups/missing/clone", `{}`},
		{"POST", "/auth/groups/missing/delete-preview", ""},
		{"PUT", "/auth/default-group", `{}`},
		{"PUT", "/auth/principals/missing/group", `{}`},
		{"PATCH", "/auth/principals/missing", `{}`},
		{"POST", "/auth/principals/missing/delete-preview", ``},
		{"DELETE", "/auth/principals/missing", `{}`},
		{"POST", "/auth/tokens", `{}`},
		{"POST", "/auth/tokens/missing/rotate", `{}`},
		{"DELETE", "/auth/tokens/history", ""},
		{"DELETE", "/auth/tokens/missing", ""},
		{"PUT", "/auth/permissions", `{}`},
		{"POST", "/auth/access/preview", `{}`},
		{"POST", "/auth/password", `{}`},
		{"POST", "/auth/unlock", `{}`},
	}
	for _, route := range writes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			w := managedRequest(t, h, route.method, route.path, route.body, cookie, session.CSRF)
			if w.Code == http.StatusForbidden || w.Code >= http.StatusInternalServerError {
				t.Fatalf("authenticated write did not reach its handler cleanly: %d %s", w.Code, w.Body.String())
			}
		})
	}

	if w := managedRequest(t, h, http.MethodPost, "/auth/logout", `{}`, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", w.Code, w.Body.String())
	}
	if w := managedRequest(t, h, http.MethodGet, "/auth/access", "", cookie, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session status = %d, want 401", w.Code)
	}
}

func TestManagedMCPEntranceSwitchesAreLiveAndReversible(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	admin := issueTestCredential(t, m, cookie, session, "/auth/tokens", `{"label":"admin route contract"}`)
	agent := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"agent route contract"}`)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	type roleState struct {
		Revision    uint64     `json:"revision"`
		Permissions RolePolicy `json:"permissions"`
	}
	currentRoles := func(t *testing.T) roleState {
		t.Helper()
		w := managedRequest(t, h, http.MethodGet, "/auth/permissions", "", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("read roles = %d: %s", w.Code, w.Body.String())
		}
		var state roleState
		if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	setRoles := func(t *testing.T, adminMCP, agentMCP bool) roleState {
		t.Helper()
		state := currentRoles(t)
		adminSurfaces := []string{"web", "api", "daemon"}
		if adminMCP {
			adminSurfaces = append(adminSurfaces, "mcp")
		}
		var agentSurfaces []string
		if agentMCP {
			agentSurfaces = []string{"mcp"}
		}
		body, err := json.Marshal(map[string]any{
			"revision": state.Revision,
			"permissions": RolePolicy{
				RoleAdmin: adminSurfaces,
				RoleAgent: agentSurfaces,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		w := managedRequest(t, h, http.MethodPut, "/auth/permissions", string(body), cookie, session.CSRF)
		if w.Code != http.StatusOK {
			t.Fatalf("write roles = %d: %s", w.Code, w.Body.String())
		}
		var next roleState
		if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
			t.Fatal(err)
		}
		return next
	}
	probe := func(t *testing.T, token string) int {
		t.Helper()
		return bearerRequest(h, http.MethodGet, "/auth/mcp", token, "").Code
	}
	probeCookie := func(t *testing.T) int {
		t.Helper()
		return managedRequest(t, h, http.MethodGet, "/auth/mcp", "", cookie, "").Code
	}

	if got := probe(t, admin.Token); got != http.StatusOK {
		t.Fatalf("initial Admin MCP probe = %d", got)
	}
	if got := probe(t, agent.Token); got != http.StatusOK {
		t.Fatalf("initial Agent MCP probe = %d", got)
	}

	setRoles(t, false, true)
	if got := probe(t, admin.Token); got != http.StatusForbidden {
		t.Fatalf("disabled Admin MCP probe = %d, want 403", got)
	}
	if got := probeCookie(t); got != http.StatusForbidden {
		t.Fatalf("disabled Admin Cookie MCP probe = %d, want 403", got)
	}
	if got := probe(t, agent.Token); got != http.StatusOK {
		t.Fatalf("Agent MCP changed with Admin switch: %d", got)
	}
	if w := managedRequest(t, h, http.MethodGet, "/api/version", "", cookie, ""); w.Code != http.StatusNoContent {
		t.Fatalf("Admin API access changed with MCP switch: %d", w.Code)
	}

	setRoles(t, false, false)
	if got := probe(t, agent.Token); got != http.StatusForbidden {
		t.Fatalf("disabled Agent MCP probe = %d, want 403", got)
	}

	setRoles(t, true, false)
	if got := probe(t, admin.Token); got != http.StatusOK {
		t.Fatalf("re-enabled Admin MCP probe = %d", got)
	}
	if got := probe(t, agent.Token); got != http.StatusForbidden {
		t.Fatalf("Agent MCP switch leaked after Admin re-enable: %d", got)
	}

	setRoles(t, true, true)
	if got := probe(t, admin.Token); got != http.StatusOK {
		t.Fatalf("re-enabled Admin MCP probe = %d", got)
	}
	if got := probe(t, agent.Token); got != http.StatusOK {
		t.Fatalf("re-enabled Agent MCP probe = %d", got)
	}

	state := currentRoles(t)
	body, err := json.Marshal(map[string]any{
		"revision": state.Revision,
		"permissions": RolePolicy{
			RoleAdmin: []string{"web"},
			RoleAgent: []string{"mcp"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := managedRequest(t, h, http.MethodPut, "/auth/permissions", string(body), cookie, session.CSRF); w.Code != http.StatusBadRequest {
		t.Fatalf("policy without mandatory Admin API access = %d, want 400", w.Code)
	}
	if got := probe(t, admin.Token); got != http.StatusOK {
		t.Fatalf("rejected policy changed Admin MCP access: %d", got)
	}
	if got := probe(t, agent.Token); got != http.StatusOK {
		t.Fatalf("rejected policy changed Agent MCP access: %d", got)
	}
}
