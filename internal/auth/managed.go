package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/open-mcp-ai/termcp/internal/configstore"
)

type Role string

const (
	RoleAdmin       Role = "admin"
	RoleAgent       Role = "agent"
	SessionCookie        = "termcp_session"
	SessionLifetime      = 24 * time.Hour
)

// Actor is constructed from verified credentials, never from request arguments.
type Actor struct {
	ID       string `json:"id"`
	Username string `json:"username,omitempty"`
	Role     Role   `json:"role"`
}

type actorKey struct{}
type credentialKey struct{}

// CredentialIDFromContext identifies a verified credential without exposing its secret.
func CredentialIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(credentialKey{}).(string)
	return id
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// RolePolicy is a coarse entrance policy, separate from per-operation policies.
// Agents authenticate with MCP-only bearer tokens; no Agent login is exposed here.
type RolePolicy map[Role][]string

func defaultPolicy() RolePolicy {
	return RolePolicy{RoleAdmin: {"web", "api", "mcp", "daemon"}, RoleAgent: {"mcp"}}
}

func validatePolicy(p RolePolicy) error {
	if len(p) != 2 {
		return errors.New("both preset roles are required")
	}
	for role, surfaces := range p {
		seen := map[string]bool{}
		for _, surface := range surfaces {
			if seen[surface] {
				return errors.New("duplicate permission")
			}
			seen[surface] = true
			if role == RoleAgent && surface != "mcp" {
				return errors.New("agent permissions are limited to MCP")
			}
			if surface != "web" && surface != "api" && surface != "mcp" && surface != "daemon" {
				return errors.New("unknown permission")
			}
		}
		switch role {
		case RoleAdmin:
			if !seen["web"] || !seen["api"] {
				return errors.New("admin must retain web and API access")
			}
		case RoleAgent:
		default:
			return errors.New("unknown role")
		}
	}
	return nil
}

type adminRecord struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	// Read only for migration from v1; absent from encrypted v2 state.
	Password string `json:"password,omitempty"`
}

type sessionRecord struct {
	Actor     Actor     `json:"actor"`
	ExpiresAt time.Time `json:"expires_at"`
	CSRF      string    `json:"csrf"`
}

type managedState struct {
	Version        int                        `json:"version"`
	InstanceID     string                     `json:"instance_id"`
	SigningKey     string                     `json:"signing_key"`
	Admin          adminRecord                `json:"admin"`
	Permissions    RolePolicy                 `json:"permissions"`
	Revision       uint64                     `json:"revision"`
	Sessions       map[string]sessionRecord   `json:"sessions"`
	Agents         map[string]agentRecord     `json:"agents,omitempty"`
	Tokens         map[string]tokenRecord     `json:"tokens,omitempty"`
	Groups         map[string]permissionGroup `json:"groups,omitempty"`
	DefaultGroupID string                     `json:"default_group_id,omitempty"`
	AccessRevision uint64                     `json:"access_revision,omitempty"`
}

type sessionClaims struct {
	Role      Role   `json:"role"`
	WebBootID string `json:"web_boot_id"`
	jwt.RegisteredClaims
}

