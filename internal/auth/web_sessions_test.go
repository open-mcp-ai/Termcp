package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestWebSessionsExpireOnRestartAndMCPCredentialsRemain(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	credential := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"persistent-client"}`)
	raw, err := m.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var disk managedState
	if err = json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.Sessions) != 0 {
		t.Error("browser sessions were persisted")
	}
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NeedsUnlock() {
		t.Fatal("automatic vault unlock failed")
	}
	if _, _, err = loaded.verify(cookie); err == nil {
		t.Fatal("restart restored a browser login")
	}
	fresh, _, err := loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = loaded.verify(cookie); err == nil {
		t.Fatal("sign-in resurrected an old browser cookie")
	}
	if _, _, err = loaded.login("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	if _, _, err = loaded.verify(fresh); err != nil {
		t.Fatal("a second sign-in removed a session from this run", err)
	}
	h := loaded.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFromContext(r.Context())
		if !ok || actor.Role != RoleAgent {
			t.Error("MCP identity was lost")
		}
		w.WriteHeader(204)
	}))
	if w := bearerRequest(h, "POST", "/stream", credential.Token, ""); w.Code != 204 {
		t.Fatalf("MCP credential lost on web restart: %d", w.Code)
	}
}

func TestManualUnlockDoesNotResurrectWebSessions(t *testing.T) {
	m := openManaged(t)
	if err := m.setup("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	old, _, err := m.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.NeedsUnlock() {
		t.Fatal("password-only vault should restart locked")
	}
	if _, _, err = loaded.verify(old); err == nil {
		t.Fatal("locked vault accepted an old cookie")
	}
	fresh, _, err := loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = loaded.verify(old); err == nil {
		t.Fatal("manual unlock resurrected an old cookie")
	}
	if _, _, err = loaded.verify(fresh); err != nil {
		t.Fatal("fresh login was refused", err)
	}
}

func TestWebSessionClaimsAreBoundToCurrentRun(t *testing.T) {
	m, cookie, _ := configuredManaged(t)
	var claims jwt.MapClaims
	if _, _, err := jwt.NewParser().ParseUnverified(cookie, &claims); err != nil {
		t.Fatal(err)
	}
	key, err := base64.RawURLEncoding.DecodeString(m.webSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	for _, boot := range []string{"", "another-service-run"} {
		claims["web_boot_id"] = boot
		forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = m.verify(forged); err == nil {
			t.Fatal("current session accepted a missing or foreign boot ID")
		}
	}
}
