// Package auth implements termcp's single-token HTTP authentication.
//
// The server is configured with either a plaintext token (--auth-token /
// TERMCP_AUTH_TOKEN) or a salted SHA-256 hash of one (--auth-hash /
// TERMCP_AUTH_HASH), so deployments that prefer not to store the cleartext
// can keep only the hash — and a hash-configured server additionally accepts
// that hash string itself as a credential, so an operator holding only the
// hash can drive `termcp daemon` management and `termcp stdio`. One token
// guards the whole HTTP surface: Web UI,
// REST API, MCP SSE, MCP streamable HTTP, and the WebSocket — except for the
// read-only documentation endpoints in publicPaths, which carry no data and no
// credentials, and must be fetchable before a client has a token configured
// (that is the whole point of serving the API reference and the curl skill).
//
// Credentials are accepted in three shapes:
//
//   - Authorization: Bearer <token>            (MCP clients, curl, scripts)
//   - Authorization: Basic base64(<any>:<token>) (browser native prompt;
//     the username carries no meaning, the password is the token. If the token
//     itself contains a colon, clients that split on the first colon — e.g.
//     `curl -u user:pass` — still authenticate, because the entire decoded
//     "user:pass" string is also accepted verbatim when it equals the token)
//   - Cookie termcp_token=<token>              (same-origin browser
//     WebSocket handshakes, which cannot set an Authorization header)
//
// The cookie is set automatically after a successful Basic authentication.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CookieName is set (HttpOnly) after successful HTTP Basic authentication so
// same-origin browser WebSocket handshakes, which cannot carry an
// Authorization header, are authenticated too.
const CookieName = "termcp_token"

const hashPrefix = "sha256"

// Hash generates a salted SHA-256 verifier string for token:
//
//	sha256-<salt_hex>-<digest_hex>   where digest = SHA256(salt || token)
//
// The salt is random and not secret; only the digest needs protecting.
// Separators are hyphens so the value contains no shell-active characters
// (a "$" would be expansion-prone when pasted into a shell command line).
func Hash(token string) (string, error) {
	if token == "" {
		return "", errors.New("token must not be empty")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	digest := digestOf(salt, token)
	return hashPrefix + "-" + hex.EncodeToString(salt) + "-" + hex.EncodeToString(digest), nil
}

func digestOf(salt []byte, token string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(token))
	return h.Sum(nil)
}

func parseHash(s string) (salt, digest []byte, err error) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 || parts[0] != hashPrefix {
		return nil, nil, fmt.Errorf("malformed hash: want %s-<salt_hex>-<digest_hex>", hashPrefix)
	}
	salt, err = hex.DecodeString(parts[1])
	if err != nil || len(salt) == 0 {
		return nil, nil, errors.New("malformed hash: salt must be non-empty hex")
	}
	digest, err = hex.DecodeString(parts[2])
	if err != nil || len(digest) != sha256.Size {
		return nil, nil, errors.New("malformed hash: digest must be 64 hex characters (SHA-256)")
	}
	return salt, digest, nil
}

// Verifier checks candidate tokens against a configured plaintext token or
// salted hash in constant time. Exactly one of the two must be set.
type Verifier struct {
	token  string
	salt   []byte
	digest []byte
	hash   string
}

// NewVerifier builds a Verifier from a plaintext token and/or a hash string.
// Supplying both is an error; supplying neither is an error.
func NewVerifier(token, hash string) (*Verifier, error) {
	if token != "" && hash != "" {
		return nil, errors.New("auth token and auth hash are mutually exclusive")
	}
	if token == "" && strings.TrimSpace(hash) == "" {
		return nil, errors.New("no auth token or auth hash configured")
	}
	v := &Verifier{token: token}
	if hash != "" {
		hash = strings.TrimSpace(hash)
		salt, digest, err := parseHash(hash)
		if err != nil {
			return nil, err
		}
		v.salt, v.digest, v.hash = salt, digest, hash
	}
	return v, nil
}