// Managed owns accounts and MCP credentials encrypted by the shared vault.
// Browser logins are memory-only and bound to this run. No OAuth is implemented.
type Managed struct {
	// MCPIdleTimeout is set by the server before serving requests.
	MCPIdleTimeout time.Duration
	HideInternal   bool
	pageAssets     *pageAssets
	mu             sync.Mutex
	loginGate      sync.Mutex
	passwordMu     sync.Mutex // serializes password work without blocking ordinary authentication
	path           string
	state          managedState
	now            func() time.Time
	background     map[string]map[*int]context.CancelFunc
	requests       map[string]map[*int]context.CancelFunc
	attempts       int
	window         time.Time
	vault          Vault
	providers      []configstore.Provider
	sealed         bool
	legacy         *managedState
	markerDirty    bool
	webBootID      string // memory-only; vault unlock does not restore browser identity
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func equalSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func cloneState(s managedState) managedState {
	s.Sessions = make(map[string]sessionRecord, len(s.Sessions))
	return s
}

func (m *Managed) login(username, password string) (string, sessionRecord, error) {
	return m.loginWithOptions(username, password, "", false)
}

func (m *Managed) loginWithOptions(username, password, previous string, automatic bool) (string, sessionRecord, error) {
	m.passwordMu.Lock()
	defer m.passwordMu.Unlock()
	m.mu.Lock()
	configured := m.sealed || m.legacy != nil
	m.mu.Unlock()
	if !configured {
		return "", sessionRecord{}, errors.New("set up the admin account first")
	}
	if err := m.unlockForLogin(username, password, previous, automatic); err != nil {
		return "", sessionRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := cloneState(m.state)
	for id, session := range m.state.Sessions {
		if now.Before(session.ExpiresAt) {
			s.Sessions[id] = session
		}
	}
	if len(s.Sessions) >= 128 {
		return "", sessionRecord{}, errors.New("session limit reached; sign out of another session")
	}
	id, err := randomSecret()
	if err != nil {
		return "", sessionRecord{}, err
	}
	csrf, err := randomSecret()
	if err != nil {
		return "", sessionRecord{}, err
	}
	session := sessionRecord{Actor: Actor{ID: s.Admin.ID, Username: username, Role: RoleAdmin}, ExpiresAt: now.Add(SessionLifetime).Truncate(time.Second), CSRF: csrf}
	claims := sessionClaims{Role: RoleAdmin, WebBootID: m.webBootID, RegisteredClaims: jwt.RegisteredClaims{Issuer: s.InstanceID, Subject: session.Actor.ID, Audience: jwt.ClaimStrings{"termcp-web"}, ID: id, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(session.ExpiresAt)}}
	key, _ := base64.RawURLEncoding.DecodeString(s.SigningKey)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		return "", sessionRecord{}, err
	}
	s.Sessions[id] = session
	m.state.Sessions = s.Sessions
	return token, session, nil
}

func (m *Managed) verify(raw string) (string, sessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Version == 0 || len(raw) > 8192 {
		return "", sessionRecord{}, errors.New("invalid session")
	}
	key, _ := base64.RawURLEncoding.DecodeString(m.state.SigningKey)
	var claims sessionClaims
	token, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return key, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(m.state.InstanceID), jwt.WithAudience("termcp-web"), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(m.now))
	if err != nil || !token.Valid || claims.WebBootID == "" || !equalSecret(claims.WebBootID, m.webBootID) {
		return "", sessionRecord{}, errors.New("invalid session")
	}
	s, ok := m.state.Sessions[claims.ID]
	if !ok || claims.Role != s.Actor.Role || claims.Subject != s.Actor.ID || claims.ExpiresAt == nil || !claims.ExpiresAt.Time.Equal(s.ExpiresAt) || !m.now().Before(s.ExpiresAt) {
		return "", sessionRecord{}, errors.New("invalid session")
	}
	return claims.ID, s, nil
}

// revoke removes an in-memory browser login even if the vault is read-only.
func (m *Managed) revoke(id string) error {
	m.mu.Lock()
	delete(m.state.Sessions, id)
	var cancels []context.CancelFunc
	for _, cancel := range m.requests[id] {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}

func (m *Managed) track(ctx context.Context, id string, s sessionRecord) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(ctx, s.ExpiresAt)
	ref := new(int)
	m.mu.Lock()
	if _, ok := m.state.Sessions[id]; !ok {
		cancel()
	} else {
		if m.requests[id] == nil {
			m.requests[id] = make(map[*int]context.CancelFunc)
		}
		m.requests[id][ref] = cancel
	}
	m.mu.Unlock()
	ctx = context.WithValue(ctx, credentialKey{}, id)
	return context.WithValue(ctx, actorKey{}, s.Actor), func() {
		cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.requests[id], ref)
		if len(m.requests[id]) == 0 {
			delete(m.requests, id)
		}
	}
}

func (m *Managed) allows(role Role, surface string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, allowed := range m.state.Permissions[role] {
		if allowed == surface {
			return true
		}
	}
	return false
}

func decodeJSON(r io.Reader, dst any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errors.New("invalid request")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid request")
	}
	return nil
}
