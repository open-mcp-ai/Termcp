package oauthserver

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const oidcTimeout = 12 * time.Second

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type Identity struct {
	Issuer           string
	Subject          string
	DisplayName      string
	Username         string
	AvatarURL        string
	ProfileUpdatedAt time.Time
}

type OIDC struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	config   oauth2.Config
}

func NewOIDC(ctx context.Context, config OIDCConfig) (*OIDC, error) {
	config.Issuer = strings.TrimSpace(config.Issuer)
	if config.Issuer == "" || config.ClientID == "" || config.RedirectURL == "" {
		return nil, errors.New("OIDC issuer, client ID, and callback URL are required")
	}
	ctx, cancel := context.WithTimeout(ctx, oidcTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, config.Issuer)
	if err != nil {
		return nil, err
	}
	return &OIDC{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: config.ClientID}),
		config: oauth2.Config{
			ClientID:     config.ClientID,
			ClientSecret: config.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  config.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile"},
		},
	}, nil
}

func (o *OIDC) AuthCodeURL(state, nonce, verifier string) string {
	return o.config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (o *OIDC) Exchange(ctx context.Context, code, verifier, expectedNonce string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, oidcTimeout)
	defer cancel()
	token, err := o.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, err
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return Identity{}, errors.New("OIDC response did not include an ID token")
	}
	idToken, err := o.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Identity{}, err
	}
	var idClaims struct {
		Nonce             string `json:"nonce"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Picture           string `json:"picture"`
	}
	if err := idToken.Claims(&idClaims); err != nil {
		return Identity{}, err
	}
	if idClaims.Nonce == "" || idClaims.Nonce != expectedNonce {
		return Identity{}, errors.New("OIDC nonce does not match this login")
	}
	if idToken.Subject == "" {
		return Identity{}, errors.New("OIDC subject is empty")
	}

	identity := Identity{
		Issuer:      trimURL(idToken.Issuer),
		Subject:     idToken.Subject,
		DisplayName: strings.TrimSpace(idClaims.Name),
		Username:    strings.TrimSpace(idClaims.PreferredUsername),
		AvatarURL:   strings.TrimSpace(idClaims.Picture),
	}
	if endpoint := o.provider.UserInfoEndpoint(); endpoint != "" {
		userInfo, err := o.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
		if err != nil {
			return Identity{}, err
		}
		if userInfo.Subject != idToken.Subject {
			return Identity{}, errors.New("OIDC UserInfo subject does not match the verified ID token")
		}
		var claims struct {
			Name              string `json:"name"`
			PreferredUsername string `json:"preferred_username"`
			Picture           string `json:"picture"`
		}
		if err := userInfo.Claims(&claims); err != nil {
			return Identity{}, err
		}
		if strings.TrimSpace(claims.Name) != "" {
			identity.DisplayName = strings.TrimSpace(claims.Name)
		}
		if strings.TrimSpace(claims.PreferredUsername) != "" {
			identity.Username = strings.TrimSpace(claims.PreferredUsername)
		}
		if strings.TrimSpace(claims.Picture) != "" {
			identity.AvatarURL = strings.TrimSpace(claims.Picture)
		}
	}
	if identity.DisplayName == "" {
		identity.DisplayName = identity.Username
	}
	if identity.DisplayName == "" {
		identity.DisplayName = "Termcp user"
	}
	identity.ProfileUpdatedAt = time.Now().UTC()
	identity.DisplayName = cleanClaim(identity.DisplayName, 128)
	identity.Username = cleanClaim(identity.Username, 128)
	identity.AvatarURL = cleanClaim(identity.AvatarURL, 2048)
	if identity.DisplayName == "" {
		identity.DisplayName = "Termcp user"
	}
	return identity, nil
}

func cleanClaim(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value))
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}
