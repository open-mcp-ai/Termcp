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

func TestMCPTokenSingleActiveRotationHistoryAndPersistence(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	sink := &captureAuditSink{}
	m.SetAuditSink(sink)
	group := createTestGroup(t, m, cookie, session, "token lifecycle", "viewer")
	first := issueTestCredential(t, m, cookie, session, "/auth/principals",
		fmt.Sprintf(`{"name":"token-lifecycle-agent","group_id":%q}`, group.ID))
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	probe := func(token string) int {
		return bearerRequest(h, http.MethodGet, "/auth/mcp", token, "").Code
	}
	if got := probe(first.Token); got != http.StatusOK {
		t.Fatalf("initial token = %d", got)
	}

	duplicate := managedRequest(t, h, http.MethodPost, "/auth/tokens",
		fmt.Sprintf(`{"principal_id":%q,"label":"duplicate"}`, first.Agent.ID), cookie, session.CSRF)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("ordinary duplicate issuance = %d, want 409: %s", duplicate.Code, duplicate.Body.String())
	}

	rotated := managedRequest(t, h, http.MethodPost, "/auth/tokens/"+first.Credential.ID+"/rotate", `{}`, cookie, session.CSRF)
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rotated.Code, rotated.Body.String())
	}
	var rotation struct {
		Token      string    `json:"token"`
		Credential tokenInfo `json:"credential"`
		Previous   tokenInfo `json:"previous_credential"`
	}
	if err := json.Unmarshal(rotated.Body.Bytes(), &rotation); err != nil {
		t.Fatal(err)
	}
	if rotation.Token == "" || !rotation.Credential.Active || rotation.Previous.Active || rotation.Previous.ReplacedBy != rotation.Credential.ID {
		t.Fatalf("unexpected rotation response: %+v", rotation)
	}
	if got := probe(first.Token); got != http.StatusUnauthorized {
		t.Fatalf("old token after rotation = %d, want 401", got)
	}
	if got := probe(rotation.Token); got != http.StatusOK {
		t.Fatalf("replacement token after rotation = %d, want 200", got)
	}

	listing := managedRequest(t, h, http.MethodGet, "/auth/tokens", "", cookie, "")
	if listing.Code != http.StatusOK {
		t.Fatalf("list tokens = %d: %s", listing.Code, listing.Body.String())
	}
	for _, secret := range []string{first.Token, rotation.Token, "digest"} {
		if strings.Contains(listing.Body.String(), secret) {
			t.Fatalf("token listing leaked %q", secret)
		}
	}
	var listed struct {
		Tokens []tokenInfo `json:"tokens"`
	}
	if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if activeTokensFor(listed.Tokens, first.Agent.ID) != 1 || len(listed.Tokens) != 2 {
		t.Fatalf("unexpected token history: %+v", listed.Tokens)
	}
	principals := managedRequest(t, h, http.MethodGet, "/auth/principals", "", cookie, "")
	if principals.Code != http.StatusOK {
		t.Fatalf("list Agents = %d: %s", principals.Code, principals.Body.String())
	}
	var listedAgents struct {
		Principals []struct {
			ID                    string `json:"id"`
			CredentialCount       int    `json:"credential_count"`
			ActiveCredentialCount int    `json:"active_credential_count"`
		} `json:"principals"`
	}
	if err := json.Unmarshal(principals.Body.Bytes(), &listedAgents); err != nil {
		t.Fatal(err)
	}
	var agentView struct {
		ID                    string `json:"id"`
		CredentialCount       int    `json:"credential_count"`
		ActiveCredentialCount int    `json:"active_credential_count"`
	}
	for _, item := range listedAgents.Principals {
		if item.ID == first.Agent.ID {
			agentView = item
		}
	}
	if agentView.CredentialCount != 2 || agentView.ActiveCredentialCount != 1 {
		t.Fatalf("unexpected Agent token counts: %+v", agentView)
	}

	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetAuditSink(sink)
	cookie, session, err = reopened.loginWithPassword(testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	h = reopened.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if got := probe(rotation.Token); got != http.StatusOK {
		t.Fatalf("replacement token after restart = %d", got)
	}
	if got := probe(first.Token); got != http.StatusUnauthorized {
		t.Fatalf("revoked token after restart = %d", got)
	}

	revoke := managedRequest(t, h, http.MethodDelete, "/auth/tokens/"+rotation.Credential.ID, "", cookie, session.CSRF)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke replacement = %d: %s", revoke.Code, revoke.Body.String())
	}
	if got := probe(rotation.Token); got != http.StatusUnauthorized {
		t.Fatalf("revoked replacement = %d, want 401", got)
	}
	clear := managedRequest(t, h, http.MethodDelete, "/auth/tokens/history?principal_id="+first.Agent.ID, "", cookie, session.CSRF)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear history = %d: %s", clear.Code, clear.Body.String())
	}
	var cleared struct {
		Deleted int `json:"deleted_tokens"`
	}
	if err := json.Unmarshal(clear.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Deleted != 2 {
		t.Fatalf("cleared %d history records, want 2", cleared.Deleted)
	}
	listing = managedRequest(t, h, http.MethodGet, "/auth/tokens", "", cookie, "")
	if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tokens) != 0 {
		t.Fatalf("cleared history remains: %+v", listed.Tokens)
	}
	actions := make([]string, 0)
	for _, event := range sink.snapshot() {
		actions = append(actions, event.Action)
	}
	for _, want := range []string{"credential.issue", "credential.rotate", "credential.revoke", "credential.history_clear"} {
		if !slicesContains(actions, want) {
			t.Fatalf("missing audit action %q: %v", want, actions)
		}
	}
}

