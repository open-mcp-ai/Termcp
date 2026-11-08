package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type oauthTestKey struct {
	private *rsa.PrivateKey
	kid     string
}

func TestManagedOAuthResourceServer(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	create := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/auth/principals",
		`{"name":"oauth-agent"}`, cookie, session.CSRF)
	if create.Code != http.StatusOK {
		t.Fatalf("create Agent: %d %s", create.Code, create.Body)
	}
	var created struct {
		Principal agentRecord `json:"principal"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	first, err := newOAuthTestKey("key-1")
	if err != nil {
		t.Fatal(err)
	}
	current := first
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwksFor(current)}})
	}))
	t.Cleanup(jwks.Close)

	const issuer = "https://issuer.example.test"
	const resource = "https://termcp.example.test"
	if err := m.SetOAuthConfig(OAuthConfig{
		Issuer: issuer, Resource: resource, JWKSURL: jwks.URL,
		AgentClaim: "termcp_agent_id", Scope: "mcp",
	}); err != nil {
		t.Fatal(err)
	}

	var actorID string
	var policy AgentPolicy
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFromContext(r.Context())
		if !ok {
			t.Error("OAuth request has no trusted actor")
		}
		actorID = actor.ID
		policy, _, _ = PolicyFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	metadata := httptest.NewRecorder()
	h.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "https://termcp.example.test/.well-known/oauth-protected-resource/stream", nil))
	if metadata.Code != http.StatusOK {
		t.Fatalf("metadata status: %d", metadata.Code)
	}
	var discovery map[string]any
	if err := json.Unmarshal(metadata.Body.Bytes(), &discovery); err != nil {
		t.Fatal(err)
	}
	if discovery["resource"] != resource+"/stream" || discovery["authorization_servers"].([]any)[0] != issuer {
		t.Fatalf("unexpected protected-resource metadata: %#v", discovery)
	}

	anonymous := httptest.NewRecorder()
	h.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, "https://termcp.example.test/stream", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous MCP status = %d, want 401", anonymous.Code)
	}
	if challenge := anonymous.Header().Get("WWW-Authenticate"); !strings.Contains(challenge, "resource_metadata") {
		t.Fatalf("OAuth challenge missing: %q", challenge)
	}

	valid := signedOAuthToken(t, first, issuer, resource+"/stream", created.Principal.ID, "mcp", time.Now().Add(time.Hour))
	if w := bearerRequest(h, http.MethodPost, "/stream", valid, ""); w.Code != http.StatusNoContent {
		t.Fatalf("valid OAuth token: %d %s", w.Code, w.Body)
	}
	if actorID != created.Principal.ID || policy.SchemaVersion != 2 {
		t.Fatalf("OAuth actor/policy not applied: actor=%q policy=%+v", actorID, policy)
	}

	// OAuth credentials must not become REST or WebUI authority. The token is
	// bound to /stream, so an /api request is rejected before the surface check.
	if w := bearerRequest(h, http.MethodGet, "/api/sessions", valid, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("OAuth token escaped MCP surface: %d", w.Code)
	}

	for name, token := range map[string]string{
		"wrong audience": signedOAuthToken(t, first, issuer, resource+"/sse", created.Principal.ID, "mcp", time.Now().Add(time.Hour)),
		"wrong issuer":   signedOAuthToken(t, first, "https://other.example.test", resource+"/stream", created.Principal.ID, "mcp", time.Now().Add(time.Hour)),
		"wrong Agent":    signedOAuthToken(t, first, issuer, resource+"/stream", "other-agent", "mcp", time.Now().Add(time.Hour)),
		"missing scope":  signedOAuthToken(t, first, issuer, resource+"/stream", created.Principal.ID, "", time.Now().Add(time.Hour)),
		"expired":        signedOAuthToken(t, first, issuer, resource+"/stream", created.Principal.ID, "mcp", time.Now().Add(-time.Minute)),
	} {
		if w := bearerRequest(h, http.MethodPost, "/stream", token, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", name, w.Code)
		}
	}

	// A key rotation with a new kid triggers a fresh JWKS read.
	second, err := newOAuthTestKey("key-2")
	if err != nil {
		t.Fatal(err)
	}
	current = second
	rotated := signedOAuthToken(t, second, issuer, resource+"/stream", created.Principal.ID, "mcp", time.Now().Add(time.Hour))
	if w := bearerRequest(h, http.MethodPost, "/stream", rotated, ""); w.Code != http.StatusNoContent {
		t.Fatalf("rotated JWKS key: %d %s", w.Code, w.Body)
	}
}

func newOAuthTestKey(kid string) (oauthTestKey, error) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return oauthTestKey{}, err
	}
	return oauthTestKey{private: private, kid: kid}, nil
}

func jwksFor(key oauthTestKey) map[string]any {
	exponent := big.NewInt(int64(key.private.PublicKey.E)).Bytes()
	return map[string]any{
		"kty": "RSA",
		"kid": key.kid,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.private.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(exponent),
	}
}

func signedOAuthToken(t *testing.T, key oauthTestKey, issuer, resource, agentID, scope string, expiry time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":             issuer,
		"aud":             resource,
		"resource":        resource,
		"termcp_agent_id": agentID,
		"exp":             expiry.Unix(),
		"iat":             time.Now().Add(-time.Minute).Unix(),
	}
	if scope != "" {
		claims["scope"] = scope
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = key.kid
	signed, err := token.SignedString(key.private)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
