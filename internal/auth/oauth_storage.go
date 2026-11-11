package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ory/fosite"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

type oauthTransactionContextKey struct{}

type oauthFositeTransaction struct {
	base      managedState
	candidate managedState
	committed bool
	cancel    map[string]string
}

type oauthFositeStorage struct {
	managed *Managed
}

func transactionFromContext(ctx context.Context) *oauthFositeTransaction {
	tx, _ := ctx.Value(oauthTransactionContextKey{}).(*oauthFositeTransaction)
	return tx
}

func (s *oauthFositeStorage) BeginTX(ctx context.Context) (context.Context, error) {
	if tx := transactionFromContext(ctx); tx != nil {
		return ctx, nil
	}
	if s == nil || s.managed == nil {
		return nil, errors.New("OAuth storage is unavailable")
	}
	m := s.managed
	m.mu.Lock()
	if m.state.Version == 0 || m.vault.AuthenticationLocked() || !m.state.OAuthServer.Enabled {
		m.mu.Unlock()
		return nil, errVaultLocked
	}
	base := cloneManagedState(m.state)
	m.mu.Unlock()
	tx := &oauthFositeTransaction{
		base:      base,
		candidate: cloneManagedState(base),
		cancel:    map[string]string{},
	}
	return context.WithValue(ctx, oauthTransactionContextKey{}, tx), nil
}

func (s *oauthFositeStorage) Commit(ctx context.Context) error {
	tx := transactionFromContext(ctx)
	if tx == nil {
		return errors.New("OAuth transaction is missing")
	}
	if tx.committed {
		return nil
	}
	if err := s.managed.commitOAuthCandidate(ctx, tx); err != nil {
		slog.WarnContext(ctx, "OAuth storage commit failed", "err", err)
		return err
	}
	tx.committed = true
	return nil
}

func (s *oauthFositeStorage) Rollback(ctx context.Context) error {
	tx := transactionFromContext(ctx)
	if tx == nil {
		return nil
	}
	tx.candidate = cloneManagedState(tx.base)
	return nil
}

func (m *Managed) commitOAuthCandidate(ctx context.Context, tx *oauthFositeTransaction) error {
	cancel := map[string]string{}
	for signature, candidate := range tx.candidate.OAuthAccess {
		base, existed := tx.base.OAuthAccess[signature]
		if (!existed || base.RevokedAt == nil) && candidate.RevokedAt != nil {
			grant := tx.candidate.OAuthGrants[candidate.GrantID]
			cancel[candidate.ID] = grant.AgentID
		}
	}
	after := func() {
		m.cancelOAuthCredentials(cancel)
	}
	return m.commitState(ctx, func(next *managedState) (func(), error) {
		if next.OAuthServer.Revision != tx.base.OAuthServer.Revision ||
			next.AccessRevision != tx.base.AccessRevision ||
			next.Revision != tx.base.Revision ||
			next.OAuthRevision != tx.base.OAuthRevision {
			return nil, fosite.ErrSerializationFailure
		}
		next.OAuthRevision++
		tx.candidate.OAuthRevision = next.OAuthRevision
		next.LoginUsers = tx.candidate.LoginUsers
		next.OAuthClients = tx.candidate.OAuthClients
		next.OAuthGrants = tx.candidate.OAuthGrants
		next.OAuthCodes = tx.candidate.OAuthCodes
		next.OAuthAccess = tx.candidate.OAuthAccess
		next.OAuthRefresh = tx.candidate.OAuthRefresh
		for id, agent := range tx.candidate.Agents {
			baseAgent, existed := tx.base.Agents[id]
			if !existed || baseAgent != agent {
				next.Agents[id] = agent
			}
		}
		return after, nil
	})
}

func (s *oauthFositeStorage) state(ctx context.Context) (managedState, error) {
	if tx := transactionFromContext(ctx); tx != nil {
		return tx.candidate, nil
	}
	if s == nil || s.managed == nil {
		return managedState{}, errors.New("OAuth storage is unavailable")
	}
	m := s.managed
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vault.AuthenticationLocked() || !m.state.OAuthServer.Enabled {
		return managedState{}, errVaultLocked
	}
	return cloneManagedState(m.state), nil
}

func (s *oauthFositeStorage) mutableState(ctx context.Context) (*managedState, error) {
	tx := transactionFromContext(ctx)
	if tx == nil {
		return nil, errors.New("OAuth mutation requires a transaction")
	}
	return &tx.candidate, nil
}

