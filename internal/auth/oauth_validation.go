package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

func validateOAuthServerState(s managedState) error {
	settings, err := normalizeOAuthServerSettings(s.OAuthServer)
	if err != nil {
		return err
	}
	if settings != s.OAuthServer {
		return errors.New("OAuth server settings are not normalized")
	}
	if settings.Enabled && (settings.PublicURL == "" || settings.OIDCIssuer == "" || settings.OIDCClientID == "") {
		return errors.New("enabled OAuth server is incomplete")
	}
	if s.OAuthRevision == 0 ||
		len(s.LoginUsers) > maxLoginUsers ||
		len(s.OAuthClients) > maxOAuthClients ||
		len(s.OAuthGrants) > maxOAuthGrants ||
		len(s.OAuthCodes) > maxOAuthCodes ||
		len(s.OAuthAccess) > maxOAuthAccess ||
		len(s.OAuthRefresh) > maxOAuthRefresh {
		return errors.New("invalid OAuth state limits or revision")
	}

	identities := map[string]string{}
	for id, user := range s.LoginUsers {
		if id == "" || id != user.ID || user.Issuer == "" || user.Subject == "" ||
			validLabel(user.DisplayName) == false || user.Revision == 0 || user.CreatedAt.IsZero() || user.UpdatedAt.IsZero() {
			return errors.New("invalid saved login user")
		}
		key := user.Issuer + "\x00" + user.Subject
		if previous := identities[key]; previous != "" && previous != id {
			return errors.New("duplicate login identity")
		}
		identities[key] = id
		if user.AvatarFile != "" && filepath.Base(user.AvatarFile) != user.AvatarFile {
			return errors.New("invalid saved avatar reference")
		}
	}
	for id, client := range s.OAuthClients {
		if id == "" || id != client.ID || !client.Enabled || client.CreatedAt.IsZero() ||
			len(client.Name) > 256 || len(client.URI) > 2048 || len(client.LogoURI) > 2048 ||
			len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 16 {
			return errors.New("invalid saved OAuth client")
		}
		for _, redirect := range client.RedirectURIs {
			if !oauthserver.ValidRedirect(redirect) {
				return errors.New("invalid saved OAuth redirect")
			}
		}
		switch client.Registration {
		case oauthserver.ClientRegistrationDCR:
		case oauthserver.ClientRegistrationCIMD:
			if !strings.HasPrefix(client.ID, "https://") {
				return errors.New("invalid saved CIMD client")
			}
		default:
			return errors.New("invalid OAuth client registration")
		}
	}
	for id, grant := range s.OAuthGrants {
		user, userOK := s.LoginUsers[grant.UserID]
		agent, agentOK := s.Agents[grant.AgentID]
		client, clientOK := s.OAuthClients[grant.ClientID]
		if id == "" || id != grant.ID || !userOK || !agentOK || !clientOK ||
			agent.OwnerUserID != user.ID || agent.Origin != agentOriginOAuth ||
			grant.ClientID != client.ID || grant.Resource == "" || grant.CreatedAt.IsZero() ||
			!grant.ExpiresAt.After(grant.CreatedAt) || grant.ConfigRevision > s.OAuthServer.Revision {
			return errors.New("invalid saved OAuth grant")
		}
		if grant.RevokedAt != nil && grant.RevokedAt.Before(grant.CreatedAt) {
			return errors.New("invalid saved OAuth grant revocation")
		}
	}
	for signature, code := range s.OAuthCodes {
		if !validOAuthSignature(signature) || code.Request.ID == "" || code.ExpiresAt.IsZero() ||
			code.Request.ClientID == "" || code.Request.SessionIdentityMissing() {
			return errors.New("invalid saved OAuth authorization code")
		}
	}
	credentialIDs := map[string]bool{}
	for signature, credential := range s.OAuthAccess {
		if err := validateOAuthCredential(s, signature, credential); err != nil {
			return err
		}
		if credentialIDs[credential.ID] {
			return errors.New("duplicate saved OAuth credential ID")
		}
		credentialIDs[credential.ID] = true
	}
	for signature, credential := range s.OAuthRefresh {
		if err := validateOAuthCredential(s, signature, credential); err != nil {
			return err
		}
		if credentialIDs[credential.ID] {
			return errors.New("duplicate saved OAuth credential ID")
		}
		credentialIDs[credential.ID] = true
	}
	return nil
}

func (r oauthStoredRequest) SessionIdentityMissing() bool {
	if r.Session.Extra == nil {
		return true
	}
	_, ok := r.Session.Extra["termcp_pending_grant"]
	return !ok
}

func validateOAuthCredential(s managedState, signature string, credential oauthCredentialRecord) error {
	grant, ok := s.OAuthGrants[credential.GrantID]
	if !validOAuthSignature(signature) || !ok || credential.ID == "" ||
		credential.ClientID != grant.ClientID || credential.Resource == "" ||
		credential.Request.ID != grant.ID || credential.CreatedAt.IsZero() ||
		!credential.ExpiresAt.After(credential.CreatedAt) ||
		credential.RevokedAt != nil && credential.RevokedAt.Before(credential.CreatedAt) {
		return fmt.Errorf("invalid saved OAuth credential: grant=%q client=%q/%q resource=%q request=%q/%q created=%s expires=%s",
			credential.GrantID, credential.ClientID, grant.ClientID, credential.Resource,
			credential.Request.ID, grant.ID, credential.CreatedAt, credential.ExpiresAt)
	}
	return nil
}

func validOAuthSignature(signature string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	return err == nil && len(raw) == 32
}

func grantActive(grant oauthGrant, now time.Time) bool {
	return grant.RevokedAt == nil && now.Before(grant.ExpiresAt)
}
