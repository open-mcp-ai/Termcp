// Package auth implements termcp's single-token HTTP authentication.
//
// The server is configured with either a plaintext token (--auth-token /
// TERMCP_AUTH_TOKEN) or a salted SHA-256 hash of one (--auth-hash /
// TERMCP_AUTH_HASH), so deployments that prefer not to store the cleartext
// can keep only the hash. One token guards the whole HTTP surface: Web UI,
// REST API, MCP SSE, MCP streamable HTTP, and the WebSocket.
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
		salt, digest, err := parseHash(strings.TrimSpace(hash))
		if err != nil {
			return nil, err
		}
		v.salt, v.digest = salt, digest
	}
	return v, nil
}

// Verify reports whether token matches the configured secret.
func (v *Verifier) Verify(token string) bool {
	if v == nil || token == "" {
		return false
	}
	if v.salt != nil {
		return subtle.ConstantTimeCompare(digestOf(v.salt, token), v.digest) == 1
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

// Middleware wraps next so every request must carry a valid token. A missing
// or invalid credential is answered with 401 plus a Basic challenge, which
// makes browsers show their native username/password dialog (username is
// ignored, password is the token). A nil or unconfigured verifier denies
// everything (fail closed).
func Middleware(v *Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range credentials(r) {
			if v.Verify(c.token) {
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
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="termcp", charset="UTF-8"`)
		http.Error(w, "unauthorized: send Authorization: Bearer <token>, Basic with the token as password, or the termcp_token cookie", http.StatusUnauthorized)
	})
}
