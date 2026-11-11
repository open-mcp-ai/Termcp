package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ory/fosite"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

type failAuthenticationWriteVault struct {
	Vault
	err error
}

type lockOverrideVault struct {
	Vault
	locked bool
}

func (v lockOverrideVault) AuthenticationLocked() bool {
	return v.locked
}

func (v failAuthenticationWriteVault) WriteAuthentication([]byte) error {
	return v.err
}

func TestOAuthCommitFailureLeavesNoAgentGrantOrCredential(t *testing.T) {
	m, _, _ := configuredManaged(t)
	resource := "https://termcp.example.test/stream"
	client := oauthserver.Client{
		ID:           "mcpc_test",
		Name:         "Test client",
		Registration: oauthserver.ClientRegistrationDCR,
		RedirectURIs: []string{"http://127.0.0.1:4567/callback"},
		CreatedAt:    time.Now().UTC(),
		Enabled:      true,
	}
	m.mu.Lock()
	if m.state.Agents == nil {
		m.state.Agents = map[string]agentRecord{}
	}
	m.state.OAuthServer = OAuthServerSettings{
		Enabled:      true,
		Revision:     1,
		PublicURL:    "https://termcp.example.test",
		OIDCIssuer:   "https://id.example.test",
		OIDCClientID: "termcp",
	}
	m.state.OAuthClients[client.ID] = client
	m.applyOAuthSettingsLocked()
	m.mu.Unlock()

	storage := &oauthFositeStorage{managed: m}
	ctx, err := storage.BeginTX(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := fosite.NewRequest()
	request.SetID("grant-failure")
	request.Client = client.FositeClient(resource)
	request.SetRequestedScopes(fosite.Arguments{"mcp", "offline_access"})
	for _, scope := range request.GetRequestedScopes() {
		request.GrantScope(scope)
	}
	request.GrantAudience(resource)
	now := time.Now().UTC()
	request.RequestedAt = now
	request.SetSession(&fosite.DefaultSession{
		ExpiresAt: map[fosite.TokenType]time.Time{
			fosite.AccessToken:  now.Add(time.Minute),
			fosite.RefreshToken: now.Add(time.Hour),
		},
		Extra: map[string]any{
			"termcp_pending_grant": pendingOAuthGrant{
				UserID:         "user-failure",
				Issuer:         "https://id.example.test",
				Subject:        "subject-failure",
				DisplayName:    "Failure User",
				ClientID:       client.ID,
				Resource:       resource,
				ConfigRevision: 1,
			},
		},
	})
	accessSignature := oauthserver.TokenSignature("toa_" + repeatTestString("a", 43))
	refreshSignature := oauthserver.TokenSignature("tor_" + repeatTestString("b", 43))
	if err := storage.CreateAccessTokenSession(ctx, accessSignature, request); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateRefreshTokenSession(ctx, refreshSignature, accessSignature, request); err != nil {
		t.Fatal(err)
	}
	m.vault = failAuthenticationWriteVault{Vault: m.vault, err: errors.New("disk unavailable")}
	if err := storage.Commit(ctx); err == nil {
		t.Fatal("OAuth commit unexpectedly succeeded")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.state.Agents) != 0 || len(m.state.OAuthGrants) != 0 || len(m.state.OAuthAccess) != 0 || len(m.state.OAuthRefresh) != 0 {
		t.Fatalf("failed commit published partial OAuth state: agents=%d grants=%d access=%d refresh=%d",
			len(m.state.Agents), len(m.state.OAuthGrants), len(m.state.OAuthAccess), len(m.state.OAuthRefresh))
	}
}

func TestOAuthCommitRejectsConcurrentPermissionRevisionChange(t *testing.T) {
	m, _, _ := configuredManaged(t)
	resource := "https://termcp.example.test/stream"
	client := oauthserver.Client{
		ID:           "mcpc_race",
		Name:         "Race client",
		Registration: oauthserver.ClientRegistrationDCR,
		RedirectURIs: []string{"http://127.0.0.1:4567/callback"},
		CreatedAt:    time.Now().UTC(),
		Enabled:      true,
	}
	m.mu.Lock()
	if m.state.Agents == nil {
		m.state.Agents = map[string]agentRecord{}
	}
	m.state.OAuthServer = OAuthServerSettings{
		Enabled: true, Revision: 1, PublicURL: "https://termcp.example.test", OIDCIssuer: "https://id.example.test", OIDCClientID: "termcp",
	}
	m.state.OAuthClients[client.ID] = client
	m.applyOAuthSettingsLocked()
	m.mu.Unlock()

	storage := &oauthFositeStorage{managed: m}
	ctx, err := storage.BeginTX(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := fosite.NewRequest()
	request.SetID("grant-race")
	request.Client = client.FositeClient(resource)
	request.SetRequestedScopes(fosite.Arguments{"mcp"})
	request.GrantScope("mcp")
	request.GrantAudience(resource)
	now := time.Now().UTC()
	request.SetSession(&fosite.DefaultSession{
		ExpiresAt: map[fosite.TokenType]time.Time{fosite.AccessToken: now.Add(time.Minute)},
		Extra: map[string]any{"termcp_pending_grant": pendingOAuthGrant{
			UserID: "race-user", Issuer: "https://id.example.test", Subject: "race-subject",
			DisplayName: "Race User", ClientID: client.ID, Resource: resource, ConfigRevision: 1,
		}},
	})
	if err := storage.CreateAccessTokenSession(ctx, oauthserver.TokenSignature("toa_race"), request); err != nil {
		t.Fatal(err)
	}
	if err := m.commitState(t.Context(), func(next *managedState) (func(), error) {
		next.AccessRevision++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Commit(ctx); !errors.Is(err, fosite.ErrSerializationFailure) {
		t.Fatalf("concurrent permission revision commit error = %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.state.Agents) != 0 || len(m.state.OAuthGrants) != 0 {
		t.Fatalf("stale OAuth transaction provisioned state: agents=%d grants=%d", len(m.state.Agents), len(m.state.OAuthGrants))
	}
}

func TestOAuthConfigurationChangeRevokesGrantsAndCancelsRequests(t *testing.T) {
	m, _, _ := configuredManaged(t)
	now := time.Now().UTC()
	resource := "https://termcp.example.test/stream"
	client := oauthserver.Client{
		ID:           "mcpc_config",
		Name:         "Config client",
		Registration: oauthserver.ClientRegistrationDCR,
		RedirectURIs: []string{"http://127.0.0.1:4567/callback"},
		CreatedAt:    now,
		Enabled:      true,
	}
	user := loginUser{
		ID:          "user-config",
		Issuer:      "https://id.example.test",
		Subject:     "subject-config",
		DisplayName: "Config User",
		Enabled:     true,
		Revision:    1,
		CreatedAt:   now,
		UpdatedAt:   now,
		LastLoginAt: now,
	}
	agent := agentRecord{
		ID:          "agent-config",
		Name:        "Config client · Config User",
		GroupID:     m.state.DefaultGroupID,
		Enabled:     true,
		Revision:    1,
		CreatedAt:   now,
		OwnerUserID: user.ID,
		Origin:      agentOriginOAuth,
	}
	grant := oauthGrant{
		ID:             "grant-config",
		UserID:         user.ID,
		AgentID:        agent.ID,
		ClientID:       client.ID,
		Resource:       resource,
		Scopes:         []string{"mcp", "offline_access"},
		ConfigRevision: 1,
		CreatedAt:      now,
		ExpiresAt:      now.Add(time.Hour),
	}
	request := oauthStoredRequest{
		ID:                grant.ID,
		RequestedAt:       now,
		ClientID:          client.ID,
		RequestedScopes:   []string{"mcp", "offline_access"},
		GrantedScopes:     []string{"mcp", "offline_access"},
		RequestedAudience: []string{resource},
		GrantedAudience:   []string{resource},
		Session: oauthStoredSession{
			ExpiresAt: map[string]time.Time{"access_token": now.Add(time.Hour)},
		},
	}
	credential := oauthCredentialRecord{
		ID:        "oauth-config",
		GrantID:   grant.ID,
		ClientID:  client.ID,
		Resource:  resource,
		Scopes:    []string{"mcp", "offline_access"},
		FamilyID:  grant.ID,
		Request:   request,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}

	ended := make(chan struct{})
	m.mu.Lock()
	if m.state.Agents == nil {
		m.state.Agents = map[string]agentRecord{}
	}
	m.state.OAuthServer = OAuthServerSettings{
		Enabled:      true,
		Revision:     1,
		PublicURL:    "https://termcp.example.test",
		OIDCIssuer:   "https://id.example.test",
		OIDCClientID: "termcp",
	}
	m.state.OAuthClients[client.ID] = client
	m.state.LoginUsers[user.ID] = user
	m.state.Agents[agent.ID] = agent
	m.state.OAuthGrants[grant.ID] = grant
	m.state.OAuthAccess[oauthserver.TokenSignature("toa_config")] = credential
	m.requests["token:"+credential.ID] = map[*int]context.CancelFunc{
		new(int): func() { close(ended) },
	}
	m.applyOAuthSettingsLocked()
	m.mu.Unlock()

	_, status, err := m.updateOAuthServerSettings(t.Context(), oauthSettingsInput{
		Enabled:          true,
		Revision:         1,
		PublicURL:        "https://new-termcp.example.test",
		OIDCIssuer:       "https://id.example.test",
		OIDCClientID:     "termcp",
		OIDCClientSecret: "",
	}, uint64Pointer(1))
	if err != nil || status != 200 {
		t.Fatalf("change OAuth settings: status=%d err=%v", status, err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("configuration change did not cancel the active OAuth request")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.OAuthGrants[grant.ID].RevokedAt == nil {
		t.Fatal("configuration change did not revoke the grant")
	}
	if m.state.OAuthAccess[oauthserver.TokenSignature("toa_config")].RevokedAt == nil {
		t.Fatal("configuration change did not revoke the access credential")
	}
}

func uint64Pointer(value uint64) *uint64 {
	return &value
}

func repeatTestString(value string, count int) string {
	out := ""
	for range count {
		out += value
	}
	return out
}
