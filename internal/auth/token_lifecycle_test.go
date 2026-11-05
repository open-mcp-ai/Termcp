package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMCPTokenLifecycleControlsPersistenceAndRotation(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	group := createTestGroup(t, m, cookie, session, "token lifecycle", "viewer")
	first := issueTestCredential(t, m, cookie, session, "/auth/principals",
		fmt.Sprintf(`{"name":"token-lifecycle-agent","group_id":%q}`, group.ID))
	second := issueTestCredential(t, m, cookie, session, "/auth/tokens",
		fmt.Sprintf(`{"principal_id":%q,"label":"second token"}`, first.Agent.ID))
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	probe := func(token string) int {
		return bearerRequest(h, http.MethodGet, "/auth/mcp", token, "").Code
	}
	requireCode := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}

	requireCode("first token", probe(first.Token), http.StatusOK)
	requireCode("second token", probe(second.Token), http.StatusOK)

	listing := managedRequest(t, h, http.MethodGet, "/auth/tokens", "", cookie, "")
	if listing.Code != http.StatusOK {
		t.Fatalf("list tokens = %d: %s", listing.Code, listing.Body.String())
	}
	for _, secret := range []string{first.Token, second.Token, "digest"} {
		if strings.Contains(listing.Body.String(), secret) {
			t.Fatalf("token listing leaked %q", secret)
		}
	}

	disablePrincipal := fmt.Sprintf(`{"revision":1,"enabled":false}`)
	if w := managedRequest(t, h, http.MethodPatch, "/auth/principals/"+first.Agent.ID, disablePrincipal, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("disable principal = %d: %s", w.Code, w.Body.String())
	}
	requireCode("disabled principal token", probe(first.Token), http.StatusUnauthorized)
	requireCode("disabled principal sibling", probe(second.Token), http.StatusUnauthorized)
	enablePrincipal := `{"revision":2,"enabled":true}`
	if w := managedRequest(t, h, http.MethodPatch, "/auth/principals/"+first.Agent.ID, enablePrincipal, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("enable principal = %d: %s", w.Code, w.Body.String())
	}
	requireCode("re-enabled principal token", probe(first.Token), http.StatusOK)
	requireCode("re-enabled principal sibling", probe(second.Token), http.StatusOK)

	disableGroup := fmt.Sprintf(`{"revision":%d,"enabled":false}`, group.Revision)
	if w := managedRequest(t, h, http.MethodPatch, "/auth/groups/"+group.ID, disableGroup, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("disable group = %d: %s", w.Code, w.Body.String())
	}
	requireCode("disabled group token", probe(first.Token), http.StatusUnauthorized)
	requireCode("disabled group sibling", probe(second.Token), http.StatusUnauthorized)
	enableGroup := fmt.Sprintf(`{"revision":%d,"enabled":true}`, group.Revision+1)
	if w := managedRequest(t, h, http.MethodPatch, "/auth/groups/"+group.ID, enableGroup, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("enable group = %d: %s", w.Code, w.Body.String())
	}
	requireCode("re-enabled group token", probe(first.Token), http.StatusOK)
	requireCode("re-enabled group sibling", probe(second.Token), http.StatusOK)

	var roles struct {
		Revision    uint64     `json:"revision"`
		Permissions RolePolicy `json:"permissions"`
	}
	roleResponse := managedRequest(t, h, http.MethodGet, "/auth/permissions", "", cookie, "")
	if roleResponse.Code != http.StatusOK {
		t.Fatalf("read roles = %d: %s", roleResponse.Code, roleResponse.Body.String())
	}
	if err := json.Unmarshal(roleResponse.Body.Bytes(), &roles); err != nil {
		t.Fatal(err)
	}
	roles.Permissions[RoleAgent] = nil
	body, _ := json.Marshal(map[string]any{"revision": roles.Revision, "permissions": roles.Permissions})
	if w := managedRequest(t, h, http.MethodPut, "/auth/permissions", string(body), cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("disable Agent MCP = %d: %s", w.Code, w.Body.String())
	}
	requireCode("disabled Agent MCP token", probe(first.Token), http.StatusForbidden)
	roles.Permissions[RoleAgent] = []string{"mcp"}
	body, _ = json.Marshal(map[string]any{"revision": roles.Revision + 1, "permissions": roles.Permissions})
	if w := managedRequest(t, h, http.MethodPut, "/auth/permissions", string(body), cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("enable Agent MCP = %d: %s", w.Code, w.Body.String())
	}
	requireCode("re-enabled Agent MCP token", probe(first.Token), http.StatusOK)

	if w := managedRequest(t, h, http.MethodDelete, "/auth/tokens/"+first.Credential.ID, "", cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("revoke first token = %d: %s", w.Code, w.Body.String())
	}
	requireCode("revoked token", probe(first.Token), http.StatusUnauthorized)
	requireCode("sibling after revoke", probe(second.Token), http.StatusOK)
	if w := managedRequest(t, h, http.MethodDelete, "/auth/tokens/"+first.Credential.ID, "", cookie, session.CSRF); w.Code != http.StatusNotFound {
		t.Fatalf("repeat revoke = %d, want 404: %s", w.Code, w.Body.String())
	}

	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	cookie, session, err = reopened.loginWithPassword(testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	h = reopened.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	requireCode("revoked token after restart", probe(first.Token), http.StatusUnauthorized)
	requireCode("sibling after restart", probe(second.Token), http.StatusOK)

	originalNow := reopened.now
	reopened.now = func() time.Time { return originalNow().Add(10 * 365 * 24 * time.Hour) }
	requireCode("token after simulated ten years", probe(second.Token), http.StatusOK)
	reopened.now = originalNow

	current := second
	for round := 1; round <= 10; round++ {
		next := issueTestCredential(t, reopened, cookie, session, "/auth/tokens",
			fmt.Sprintf(`{"principal_id":%q,"label":"rotation-%d"}`, second.Agent.ID, round))
		requireCode(fmt.Sprintf("rotation %d old token", round), probe(current.Token), http.StatusOK)
		requireCode(fmt.Sprintf("rotation %d new token", round), probe(next.Token), http.StatusOK)
		if w := managedRequest(t, h, http.MethodDelete, "/auth/tokens/"+current.Credential.ID, "", cookie, session.CSRF); w.Code != http.StatusOK {
			t.Fatalf("rotation %d revoke = %d: %s", round, w.Code, w.Body.String())
		}
		requireCode(fmt.Sprintf("rotation %d revoked token", round), probe(current.Token), http.StatusUnauthorized)
		requireCode(fmt.Sprintf("rotation %d surviving token", round), probe(next.Token), http.StatusOK)
		current = next
	}

	const workers = 32
	var wg sync.WaitGroup
	codes := make(chan int, workers)
	for range workers {
		wg.Go(func() {
			codes <- probe(current.Token)
		})
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		requireCode("concurrent token request", code, http.StatusOK)
	}
}