// Verify reports whether token matches the configured secret. A hash-configured
// verifier accepts both the plaintext token and the configured hash string
// itself: an operator who kept only the hash (e.g. the value in a deployment's
// config) can still drive `termcp daemon` management and `termcp stdio`.
// The hash is a credential then, as sensitive as the token.
func (v *Verifier) Verify(token string) bool {
	if v == nil || token == "" {
		return false
	}
	if v.salt != nil {
		if subtle.ConstantTimeCompare(digestOf(v.salt, token), v.digest) == 1 {
			return true
		}
		return subtle.ConstantTimeCompare([]byte(token), []byte(v.hash)) == 1
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(v.token)) == 1
}

// candidate is one client-supplied credential awaiting verification.
type candidate struct {
	token    string
	viaBasic bool
}

// credentials extracts the credential(s) carried by r, in priority order:
// Authorization Bearer, Authorization Basic (password field after the first
// colon; the username is ignored — plus the entire decoded "user:pass" string
// as a fallback, so a client that split a colon-containing token across the
// two fields still authenticates when its join reproduces the token verbatim),
// then the termcp_token cookie. viaBasic marks Basic-sourced candidates, which
// trigger the cookie handoff in Middleware.
func credentials(r *http.Request) []candidate {
	var out []candidate
	if authz := r.Header.Get("Authorization"); authz != "" {
		if scheme, rest, ok := strings.Cut(authz, " "); ok {
			rest = strings.TrimSpace(rest)
			switch {
			case strings.EqualFold(scheme, "Bearer"):
				out = append(out, candidate{token: rest})
			case strings.EqualFold(scheme, "Basic"):
				if decoded, err := base64.StdEncoding.DecodeString(rest); err == nil {
					if _, password, ok := strings.Cut(string(decoded), ":"); ok {
						out = append(out, candidate{token: password, viaBasic: true})
						// Fallback for colon-containing tokens: whatever the
						// client split, only an exact byte-for-byte join of the
						// decoded credentials is accepted.
						out = append(out, candidate{token: string(decoded), viaBasic: true})
					}
				}
			}
		}
	}
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		out = append(out, candidate{token: c.Value})
	}
	return out
}

// publicPaths are static, secret-free documentation documents served from the
// embedded assets. They are readable without credentials so a fresh client (an
// agent before MCP setup, a script, a human with a browser) can read the API
// reference; every data/action surface still requires the token.
var publicPaths = map[string]struct{}{
	"/api.md": {},
}

// isPublicDoc reports whether the request targets one of the credential-free
// documentation endpoints. Only safe read methods are allowed through.
func isPublicDoc(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	_, ok := publicPaths[r.URL.Path]
	return ok
}

// Kind names the trust domain a verified credential belongs to. It rides the
// request context so downstream (logging, scope checks, the approval policy)
// can tell the operator from an agent without knowing token shapes.
type Kind string

const (
	// KindOperator is the deployment token: every surface, including the
	// management REST API, the Web UI and the WebSocket.
	KindOperator Kind = "human"
	// KindAgent is the MCP token: the MCP transports only. A valid MCP token
	// presented anywhere else is answered 403 — the credential is good and the
	// door is wrong, and saying so is not an oracle (a random string still
	// gets 401).
	KindAgent Kind = "agent"
)

type kindContextKey struct{}
type principalContextKey struct{}

// KindFromContext returns the trust domain the middleware verified for this
// request. The second result is false when no credential was involved (public
// documentation paths, or an unauthenticated server).
func KindFromContext(ctx context.Context) (Kind, bool) {
	k, ok := ctx.Value(kindContextKey{}).(Kind)
	return k, ok
}

// PrincipalFromContext returns the full principal the middleware verified:
// kind plus any scopes (scoped tokens) or the unrestricted static-credential
// principal.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*Principal)
	return p, ok
}

// mcpPaths are the MCP transports: the only doors an agent-kind token may
// open. Everything else — management REST, Web UI, WebSocket — is the
// operator surface.
var mcpPaths = map[string]struct{}{
	"/sse":     {},
	"/message": {},
	"/stream":  {},
}

func isMCPPath(path string) bool {
	_, ok := mcpPaths[path]
	return ok
}

