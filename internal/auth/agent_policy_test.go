package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestAgentPolicySelectionRevisionAndPersistence(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	w := managedRequest(t, h, "POST", "/auth/agents", `{"name":"restricted","issue_token":false,"policy":{"functions":[],"host_mode":"none","hosts":[]}}`, cookie, session.CSRF)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"token"`) {
		t.Fatalf("create without credential: %d %s", w.Code, w.Body)
	}
	var created struct {
		Agent agentRecord `json:"agent"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Agent.ID
	store := m.vault.(*sshconfig.Store)
	if err := store.Save("chosen-host", []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"test\"\npassword = \"synthetic-password\"\n")); err != nil {
		t.Fatal(err)
	}
	hostID, err := store.Identity("chosen-host")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"revision":1,"policy":{"functions":["file_read","profile_read"],"host_mode":"selected","hosts":["` + hostID + `"]}}`
	if w = managedRequest(t, h, "PATCH", "/auth/agents/"+id, body, cookie, session.CSRF); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if w = managedRequest(t, h, "PATCH", "/auth/agents/"+id, body, cookie, session.CSRF); w.Code != 409 {
		t.Fatal("stale policy overwrote newer grant")
	}
	bad := `{"revision":2,"policy":{"functions":["profile_write"],"host_mode":"selected","hosts":[]}}`
	if w = managedRequest(t, h, "PATCH", "/auth/agents/"+id, bad, cookie, session.CSRF); w.Code != 400 {
		t.Fatal("global configuration granted through narrow host scope")
	}
	c := issueTestCredential(t, m, cookie, session, "/auth/tokens", `{"agent_id":"`+id+`","label":"client"}`)
	if err = store.Rename("chosen-host", "renamed-host"); err != nil {
		t.Fatal(err)
	}
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	check := loaded.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, restricted, e := PolicyFromContext(r.Context())
		if e != nil || !restricted || !p.Allows("file_read") || p.Allows("file_write") || !p.AllowsHost(hostID) {
			t.Error("lost selected policy", p, e)
		}
		w.WriteHeader(204)
	}))
	if w = bearerRequest(check, "POST", "/stream", c.Token, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	cookie, _, err = loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	catalog := managedRequest(t, loaded.Wrap(http.NotFoundHandler()), "GET", "/auth/access", "", cookie, "")
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), "renamed-host") || strings.Contains(catalog.Body.String(), "synthetic-password") {
		t.Fatal("host catalog missing or leaked credentials")
	}
}

func TestAgentPolicyResolverChecksLiveGrant(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	issued := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"agent","policy":{"functions":["terminal_write"],"host_mode":"all","hosts":[]}}`)
	r := httptest.NewRequest("GET", "http://127.0.0.1/sse", nil)
	r.Header.Set("Authorization", "Bearer "+issued.Token)
	ctx, done, status := m.admitToken(r)
	if status != 0 {
		t.Fatal(status)
	}
	defer done()
	if p, ok, err := PolicyFromContext(ctx); err != nil || !ok || !p.Allows("terminal_write") {
		t.Fatal("legacy policy was not preserved")
	}
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "PATCH", "/auth/agents/"+issued.Agent.ID, `{"revision":1,"policy":{"functions":[],"host_mode":"none","hosts":[]}}`, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if _, _, err := PolicyFromContext(ctx); err == nil {
		t.Fatal("in-flight context retained removed grant")
	}
}