func TestMCPTokenConcurrentIssueAndRotationKeepOneActive(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	create := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/auth/principals",
		`{"name":"race-agent","issue_token":false}`, cookie, session.CSRF)
	if create.Code != http.StatusOK {
		t.Fatalf("create Agent = %d: %s", create.Code, create.Body.String())
	}
	var created struct {
		Principal agentRecord `json:"principal"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	const workers = 24
	codes := make(chan int, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			response := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/auth/tokens",
				fmt.Sprintf(`{"principal_id":%q}`, created.Principal.ID), cookie, session.CSRF)
			codes <- response.Code
		})
	}
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
		default:
			t.Fatalf("concurrent issue = %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent issue succeeded %d times, want 1", successes)
	}

	s := credentialState(m.state)
	active, ok := activeTokenFor(s, created.Principal.ID)
	if !ok {
		t.Fatal("concurrent issue left no active token")
	}
	rotateCodes := make(chan int, workers)
	for range workers {
		wg.Go(func() {
			response := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/auth/tokens/"+active.ID+"/rotate", `{}`, cookie, session.CSRF)
			rotateCodes <- response.Code
		})
	}
	wg.Wait()
	close(rotateCodes)
	successes = 0
	for code := range rotateCodes {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusNotFound:
		default:
			t.Fatalf("concurrent rotate = %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent rotation succeeded %d times, want 1", successes)
	}
	s = credentialState(m.state)
	if activeTokensForState(s, created.Principal.ID) != 1 {
		t.Fatal("concurrent rotation left multiple active tokens")
	}
}

func TestMCPTokenExpiryAndLastUseMetadata(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	base := m.now().UTC()
	m.now = func() time.Time { return base }
	create := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/auth/principals",
		`{"name":"expiry-agent","issue_token":false}`, cookie, session.CSRF)
	if create.Code != http.StatusOK {
		t.Fatalf("create Agent = %d: %s", create.Code, create.Body.String())
	}
	var created struct {
		Principal agentRecord `json:"principal"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	expiry := base.Add(time.Hour)
	issuedResponse := managedRequest(t, h, http.MethodPost, "/auth/tokens",
		`{"principal_id":"`+created.Principal.ID+`","expires_at":"`+expiry.Format(time.RFC3339Nano)+`"}`,
		cookie, session.CSRF)
	if issuedResponse.Code != http.StatusOK {
		t.Fatalf("issue expiring token = %d: %s", issuedResponse.Code, issuedResponse.Body.String())
	}
	var issued struct {
		Token      string    `json:"token"`
		Credential tokenInfo `json:"credential"`
	}
	if err := json.Unmarshal(issuedResponse.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Credential.ExpiresAt == nil || !issued.Credential.ExpiresAt.Equal(expiry) || issued.Credential.Active != true {
		t.Fatalf("unexpected issued credential: %+v", issued.Credential)
	}
	if code := bearerRequest(h, http.MethodGet, "/auth/mcp", issued.Token, "").Code; code != http.StatusOK {
		t.Fatalf("expiring token before expiry = %d", code)
	}
	listing := managedRequest(t, h, http.MethodGet, "/auth/tokens", "", cookie, "")
	var listed struct {
		Tokens []tokenInfo `json:"tokens"`
	}
	if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tokens) != 1 || listed.Tokens[0].LastUsedAt == nil {
		t.Fatalf("last-used metadata missing: %+v", listed.Tokens)
	}

	m.now = func() time.Time { return base.Add(2 * time.Hour) }
	if code := bearerRequest(h, http.MethodGet, "/auth/mcp", issued.Token, "").Code; code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", code)
	}
	listing = managedRequest(t, h, http.MethodGet, "/auth/tokens", "", cookie, "")
	if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tokens) != 1 || listed.Tokens[0].Active || listed.Tokens[0].Status != "expired" {
		t.Fatalf("expired token status = %+v", listed.Tokens)
	}
	past := managedRequest(t, h, http.MethodPost, "/auth/tokens",
		`{"principal_id":"`+created.Principal.ID+`","expires_at":"`+base.Add(-time.Minute).Format(time.RFC3339Nano)+`"}`,
		cookie, session.CSRF)
	if past.Code != http.StatusBadRequest {
		t.Fatalf("past expiry = %d, want 400: %s", past.Code, past.Body.String())
	}

	next := managedRequest(t, h, http.MethodPost, "/auth/tokens", `{"principal_id":"`+created.Principal.ID+`"}`, cookie, session.CSRF)
	if next.Code != http.StatusOK {
		t.Fatalf("issue after expiry = %d: %s", next.Code, next.Body.String())
	}
	listing = managedRequest(t, h, http.MethodGet, "/auth/tokens", "", cookie, "")
	if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if activeTokensFor(listed.Tokens, created.Principal.ID) != 1 || len(listed.Tokens) != 2 {
		t.Fatalf("expired-token replacement state = %+v", listed.Tokens)
	}
	foundUsedHistory := false
	for _, token := range listed.Tokens {
		if token.ID == issued.Credential.ID && token.LastUsedAt != nil && token.RevokedAt != nil {
			foundUsedHistory = true
		}
	}
	if !foundUsedHistory {
		t.Fatalf("last-used history did not persist with the next mutation: %+v", listed.Tokens)
	}
}