func (s *oauthFositeStorage) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	if id == "" {
		return nil, fosite.ErrNotFound
	}
	client, err := s.managed.resolveOAuthClient(ctx, id)
	if err != nil {
		return nil, err
	}
	if requested := requestedRedirectFromContext(ctx); requested != "" {
		target, parseErr := url.Parse(requested)
		if parseErr != nil || !oauthserver.RedirectAllowed(client.RedirectURIs, target) {
			return nil, fosite.ErrInvalidClient.WithHint("The requested redirect URI is not registered for this client")
		}
		if !slices.Contains(client.RedirectURIs, requested) {
			client.RedirectURIs = append(append([]string(nil), client.RedirectURIs...), requested)
		}
	}
	return client.FositeClient(s.managed.oauthResource()), nil
}

// Public clients use token_endpoint_auth_method=none. Fosite still asks the
// storage to reject replayed JWT assertions; this profile does not accept them.
func (s *oauthFositeStorage) ClientAssertionJWTValid(context.Context, string) error {
	return nil
}

func (s *oauthFositeStorage) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return fosite.ErrInvalidClient.WithHint("JWT client assertions are not enabled for public MCP clients")
}

func (s *oauthFositeStorage) CreateAuthorizeCodeSession(ctx context.Context, signature string, request fosite.Requester) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	if len(state.OAuthCodes) >= maxOAuthCodes {
		pruneOAuthCodes(state, s.managed.now().UTC())
		if len(state.OAuthCodes) >= maxOAuthCodes {
			return fosite.ErrTemporarilyUnavailable.WithHint("The OAuth authorization-code limit has been reached")
		}
	}
	expiry := request.GetSession().GetExpiresAt(fosite.AuthorizeCode)
	state.OAuthCodes[signature] = oauthCodeRecord{
		Request:   storedRequestFromFosite(request),
		ExpiresAt: expiry,
	}
	return nil
}

func (s *oauthFositeStorage) GetAuthorizeCodeSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	state, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	record, ok := state.OAuthCodes[signature]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	request, err := requestFromStored(state, record.Request, session)
	if err != nil {
		return nil, err
	}
	if record.ConsumedAt != nil {
		return request, fosite.ErrInvalidatedAuthorizeCode
	}
	if !record.ExpiresAt.IsZero() && !s.managed.now().UTC().Before(record.ExpiresAt) {
		return request, fosite.ErrTokenExpired
	}
	return request, nil
}

func (s *oauthFositeStorage) InvalidateAuthorizeCodeSession(ctx context.Context, signature string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	record, ok := state.OAuthCodes[signature]
	if !ok {
		return fosite.ErrNotFound
	}
	now := s.managed.now().UTC()
	if record.ConsumedAt == nil {
		record.ConsumedAt = &now
		state.OAuthCodes[signature] = record
	}
	return nil
}

func (s *oauthFositeStorage) CreatePKCERequestSession(ctx context.Context, signature string, request fosite.Requester) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	record, ok := state.OAuthCodes[signature]
	if !ok {
		return fosite.ErrNotFound
	}
	record.PKCEChallenge = request.GetRequestForm().Get("code_challenge")
	record.PKCEMethod = request.GetRequestForm().Get("code_challenge_method")
	state.OAuthCodes[signature] = record
	return nil
}

func (s *oauthFositeStorage) GetPKCERequestSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	state, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	record, ok := state.OAuthCodes[signature]
	if !ok || record.PKCEChallenge == "" {
		return nil, fosite.ErrNotFound
	}
	request, err := requestFromStored(state, record.Request, session)
	if err != nil {
		return nil, err
	}
	request.GetRequestForm().Set("code_challenge", record.PKCEChallenge)
	request.GetRequestForm().Set("code_challenge_method", record.PKCEMethod)
	return request, nil
}

func (s *oauthFositeStorage) DeletePKCERequestSession(ctx context.Context, signature string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	record, ok := state.OAuthCodes[signature]
	if !ok {
		return nil
	}
	record.PKCEChallenge = ""
	record.PKCEMethod = ""
	state.OAuthCodes[signature] = record
	return nil
}

