package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	oauthKeyCacheTTL = 5 * time.Minute
	oauthHTTPTimeout = 10 * time.Second
)

// OAuthConfig describes an external OAuth 2.1/OIDC authorization server. Termcp
// is the protected resource server here; it only discovers and verifies tokens.
type OAuthConfig struct {
	Issuer     string `json:"issuer"`
	Resource   string `json:"resource"`
	JWKSURL    string `json:"jwks_url"`
	AgentClaim string `json:"agent_claim"`
	Scope      string `json:"scope"`
}

func (cfg OAuthConfig) enabled() bool {
	return strings.TrimSpace(cfg.Issuer) != "" ||
		strings.TrimSpace(cfg.Resource) != "" ||
		strings.TrimSpace(cfg.JWKSURL) != ""
}

func normalizeOAuthConfig(cfg OAuthConfig) (OAuthConfig, error) {
	cfg.Issuer = strings.TrimSpace(cfg.Issuer)
	cfg.Resource = strings.TrimRight(strings.TrimSpace(cfg.Resource), "/")
	cfg.JWKSURL = strings.TrimSpace(cfg.JWKSURL)
	cfg.AgentClaim = strings.TrimSpace(cfg.AgentClaim)
	cfg.Scope = strings.TrimSpace(cfg.Scope)
	if cfg.AgentClaim == "" {
		cfg.AgentClaim = "termcp_agent_id"
	}
	if !cfg.enabled() {
		return cfg, nil
	}
	if cfg.Issuer == "" || cfg.Resource == "" || cfg.JWKSURL == "" {
		return OAuthConfig{}, errors.New("oauth issuer, resource, and JWKS URL are required together")
	}
	for name, raw := range map[string]string{
		"oauth issuer":   cfg.Issuer,
		"oauth resource": cfg.Resource,
		"oauth JWKS URL": cfg.JWKSURL,
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return OAuthConfig{}, fmt.Errorf("%s must be an absolute http(s) URL", name)
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return OAuthConfig{}, fmt.Errorf("%s must not contain a query or fragment", name)
		}
		if u.Scheme == "http" {
			ip := net.ParseIP(u.Hostname())
			if !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
				return OAuthConfig{}, fmt.Errorf("%s must use https unless it is loopback", name)
			}
		}
	}
	return cfg, nil
}

type oauthVerifier struct {
	cfg    OAuthConfig
	client *http.Client

	mu      sync.RWMutex
	keys    map[string]oauthKey
	fetched time.Time
}

type oauthKey struct {
	publicKey crypto.PublicKey
	alg       string
}

type oauthIdentity struct {
	PrincipalID  string
	CredentialID string
	ExpiresAt    time.Time
}

type oauthSession struct {
	principal string
	expires   time.Time
}

// SetOAuthConfig persists and applies the external OAuth profile. The owner
// normally configures it through /oauth; this entrance also supports embedders.
func (m *Managed) SetOAuthConfig(cfg OAuthConfig) error {
	_, _, err := m.updateOAuthSettings(OAuthSettings{Enabled: cfg.enabled(), OAuthConfig: cfg}, nil)
	return err
}

// OAuthEnabled reports whether OAuth access tokens are accepted.
func (m *Managed) OAuthEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.oauth != nil
}

// ServeOAuthMetadata serves RFC 9728 protected-resource metadata. The root,
// path-specific, and reverse-proxy-prefixed well-known paths are accepted so
// Claude and Codex can discover the same resource through either probe order.
func (m *Managed) ServeOAuthMetadata(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/.well-known/oauth-protected-resource"
	index := strings.Index(r.URL.Path, prefix)
	if index < 0 {
		return false
	}
	if m == nil {
		return false
	}
	m.mu.Lock()
	verifier := m.oauth
	m.mu.Unlock()
	if verifier == nil {
		return false
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	resource := verifier.metadataResource(r.URL.Path[index:])
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":              resource,
		"authorization_servers": []string{verifier.cfg.Issuer},
		"scopes_supported":      oauthScopes(verifier.cfg.Scope),
		"bearer_methods_supported": []string{
			"header",
		},
	})
	return true
}

func oauthScopes(scope string) []string {
	if scope == "" {
		return []string{}
	}
	return strings.Fields(scope)
}

func (v *oauthVerifier) resourceFor(path string) string {
	base := strings.TrimRight(v.cfg.Resource, "/")
	if strings.HasSuffix(base, "/stream") || strings.HasSuffix(base, "/sse") {
		if path == "/stream" || path == "/sse" {
			return strings.TrimSuffix(strings.TrimSuffix(base, "/stream"), "/sse") + path
		}
		return base
	}
	if path == "/stream" || path == "/sse" {
		return base + path
	}
	return base
}

