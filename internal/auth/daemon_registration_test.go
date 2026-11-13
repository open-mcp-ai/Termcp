package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalDaemonRegistrationLifecycleAndCredentialHandoff(t *testing.T) {
	m, _, _ := configuredManaged(t)
	dataDir := filepath.Dir(m.path)
	if ok, err := m.EnsureLocalDaemonRegistration(t.Context()); err != nil || !ok {
		t.Fatalf("initial daemon registration: ok=%v err=%v", ok, err)
	}

	m.mu.Lock()
	registration := *m.state.DaemonRegistration
	agent := m.state.Agents[registration.AgentID]
	token := m.state.Tokens[registration.TokenID]
	defaultGroup := m.state.DefaultGroupID
	m.mu.Unlock()
	if registration.RevokedAt != nil || agent.Origin != agentOriginDaemon ||
		agent.GroupID != defaultGroup || token.Kind != tokenKindDaemon {
		t.Fatalf("daemon registration state: registration=%+v agent=%+v token=%+v default=%q", registration, agent, token, defaultGroup)
	}
	m.mu.Lock()
	view := m.principalViewLocked(m.state, agent)
	m.mu.Unlock()
	if view.Origin != agentOriginDaemon || view.DaemonRegistrationID != registration.ID ||
		view.DaemonTokenID != registration.TokenID || view.DaemonTokenStatus != "active" ||
		view.StaticCredentialCount != 0 || view.ActiveStaticCredentialCount != 0 {
		t.Fatalf("daemon principal view conflates origin and static credentials: %+v", view)
	}

	firstCredential, err := ReadLocalDaemonCredential(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if firstCredential == "" {
		t.Fatal("daemon credential file is empty")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(LocalDaemonCredentialPath(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("daemon credential mode = %o, want 600", got)
		}
	}
	encrypted, err := m.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(firstCredential)) {
		t.Fatal("encrypted auth state contains the raw daemon credential")
	}
	if w := bearerRequest(m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFromContext(r.Context())
		if !ok || actor.ID != registration.AgentID {
			t.Errorf("daemon credential actor = %+v, want %q", actor, registration.AgentID)
		}
		w.WriteHeader(http.StatusNoContent)
	})), http.MethodPost, "/stream", firstCredential, ""); w.Code != http.StatusNoContent {
		t.Fatalf("daemon credential did not authenticate: %d %s", w.Code, w.Body)
	}

	restarted, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := restarted.EnsureLocalDaemonRegistration(t.Context()); err != nil || !ok {
		t.Fatalf("restart registration: ok=%v err=%v", ok, err)
	}
	restartedCredential, err := ReadLocalDaemonCredential(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if restartedCredential != firstCredential {
		t.Fatal("restart unnecessarily rotated the daemon credential")
	}
	restarted.mu.Lock()
	if got := len(restarted.state.Agents); got != 1 {
		t.Fatalf("restart created %d Agents, want 1", got)
	}
	restarted.mu.Unlock()

	// A corrupt handoff file is repaired with a newly minted internal credential.
	// The previous credential is revoked and its live authority is invalidated
	// only after the encrypted registration commit succeeds.
	var invalidatedCredential string
	restarted.SetMCPConnectionInvalidator(func(_, credentialID string) {
		invalidatedCredential = credentialID
	})
	if err := os.WriteFile(LocalDaemonCredentialPath(dataDir), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := restarted.EnsureLocalDaemonRegistration(t.Context()); err != nil || !ok {
		t.Fatalf("repair registration: ok=%v err=%v", ok, err)
	}
	repairedCredential, err := ReadLocalDaemonCredential(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if repairedCredential == firstCredential {
		t.Fatal("corrupt handoff file reused a credential the bridge can no longer know")
	}
	if invalidatedCredential != token.ID {
		t.Fatalf("replaced daemon credential invalidated %q, want %q", invalidatedCredential, token.ID)
	}
	if w := bearerRequest(restarted.Wrap(http.NotFoundHandler()), http.MethodPost, "/stream", firstCredential, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("replaced daemon credential still authenticated: %d", w.Code)
	}
	if w := bearerRequest(restarted.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), http.MethodPost, "/stream", repairedCredential, ""); w.Code != http.StatusNoContent {
		t.Fatalf("replacement daemon credential did not authenticate: %d %s", w.Code, w.Body)
	}

	// Explicit Admin revocation is a tombstone. Restart and registration repair
	// must not silently create a replacement identity or credential.
	adminCookie, adminSession, err := restarted.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	repairedTokenID := restarted.state.DaemonRegistration.TokenID
	restarted.mu.Unlock()
	if w := managedRequest(t, restarted.Wrap(http.NotFoundHandler()), http.MethodDelete, "/auth/tokens/"+repairedTokenID, "", adminCookie, adminSession.CSRF); w.Code != http.StatusOK {
		t.Fatalf("revoke daemon credential: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(LocalDaemonCredentialPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("revoked daemon credential file still exists after commit: %v", err)
	}
	if ok, err := restarted.EnsureLocalDaemonRegistration(t.Context()); err != nil || !ok {
		t.Fatalf("registration after revoke: ok=%v err=%v", ok, err)
	}
	restarted.mu.Lock()
	defer restarted.mu.Unlock()
	if restarted.state.DaemonRegistration.RevokedAt == nil || restarted.state.DaemonRegistration.RevokeReason != "credential_revoked" {
		t.Fatalf("revocation tombstone = %+v", restarted.state.DaemonRegistration)
	}
	if len(restarted.state.Agents) != 1 {
		t.Fatalf("revocation created another daemon Agent: %d", len(restarted.state.Agents))
	}
	if _, ok := restarted.state.Agents[agent.ID]; !ok {
		t.Fatal("revocation removed the existing daemon Agent instead of retaining its history")
	}
}

func TestDaemonRegistrationDoesNotOverrideExplicitStaticCredential(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	explicit := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"Manual bridge"}`)
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFromContext(r.Context())
		if !ok || actor.ID != explicit.Agent.ID {
			t.Errorf("explicit credential actor = %+v, want %+v", actor, explicit.Agent)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18765/stream", nil)
	req.Header.Set("Authorization", "Bearer "+explicit.Token)
	req.Header.Set("X-Termcp-Client-Mode", "stdio-bridge")
	req.Header.Set("X-Termcp-Namespace", "forged-daemon-namespace")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("explicit static bridge credential failed: %d %s", recorder.Code, recorder.Body)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.state.Agents[explicit.Agent.ID].Origin; got != agentOriginManual {
		t.Fatalf("bridge headers changed manual Agent origin to %q", got)
	}
	if m.state.DaemonRegistration != nil {
		t.Fatal("explicit static bridge credential created a daemon registration")
	}
}