func (s *oauthFositeStorage) CreateAccessTokenSession(ctx context.Context, signature string, request fosite.Requester) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	if err := s.createGrantIfNeeded(state, request); err != nil {
		return err
	}
	if len(state.OAuthAccess) >= maxOAuthAccess {
		pruneOAuthCredentials(state.OAuthAccess, s.managed.now().UTC())
		if len(state.OAuthAccess) >= maxOAuthAccess {
			return fosite.ErrTemporarilyUnavailable.WithHint("The OAuth access-token limit has been reached")
		}
	}
	expiry := request.GetSession().GetExpiresAt(fosite.AccessToken)
	record := oauthCredentialRecord{
		ID:        "oauth-" + strings.TrimPrefix(randomIDOrEmpty(), "id-"),
		GrantID:   request.GetID(),
		ClientID:  request.GetClient().GetID(),
		Resource:  firstAudience(request.GetGrantedAudience(), request.GetRequestedAudience()),
		Scopes:    append([]string(nil), request.GetGrantedScopes()...),
		FamilyID:  request.GetID(),
		Request:   storedRequestFromFosite(request),
		CreatedAt: s.managed.now().UTC(),
		ExpiresAt: expiry,
	}
	if record.ID == "oauth-" {
		return errors.New("could not generate OAuth credential ID")
	}
	if record.Resource == "" {
		record.Resource = s.managed.oauthResource()
	}
	state.OAuthAccess[signature] = record
	return nil
}

func (s *oauthFositeStorage) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	state, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	record, ok := state.OAuthAccess[signature]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	request, err := requestFromStored(state, record.Request, session)
	if err != nil {
		return nil, err
	}
	now := s.managed.now().UTC()
	if record.RevokedAt != nil || !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return request, fosite.ErrInactiveToken
	}
	return request, nil
}

func (s *oauthFositeStorage) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	record, ok := state.OAuthAccess[signature]
	if !ok {
		return fosite.ErrNotFound
	}
	record.RevokedAt = ptrTime(s.managed.now().UTC())
	state.OAuthAccess[signature] = record
	return nil
}

func (s *oauthFositeStorage) CreateRefreshTokenSession(ctx context.Context, signature, accessSignature string, request fosite.Requester) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	if err := s.createGrantIfNeeded(state, request); err != nil {
		return err
	}
	if len(state.OAuthRefresh) >= maxOAuthRefresh {
		pruneOAuthCredentials(state.OAuthRefresh, s.managed.now().UTC())
		if len(state.OAuthRefresh) >= maxOAuthRefresh {
			return fosite.ErrTemporarilyUnavailable.WithHint("The OAuth refresh-token limit has been reached")
		}
	}
	expiry := request.GetSession().GetExpiresAt(fosite.RefreshToken)
	if grant := state.OAuthGrants[request.GetID()]; !grant.ExpiresAt.IsZero() &&
		(expiry.IsZero() || expiry.After(grant.ExpiresAt)) {
		expiry = grant.ExpiresAt
		request.GetSession().SetExpiresAt(fosite.RefreshToken, expiry)
	}
	record := oauthCredentialRecord{
		ID:        "oauth-" + strings.TrimPrefix(randomIDOrEmpty(), "id-"),
		GrantID:   request.GetID(),
		ClientID:  request.GetClient().GetID(),
		Resource:  firstAudience(request.GetGrantedAudience(), request.GetRequestedAudience()),
		Scopes:    append([]string(nil), request.GetGrantedScopes()...),
		FamilyID:  request.GetID(),
		Request:   storedRequestFromFosite(request),
		CreatedAt: s.managed.now().UTC(),
		ExpiresAt: expiry,
	}
	if record.ID == "oauth-" {
		return errors.New("could not generate OAuth credential ID")
	}
	if record.Resource == "" {
		record.Resource = s.managed.oauthResource()
	}
	for oldSignature, old := range state.OAuthRefresh {
		if old.GrantID == record.GrantID && old.RevokedAt != nil && old.ReplacedBy == "" {
			old.ReplacedBy = record.ID
			state.OAuthRefresh[oldSignature] = old
		}
	}
	state.OAuthRefresh[signature] = record
	return nil
}

func (s *oauthFositeStorage) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	state, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	record, ok := state.OAuthRefresh[signature]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	request, err := requestFromStored(state, record.Request, session)
	if err != nil {
		return nil, err
	}
	now := s.managed.now().UTC()
	if record.RevokedAt != nil || !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return request, fosite.ErrInactiveToken
	}
	return request, nil
}

func (s *oauthFositeStorage) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	record, ok := state.OAuthRefresh[signature]
	if !ok {
		return fosite.ErrNotFound
	}
	now := s.managed.now().UTC()
	if record.RevokedAt == nil {
		record.RevokedAt = &now
		state.OAuthRefresh[signature] = record
	}
	return nil
}