func (v *oauthVerifier) resourceForRequest(path string) string {
	return v.resourceFor(path)
}

func (v *oauthVerifier) metadataResource(path string) string {
	suffix := strings.TrimPrefix(path, "/.well-known/oauth-protected-resource")
	if strings.HasSuffix(suffix, "/stream") {
		return v.resourceFor("/stream")
	}
	if strings.HasSuffix(suffix, "/sse") {
		return v.resourceFor("/sse")
	}
	return v.resourceFor("")
}

func (v *oauthVerifier) metadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	u.Path = "/.well-known/oauth-protected-resource" + strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (m *Managed) oauthChallenge(r *http.Request, invalid bool) string {
	m.mu.Lock()
	verifier := m.oauth
	m.mu.Unlock()
	if verifier == nil {
		return ""
	}
	resource := verifier.resourceForRequest(r.URL.Path)
	metadata := verifier.metadataURL(resource)
	if metadata == "" {
		return ""
	}
	parts := []string{`Bearer resource_metadata=` + strconv.Quote(metadata)}
	if verifier.cfg.Scope != "" {
		parts = append(parts, "scope="+strconv.Quote(verifier.cfg.Scope))
	}
	if invalid {
		parts = append(parts, `error="invalid_token"`)
	}
	return strings.Join(parts, ", ")
}

func (m *Managed) admitOAuthToken(r *http.Request, raw string) (context.Context, context.CancelFunc, int) {
	m.mu.Lock()
	verifier := m.oauth
	locked := m.vault.AuthenticationLocked()
	m.mu.Unlock()
	if verifier == nil {
		return nil, nil, http.StatusUnauthorized
	}
	if locked {
		return nil, nil, http.StatusLocked
	}
	expectedResource := verifier.resourceForRequest(r.URL.Path)
	identity, err := verifier.verify(raw, expectedResource)
	if err != nil {
		return nil, nil, http.StatusUnauthorized
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Version == 0 || m.vault.AuthenticationLocked() {
		return nil, nil, http.StatusLocked
	}
	// Verification may fetch keys outside the lock. A saved configuration must
	// not admit a request which finished verifying against the previous issuer.
	if m.oauth != verifier {
		return nil, nil, http.StatusUnauthorized
	}
	agent, ok := m.state.Agents[identity.PrincipalID]
	if !ok || !agent.Enabled {
		return nil, nil, http.StatusUnauthorized
	}
	actor := Actor{ID: agent.ID, Username: agent.Name, Role: RoleAgent}
	if _, err := resolvedAgentPolicy(m.state, agent); err != nil {
		return nil, nil, http.StatusUnauthorized
	}
	if requestSurface(r) != "mcp" {
		return nil, nil, http.StatusForbidden
	}
	switch r.URL.Path {
	case "/stream", "/sse", "/message", "/auth/mcp":
	default:
		return nil, nil, http.StatusForbidden
	}
	permitted := false
	for _, surface := range m.state.Permissions[RoleAgent] {
		if surface == "mcp" {
			permitted = true
			break
		}
	}
	if !permitted {
		return nil, nil, http.StatusForbidden
	}
	now := m.now().UTC()
	if !identity.ExpiresAt.After(now) {
		return nil, nil, http.StatusUnauthorized
	}
	if m.oauthSessions == nil {
		m.oauthSessions = make(map[string]oauthSession)
	}
	for id, session := range m.oauthSessions {
		if !session.expires.After(now) {
			m.cancelOAuthSessionLocked(id)
		}
	}
	m.oauthSessions[identity.CredentialID] = oauthSession{
		principal: identity.PrincipalID,
		expires:   identity.ExpiresAt,
	}
	baseCtx, baseCancel := context.WithCancel(r.Context())
	if identity.ExpiresAt.After(now) {
		baseCtx, baseCancel = context.WithDeadline(baseCtx, identity.ExpiresAt)
	}
	ctx := baseCtx
	ctx = m.withOAuthAgentPolicy(ctx, actor.ID, identity.CredentialID)
	ctx = m.withOAuthBackgroundWork(ctx, actor.ID, identity.CredentialID, identity.ExpiresAt)
	ctx = context.WithValue(ctx, actorKey{}, actor)
	ctx = context.WithValue(ctx, credentialKey{}, "token:"+identity.CredentialID)
	ctx = withOAuthCredential(ctx)
	ref := new(int)
	key := "token:" + identity.CredentialID
	if m.requests[key] == nil {
		m.requests[key] = make(map[*int]context.CancelFunc)
	}
	m.requests[key][ref] = baseCancel
	done := func() {
		baseCancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.requests[key], ref)
		if len(m.requests[key]) == 0 {
			delete(m.requests, key)
		}
	}
	return ctx, done, 0
}