// MiddlewareFull is MiddlewarePair plus the scoped-token table: a presented
// tcptok_… credential resolves against the store into a scoped Principal.
// Scoped tokens authenticate on every surface — what they may DO there is the
// admission layer's question (their Tools/Sessions whitelists), not the
// middleware's. Resolution failures (unknown, expired, revoked) are plain
// invalid credentials: 401, indistinguishable from a random string.
func MiddlewareFull(operator, mcp *Verifier, tokens *TokenStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicDoc(r) {
			next.ServeHTTP(w, r)
			return
		}
		mcpOK := isMCPPath(r.URL.Path)
		for _, c := range credentials(r) {
			if operator != nil && operator.Verify(c.token) {
				serveWithPrincipal(w, r, next, c, &Principal{ID: "operator", Kind: KindOperator})
				return
			}
			if mcpOK && mcp != nil && mcp.Verify(c.token) {
				// The MCP token never earns the browser cookie: the cookie
				// exists for the Web UI and WebSocket, both operator surface.
				serveWithPrincipal(w, r, next, candidate{token: c.token}, &Principal{ID: "mcp", Kind: KindAgent})
				return
			}
			if tokens != nil && strings.HasPrefix(c.token, tokenPrefix) {
				if p, err := tokens.Resolve(c.token); err == nil {
					serveWithPrincipal(w, r, next, c, p)
					return
				}
			}
		}
		// A valid MCP token aimed at the operator surface gets 403: distinct
		// from 401 so the caller learns the door, not just the failure.
		if mcp != nil && !mcpOK {
			for _, c := range credentials(r) {
				if mcp.Verify(c.token) {
					http.Error(w, "forbidden: the MCP token reaches /sse, /message and /stream only", http.StatusForbidden)
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="termcp", charset="UTF-8"`)
		http.Error(w, "unauthorized: send Authorization: Bearer <token>, Basic with the token as password, or the termcp_token cookie", http.StatusUnauthorized)
	})
}

// MiddlewarePair splits authentication into two trust domains (design doc
// scoped-tokens.md PR 0): the operator verifier opens every surface, the mcp
// verifier opens only the MCP transports. A request authenticated by the mcp
// token against any other path is answered 403 — the token is valid and the
// surface is wrong. A nil mcp verifier collapses the split: behavior is
// byte-for-byte the legacy single-token Middleware. A nil operator verifier
// leaves only the MCP transports reachable.
//
// The matched kind is stored on the request context (KindFromContext).
func MiddlewarePair(operator, mcp *Verifier, next http.Handler) http.Handler {
	return MiddlewareFull(operator, mcp, nil, next)
}

// serveWithPrincipal runs next with the principal (and its kind, for the
// legacy reader) on the context, plus the Basic→cookie handoff for
// operator-shaped credentials.
func serveWithPrincipal(w http.ResponseWriter, r *http.Request, next http.Handler, c candidate, p *Principal) {
	if c.viaBasic {
		http.SetCookie(w, &http.Cookie{
			Name:     CookieName,
			Value:    c.token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   r.TLS != nil,
		})
	}
	ctx := context.WithValue(r.Context(), kindContextKey{}, p.Kind)
	ctx = context.WithValue(ctx, principalContextKey{}, p)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// Middleware wraps next so every request must carry a valid token. The static
// documentation endpoints (publicPaths) bypass the check. For any other path,
// a missing or invalid credential is answered with 401 plus a Basic challenge,
// which makes browsers show their native username/password dialog (username is
// ignored, password is the token). A nil or unconfigured verifier denies
// everything except the public documentation (fail closed).
//
// Middleware is the single-verifier legacy shape; MiddlewarePair adds the
// operator/agent split.
func Middleware(v *Verifier, next http.Handler) http.Handler {
	return MiddlewarePair(v, nil, next)
}

// WithPrincipal attaches a principal to a context. It exists for tests and
// for transports that authenticate outside the HTTP middleware (the stdio
// bridge forwards credentials to the instance, which resolves them there).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	ctx = context.WithValue(ctx, kindContextKey{}, p.Kind)
	return context.WithValue(ctx, principalContextKey{}, p)
}