func (s *oauthFositeStorage) RotateRefreshToken(ctx context.Context, requestID, signature string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	record, ok := state.OAuthRefresh[signature]
	if !ok || record.GrantID != requestID || record.RevokedAt != nil {
		return fosite.ErrSerializationFailure
	}
	now := s.managed.now().UTC()
	record.RevokedAt = &now
	state.OAuthRefresh[signature] = record
	return nil
}

func (s *oauthFositeStorage) RevokeAccessToken(ctx context.Context, requestID string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	return s.revokeGrant(state, requestID, s.managed.now().UTC(), "access_token_family_revoked")
}

func (s *oauthFositeStorage) RevokeRefreshToken(ctx context.Context, requestID string) error {
	state, err := s.mutableState(ctx)
	if err != nil {
		return err
	}
	return s.revokeGrant(state, requestID, s.managed.now().UTC(), "refresh_token_family_revoked")
}

func (s *oauthFositeStorage) createGrantIfNeeded(state *managedState, request fosite.Requester) error {
	requestID := request.GetID()
	if grant, ok := state.OAuthGrants[requestID]; ok {
		now := s.managed.now().UTC()
		if !grantActive(grant, now) || grant.ConfigRevision != state.OAuthServer.Revision {
			return fosite.ErrAccessDenied.WithHint("The OAuth grant is no longer active")
		}
		if user := state.LoginUsers[grant.UserID]; !user.Enabled {
			return fosite.ErrAccessDenied.WithHint("The login user is disabled")
		}
		agent := state.Agents[grant.AgentID]
		if !agent.Enabled || agent.OwnerUserID != grant.UserID {
			return fosite.ErrAccessDenied.WithHint("The Agent is disabled")
		}
		if _, err := resolvedAgentPolicy(*state, agent); err != nil {
			return fosite.ErrAccessDenied.WithHint("The Agent permission group is unavailable")
		}
		if client := state.OAuthClients[grant.ClientID]; !client.Enabled {
			return fosite.ErrAccessDenied.WithHint("The OAuth client is disabled")
		}
		return nil
	}
	pending, err := pendingGrantFromRequest(request)
	if err != nil {
		return fosite.ErrInvalidGrant.WithWrap(err)
	}
	now := s.managed.now().UTC()
	user, ok := state.LoginUsers[pending.UserID]
	identityID := findLoginUserBySubject(*state, pending.Issuer, pending.Subject)
	if identityID != "" {
		user = state.LoginUsers[identityID]
		ok = true
		pending.UserID = identityID
	}
	if ok {
		if !user.Enabled {
			return fosite.ErrAccessDenied.WithHint("The login user is disabled")
		}
		if user.Issuer != pending.Issuer || user.Subject != pending.Subject {
			return fosite.ErrAccessDenied.WithHint("The login identity no longer matches")
		}
		user.DisplayName = pending.DisplayName
		user.Username = pending.Username
		user.AvatarURL = pending.AvatarURL
		user.AvatarFile = pending.AvatarFile
		user.AvatarType = pending.AvatarType
		user.UpdatedAt = now
		user.LastLoginAt = now
		state.LoginUsers[user.ID] = user
	} else {
		if pending.UserID == "" || pending.Issuer == "" || pending.Subject == "" {
			return fosite.ErrAccessDenied.WithHint("The login identity is incomplete")
		}
		if _, exists := state.LoginUsers[pending.UserID]; exists {
			return fosite.ErrSerializationFailure
		}
		user = loginUser{
			ID:          pending.UserID,
			Issuer:      pending.Issuer,
			Subject:     pending.Subject,
			DisplayName: pending.DisplayName,
			Username:    pending.Username,
			AvatarURL:   pending.AvatarURL,
			AvatarFile:  pending.AvatarFile,
			AvatarType:  pending.AvatarType,
			Enabled:     true,
			Revision:    1,
			CreatedAt:   now,
			UpdatedAt:   now,
			LastLoginAt: now,
		}
		state.LoginUsers[user.ID] = user
	}
	if countGrantsForUser(*state, user.ID, now) >= maxGrantsPerUser {
		return fosite.ErrTemporarilyUnavailable.WithHint("This login user has reached the OAuth grant limit")
	}
	if len(state.OAuthGrants) >= maxOAuthGrants {
		return fosite.ErrTemporarilyUnavailable.WithHint("The OAuth grant limit has been reached")
	}
	groupID := state.DefaultGroupID
	group, ok := state.Groups[groupID]
	if !ok || !group.Enabled {
		return fosite.ErrTemporarilyUnavailable.WithHint("The default permission group is unavailable")
	}
	if len(state.Agents) >= maxAgents {
		return fosite.ErrTemporarilyUnavailable.WithHint("The Agent limit has been reached")
	}
	client, ok := state.OAuthClients[request.GetClient().GetID()]
	if !ok || !client.Enabled {
		return fosite.ErrInvalidClient
	}
	agentID, err := randomSecret()
	if err != nil {
		return err
	}
	name := agentNameForOAuth(client.Name, user.DisplayName)
	agent := agentRecord{
		ID:          agentID,
		Name:        name,
		GroupID:     groupID,
		Enabled:     true,
		Revision:    1,
		CreatedAt:   now,
		OwnerUserID: user.ID,
		Origin:      agentOriginOAuth,
	}
	state.Agents[agent.ID] = agent
	resource := firstAudience(request.GetRequestedAudience(), request.GetGrantedAudience())
	if resource == "" {
		resource = s.managed.oauthResource()
	}
	state.OAuthGrants[requestID] = oauthGrant{
		ID:             requestID,
		UserID:         user.ID,
		AgentID:        agent.ID,
		ClientID:       client.ID,
		Resource:       resource,
		Scopes:         append([]string(nil), request.GetRequestedScopes()...),
		ConfigRevision: pending.ConfigRevision,
		CreatedAt:      now,
		ExpiresAt:      now.Add(oauthserver.DefaultRefreshTokenLifetime),
	}
	return nil
}