func (m *Managed) cancelOAuthSessionLocked(id string) {
	if _, ok := m.oauthSessions[id]; !ok {
		return
	}
	delete(m.oauthSessions, id)
	m.cancelCredentialLocked(id)
}

func (m *Managed) withOAuthAgentPolicy(ctx context.Context, principal, credential string) context.Context {
	return context.WithValue(ctx, agentPolicyKey{}, policyResolver(func() (AgentPolicy, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		a, ok := m.state.Agents[principal]
		if !ok || !a.Enabled {
			return AgentPolicy{}, errors.New("Agent access has ended")
		}
		if session, exists := m.oauthSessions[credential]; !exists || session.principal != principal || !session.expires.After(m.now().UTC()) {
			return AgentPolicy{}, errors.New("OAuth credential has ended")
		}
		permitted := false
		for _, surface := range m.state.Permissions[RoleAgent] {
			if surface == "mcp" {
				permitted = true
				break
			}
		}
		if !permitted {
			return AgentPolicy{}, errors.New("Agent MCP access is disabled")
		}
		return resolvedAgentPolicy(m.state, a)
	}))
}

func (m *Managed) withOAuthBackgroundWork(ctx context.Context, principal, credential string, expires time.Time) context.Context {
	return context.WithValue(ctx, backgroundWorkKey{}, backgroundRegistrar(func(message context.Context, cancel context.CancelFunc) (func(), error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := message.Err(); err != nil {
			return nil, err
		}
		a, ok := m.state.Agents[principal]
		session, exists := m.oauthSessions[credential]
		if !ok || !a.Enabled || !exists || session.principal != principal || !expires.After(m.now().UTC()) {
			return nil, errors.New("credential access has ended")
		}
		if _, err := resolvedAgentPolicy(m.state, a); err != nil {
			return nil, err
		}
		permitted := false
		for _, surface := range m.state.Permissions[RoleAgent] {
			if surface == "mcp" {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, errors.New("Agent MCP access has ended")
		}
		ref := new(int)
		if m.background[credential] == nil {
			m.background[credential] = make(map[*int]context.CancelFunc)
		}
		m.background[credential][ref] = cancel
		return func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.background[credential], ref)
			if len(m.background[credential]) == 0 {
				delete(m.background, credential)
			}
		}, nil
	}))
}

func (v *oauthVerifier) verify(raw, expectedResource string) (oauthIdentity, error) {
	if raw == "" {
		return oauthIdentity{}, errors.New("empty OAuth token")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		alg := token.Method.Alg()
		if !oauthAlgorithmAllowed(alg) {
			return nil, fmt.Errorf("OAuth algorithm %q is not allowed", alg)
		}
		kid, _ := token.Header["kid"].(string)
		return v.key(kid, alg)
	},
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil || token == nil || !token.Valid {
		if err == nil {
			err = errors.New("invalid OAuth token")
		}
		return oauthIdentity{}, err
	}
	principal, _ := claims[v.cfg.AgentClaim].(string)
	if strings.TrimSpace(principal) == "" {
		return oauthIdentity{}, fmt.Errorf("OAuth claim %q is missing", v.cfg.AgentClaim)
	}
	if !oauthResourceMatches(claims, expectedResource, v.cfg.Resource) {
		return oauthIdentity{}, errors.New("OAuth audience/resource does not match this MCP endpoint")
	}
	if v.cfg.Scope != "" && !oauthHasScope(claims, v.cfg.Scope) {
		return oauthIdentity{}, errors.New("OAuth scope is missing")
	}
	expires, err := oauthExpiry(claims)
	if err != nil {
		return oauthIdentity{}, err
	}
	sum := sha256.Sum256([]byte(raw))
	return oauthIdentity{
		PrincipalID:  principal,
		CredentialID: "oauth-" + hex.EncodeToString(sum[:]),
		ExpiresAt:    expires,
	}, nil
}

