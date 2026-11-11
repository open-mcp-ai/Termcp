package oauthserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
)

const (
	accessTokenPrefix   = "toa_"
	refreshTokenPrefix  = "tor_"
	authorizeCodePrefix = "toc_"
)

type tokenLifespans interface {
	GetAccessTokenLifespan(context.Context) time.Duration
	GetRefreshTokenLifespan(context.Context) time.Duration
	GetAuthorizeCodeLifespan(context.Context) time.Duration
}

// opaqueStrategy implements Fosite's strategy seam with random bearer values
// and SHA-256 lookup signatures. Raw credentials are returned once and only the
// signature reaches encrypted storage.
type opaqueStrategy struct {
	config tokenLifespans
}

var _ oauth2.CoreStrategy = (*opaqueStrategy)(nil)

func newOpaqueStrategy(config tokenLifespans) *opaqueStrategy {
	return &opaqueStrategy{config: config}
}

func (s *opaqueStrategy) AccessTokenSignature(_ context.Context, token string) string {
	return opaqueSignature(token)
}

func (s *opaqueStrategy) RefreshTokenSignature(_ context.Context, token string) string {
	return opaqueSignature(token)
}

func (s *opaqueStrategy) AuthorizeCodeSignature(_ context.Context, token string) string {
	return opaqueSignature(token)
}

func (s *opaqueStrategy) GenerateAccessToken(context.Context, fosite.Requester) (string, string, error) {
	return newOpaqueToken(accessTokenPrefix)
}

func (s *opaqueStrategy) GenerateRefreshToken(context.Context, fosite.Requester) (string, string, error) {
	return newOpaqueToken(refreshTokenPrefix)
}

func (s *opaqueStrategy) GenerateAuthorizeCode(context.Context, fosite.Requester) (string, string, error) {
	return newOpaqueToken(authorizeCodePrefix)
}

func (s *opaqueStrategy) ValidateAccessToken(ctx context.Context, request fosite.Requester, token string) error {
	return validateOpaqueToken(ctx, request, token, accessTokenPrefix, fosite.AccessToken, s.config.GetAccessTokenLifespan(ctx))
}

func (s *opaqueStrategy) ValidateRefreshToken(ctx context.Context, request fosite.Requester, token string) error {
	return validateOpaqueToken(ctx, request, token, refreshTokenPrefix, fosite.RefreshToken, s.config.GetRefreshTokenLifespan(ctx))
}

func (s *opaqueStrategy) ValidateAuthorizeCode(ctx context.Context, request fosite.Requester, token string) error {
	return validateOpaqueToken(ctx, request, token, authorizeCodePrefix, fosite.AuthorizeCode, s.config.GetAuthorizeCodeLifespan(ctx))
}

func newOpaqueToken(prefix string) (string, string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(random)
	return token, opaqueSignature(token), nil
}

func opaqueSignature(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TokenSignature returns the non-secret storage index for an opaque token.
func TokenSignature(token string) string {
	return opaqueSignature(token)
}

func validateOpaqueToken(ctx context.Context, request fosite.Requester, token, prefix string, kind fosite.TokenType, fallback time.Duration) error {
	if request == nil || !strings.HasPrefix(token, prefix) || len(token) < len(prefix)+32 {
		return fosite.ErrInvalidTokenFormat
	}
	session := request.GetSession()
	if session == nil {
		return errors.New("OAuth request has no session")
	}
	expiry := session.GetExpiresAt(kind)
	if expiry.IsZero() && fallback > 0 {
		expiry = request.GetRequestedAt().Add(fallback)
	}
	if !expiry.IsZero() && !time.Now().UTC().Before(expiry) {
		return fosite.ErrTokenExpired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