func (s *oauthFositeStorage) revokeGrant(state *managedState, grantID string, now time.Time, reason string) error {
	grant, ok := state.OAuthGrants[grantID]
	if !ok {
		return fosite.ErrNotFound
	}
	if grant.RevokedAt == nil {
		grant.RevokedAt = &now
		grant.RevokeReason = reason
		state.OAuthGrants[grantID] = grant
	}
	for signature, record := range state.OAuthAccess {
		if record.GrantID == grantID && record.RevokedAt == nil {
			record.RevokedAt = &now
			state.OAuthAccess[signature] = record
		}
	}
	for signature, record := range state.OAuthRefresh {
		if record.GrantID == grantID && record.RevokedAt == nil {
			record.RevokedAt = &now
			state.OAuthRefresh[signature] = record
		}
	}
	return nil
}

func storedRequestFromFosite(request fosite.Requester) oauthStoredRequest {
	form := map[string][]string{}
	if request.GetRequestForm() != nil {
		for key, values := range request.GetRequestForm() {
			form[key] = append([]string(nil), values...)
		}
	}
	return oauthStoredRequest{
		ID:                request.GetID(),
		RequestedAt:       request.GetRequestedAt(),
		ClientID:          request.GetClient().GetID(),
		RequestedScopes:   append([]string(nil), request.GetRequestedScopes()...),
		GrantedScopes:     append([]string(nil), request.GetGrantedScopes()...),
		RequestedAudience: append([]string(nil), request.GetRequestedAudience()...),
		GrantedAudience:   append([]string(nil), request.GetGrantedAudience()...),
		Form:              form,
		Session:           storedSession(request.GetSession()),
	}
}

func requestFromStored(state managedState, stored oauthStoredRequest, session fosite.Session) (fosite.Requester, error) {
	client, ok := state.OAuthClients[stored.ClientID]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	request := fosite.NewRequest()
	request.ID = stored.ID
	request.RequestedAt = stored.RequestedAt
	request.Client = client.FositeClient(resourceForState(state))
	request.SetRequestedScopes(fosite.Arguments(stored.RequestedScopes))
	request.SetRequestedAudience(fosite.Arguments(stored.RequestedAudience))
	for _, scope := range stored.GrantedScopes {
		request.GrantScope(scope)
	}
	for _, audience := range stored.GrantedAudience {
		request.GrantAudience(audience)
	}
	request.Form = cloneURLValues(stored.Form)
	request.Session = hydrateSession(session, stored.Session)
	return request, nil
}