func (v *oauthVerifier) key(kid, alg string) (crypto.PublicKey, error) {
	v.mu.RLock()
	if v.keys != nil && time.Since(v.fetched) < oauthKeyCacheTTL {
		if key, ok := v.lookupLocked(kid); ok {
			v.mu.RUnlock()
			return key, nil
		}
	}
	v.mu.RUnlock()

	keys, err := v.fetchKeys()
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	key, ok := v.lookupLocked(kid)
	v.mu.Unlock()
	if ok {
		return key, nil
	}
	return nil, fmt.Errorf("OAuth signing key %q was not found", kid)
}

func (v *oauthVerifier) lookupLocked(kid string) (crypto.PublicKey, bool) {
	if kid == "" {
		if len(v.keys) == 1 {
			for _, key := range v.keys {
				return key.publicKey, true
			}
		}
		return nil, false
	}
	key, ok := v.keys[kid]
	if !ok {
		return nil, false
	}
	return key.publicKey, true
}

func (v *oauthVerifier) fetchKeys() (map[string]oauthKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), oauthHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OAuth JWKS returned HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]oauthKey)
	for _, raw := range set.Keys {
		key, err := parseJSONWebKey(raw)
		if err != nil {
			continue
		}
		keys[raw.Kid] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("OAuth JWKS contained no usable signing keys")
	}
	return keys, nil
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func parseJSONWebKey(raw jsonWebKey) (oauthKey, error) {
	if raw.Use != "" && raw.Use != "sig" {
		return oauthKey{}, errors.New("JWK is not a signing key")
	}
	decode := func(value string) ([]byte, error) {
		return base64.RawURLEncoding.DecodeString(value)
	}
	switch raw.Kty {
	case "RSA":
		nBytes, err := decode(raw.N)
		if err != nil || len(nBytes) == 0 {
			return oauthKey{}, errors.New("invalid RSA modulus")
		}
		eBytes, err := decode(raw.E)
		if err != nil || len(eBytes) == 0 {
			return oauthKey{}, errors.New("invalid RSA exponent")
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		if e < 3 {
			return oauthKey{}, errors.New("invalid RSA exponent")
		}
		return oauthKey{publicKey: &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, alg: raw.Alg}, nil
	case "EC":
		var curve elliptic.Curve
		switch raw.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return oauthKey{}, errors.New("unsupported EC curve")
		}
		x, errX := decode(raw.X)
		y, errY := decode(raw.Y)
		if errX != nil || errY != nil {
			return oauthKey{}, errors.New("invalid EC coordinates")
		}
		return oauthKey{publicKey: &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, alg: raw.Alg}, nil
	case "OKP":
		if raw.Crv != "Ed25519" {
			return oauthKey{}, errors.New("unsupported OKP curve")
		}
		x, err := decode(raw.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return oauthKey{}, errors.New("invalid Ed25519 key")
		}
		return oauthKey{publicKey: ed25519.PublicKey(x), alg: raw.Alg}, nil
	default:
		return oauthKey{}, errors.New("unsupported JWK type")
	}
}

func oauthAlgorithmAllowed(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA":
		return true
	default:
		return false
	}
}

func oauthResourceMatches(claims jwt.MapClaims, expected, base string) bool {
	for _, claimName := range []string{"resource", "aud"} {
		for _, value := range oauthClaimStrings(claims[claimName]) {
			if value == expected || value == base {
				return true
			}
		}
	}
	return false
}

func oauthHasScope(claims jwt.MapClaims, wanted string) bool {
	tokenScopes := map[string]bool{}
	for _, scope := range strings.Fields(oauthClaimString(claims["scope"])) {
		tokenScopes[scope] = true
	}
	for _, scope := range oauthClaimStrings(claims["scp"]) {
		tokenScopes[scope] = true
	}
	wantedScopes := strings.Fields(wanted)
	if len(wantedScopes) == 0 {
		return true
	}
	for _, scope := range wantedScopes {
		if !tokenScopes[scope] {
			return false
		}
	}
	return true
}

func oauthClaimString(value any) string {
	text, _ := value.(string)
	return text
}

func oauthClaimStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func oauthExpiry(claims jwt.MapClaims) (time.Time, error) {
	switch value := claims["exp"].(type) {
	case float64:
		return time.Unix(int64(value), 0).UTC(), nil
	case json.Number:
		seconds, err := value.Int64()
		if err != nil {
			return time.Time{}, errors.New("invalid OAuth expiry")
		}
		return time.Unix(seconds, 0).UTC(), nil
	default:
		return time.Time{}, errors.New("OAuth expiry is missing")
	}
}