func TestLegacyDuplicateTokensMigrateDeterministically(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	agent := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"legacy-duplicates"}`)
	s := credentialState(m.state)
	now := m.now().UTC()
	older := s.Tokens[agent.Credential.ID]
	older.RevokedAt = nil
	older.CreatedAt = now.Add(-2 * time.Hour)
	s.Tokens[older.ID] = older
	newer := older
	newer.ID = "newer-token"
	newer.Digest = tokenDigest("newer-token-secret")
	newer.CreatedAt = now.Add(-time.Hour)
	s.Tokens[newer.ID] = newer

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.vault.WriteAuthentication(raw); err != nil {
		t.Fatal(err)
	}
	sink := &captureAuditSink{}
	m.SetAuditSink(sink)
	m.mu.Lock()
	err = m.loadLocked()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateState(m.state, false); err != nil {
		t.Fatal(err)
	}
	migratedOlder := m.state.Tokens[older.ID]
	migratedNewer := m.state.Tokens[newer.ID]
	if migratedOlder.RevokedAt == nil || migratedOlder.ReplacedBy != newer.ID {
		t.Fatalf("older token was not revoked deterministically: %+v", migratedOlder)
	}
	if migratedNewer.RevokedAt != nil {
		t.Fatal("newest token was not retained as current")
	}
	actions := make([]string, 0)
	for _, event := range sink.snapshot() {
		actions = append(actions, event.Action)
	}
	if !slicesContains(actions, "credential.migrate_single_active") {
		t.Fatalf("missing migration audit event: %v", actions)
	}
}

func activeTokensFor(tokens []tokenInfo, principal string) int {
	count := 0
	for _, token := range tokens {
		if token.PrincipalID == principal && token.Active {
			count++
		}
	}
	return count
}

func activeTokensForState(state managedState, principal string) int {
	count := 0
	for _, token := range state.Tokens {
		if token.PrincipalID == principal && tokenActive(token) {
			count++
		}
	}
	return count
}