func cloneURLValues(in map[string][]string) url.Values {
	out := make(url.Values, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func pendingGrantFromRequest(request fosite.Requester) (pendingOAuthGrant, error) {
	extra, ok := request.GetSession().(interface{ GetExtraClaims() map[string]any })
	if !ok {
		return pendingOAuthGrant{}, errors.New("OAuth session has no extra claims")
	}
	raw, ok := extra.GetExtraClaims()["termcp_pending_grant"]
	if !ok {
		return pendingOAuthGrant{}, errors.New("OAuth authorization has no pending grant")
	}
	return pendingGrantFromValue(raw)
}

func pendingGrantFromStored(request oauthStoredRequest) (pendingOAuthGrant, error) {
	if request.Session.Extra == nil {
		return pendingOAuthGrant{}, errors.New("OAuth authorization has no pending grant")
	}
	raw, ok := request.Session.Extra["termcp_pending_grant"]
	if !ok {
		return pendingOAuthGrant{}, errors.New("OAuth authorization has no pending grant")
	}
	return pendingGrantFromValue(raw)
}

func pendingGrantFromValue(raw any) (pendingOAuthGrant, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return pendingOAuthGrant{}, err
	}
	var pending pendingOAuthGrant
	if err := json.Unmarshal(encoded, &pending); err != nil {
		return pendingOAuthGrant{}, err
	}
	return pending, nil
}

func findLoginUserBySubject(state managedState, issuer, subject string) string {
	for id, user := range state.LoginUsers {
		if user.Issuer == issuer && user.Subject == subject {
			return id
		}
	}
	return ""
}

func countGrantsForUser(state managedState, userID string, now time.Time) int {
	count := 0
	for _, grant := range state.OAuthGrants {
		if grant.UserID == userID && grant.RevokedAt == nil && now.Before(grant.ExpiresAt) {
			count++
		}
	}
	return count
}

func agentNameForOAuth(clientName, displayName string) string {
	clean := func(value string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, value))
	}
	clientName = clean(clientName)
	displayName = clean(displayName)
	if clientName == "" {
		clientName = "OAuth"
	}
	name := clientName + " · " + displayName
	if len(name) > 128 {
		name = string([]rune(name)[:128])
	}
	return name
}

func firstAudience(groups ...[]string) string {
	for _, values := range groups {
		for _, value := range values {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func pruneOAuthCodes(state *managedState, now time.Time) {
	for signature, code := range state.OAuthCodes {
		if code.ConsumedAt != nil || !now.Before(code.ExpiresAt) {
			delete(state.OAuthCodes, signature)
		}
	}
}

func pruneOAuthCredentials(records map[string]oauthCredentialRecord, now time.Time) {
	for signature, record := range records {
		if record.RevokedAt != nil || !now.Before(record.ExpiresAt) {
			delete(records, signature)
		}
	}
}

func randomIDOrEmpty() string {
	id, err := randomSecret()
	if err != nil {
		return ""
	}
	return id
}

func ptrTime(value time.Time) *time.Time {
	return &value
}

func (m *Managed) oauthResource() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return resourceForState(m.state)
}

func resourceForState(state managedState) string {
	if state.OAuthServer.PublicURL == "" {
		return ""
	}
	return state.OAuthServer.PublicURL + "/stream"
}

func (m *Managed) cancelOAuthCredentials(records map[string]string) {
	for credentialID, principalID := range records {
		m.cancelCredentialLockedByID(principalID, credentialID)
	}
}

func (m *Managed) cancelCredentialLockedByID(principalID, credentialID string) {
	m.mu.Lock()
	for _, cancel := range m.requests["token:"+credentialID] {
		cancel()
	}
	for _, cancel := range m.background[credentialID] {
		cancel()
	}
	invalidate := m.mcpConnectionInvalidator
	m.mu.Unlock()
	if invalidate != nil {
		invalidate(principalID, credentialID)
	}
}

func revokeAllOAuth(state *managedState, now time.Time, reason string) ([]string, map[string]string) {
	active := map[string]string{}
	for id, grant := range state.OAuthGrants {
		if grant.RevokedAt == nil {
			grant.RevokedAt = &now
			grant.RevokeReason = reason
			state.OAuthGrants[id] = grant
		}
	}
	for signature, record := range state.OAuthAccess {
		if record.RevokedAt == nil {
			record.RevokedAt = &now
			state.OAuthAccess[signature] = record
			active[record.ID] = state.OAuthGrants[record.GrantID].AgentID
		}
	}
	for signature, record := range state.OAuthRefresh {
		if record.RevokedAt == nil {
			record.RevokedAt = &now
			state.OAuthRefresh[signature] = record
		}
	}
	for signature := range state.OAuthCodes {
		delete(state.OAuthCodes, signature)
	}
	state.OAuthRevision++
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, active
}
