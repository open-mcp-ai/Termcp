package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ory/fosite"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

type oauthCachedClient struct {
	client  oauthserver.Client
	expires time.Time
}

const maxOAuthClientCache = 512

type oauthRateBucket struct {
	window time.Time
	count  int
}

type oauthRequestedRedirectKey struct{}

func withOAuthRequestedRedirect(ctx context.Context, raw string) context.Context {
	return context.WithValue(ctx, oauthRequestedRedirectKey{}, raw)
}

func requestedRedirectFromContext(ctx context.Context) string {
	value, _ := ctx.Value(oauthRequestedRedirectKey{}).(string)
	return value
}

type oauthAuthorizationTransaction struct {
	mu             sync.Mutex
	Browser        string
	CSRF           string
	Nonce          string
	Verifier       string
	ExpiresAt      time.Time
	ConfigRevision uint64
	Client         oauthserver.Client
	OIDC           *oauthserver.OIDC
	Authorize      fosite.AuthorizeRequester
	Identity       *oauthserver.Identity
	AvatarFile     string
	AvatarType     string
	Confirmed      bool
}

func (m *Managed) resolveOAuthClient(ctx context.Context, id string) (oauthserver.Client, error) {
	if id == "" || len(id) > 2048 {
		return oauthserver.Client{}, fosite.ErrNotFound
	}
	if !strings.HasPrefix(id, "https://") {
		m.mu.Lock()
		client, ok := m.state.OAuthClients[id]
		m.mu.Unlock()
		if !ok || !client.Enabled {
			return oauthserver.Client{}, fosite.ErrNotFound
		}
		return client, nil
	}
	if !validCIMDClientID(id) {
		return oauthserver.Client{}, fosite.ErrInvalidClient
	}
	now := m.now().UTC()
	m.oauthClientCacheMu.Lock()
	if cached, ok := m.oauthClientCache[id]; ok && now.Before(cached.expires) {
		m.oauthClientCacheMu.Unlock()
		return cached.client, nil
	}
	m.oauthClientCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, fetchTimeoutForOAuth())
	defer cancel()
	result, err := m.oauthHTTPFetcher().ClientMetadata(ctx, id)
	if err != nil {
		return oauthserver.Client{}, fosite.ErrInvalidClient.WithWrap(err)
	}
	client := oauthserver.Client{
		ID:           id,
		Name:         result.Value.ClientName,
		URI:          result.Value.ClientURI,
		LogoURI:      result.Value.LogoURI,
		Registration: oauthserver.ClientRegistrationCIMD,
		RedirectURIs: append([]string(nil), result.Value.RedirectURIs...),
		CreatedAt:    now,
		Enabled:      true,
	}
	m.mu.Lock()
	if existing, ok := m.state.OAuthClients[id]; ok {
		client.CreatedAt = existing.CreatedAt
	}
	m.mu.Unlock()
	if tx := transactionFromContext(ctx); tx != nil {
		tx.candidate.OAuthClients[id] = client
	}
	m.oauthClientCacheMu.Lock()
	if len(m.oauthClientCache) >= maxOAuthClientCache {
		for id, cached := range m.oauthClientCache {
			if !now.Before(cached.expires) {
				delete(m.oauthClientCache, id)
			}
		}
	}
	if len(m.oauthClientCache) >= maxOAuthClientCache {
		m.oauthClientCacheMu.Unlock()
		return oauthserver.Client{}, fosite.ErrTemporarilyUnavailable.WithHint("The CIMD cache is full")
	}
	m.oauthClientCache[id] = oauthCachedClient{client: client, expires: result.CacheUntil}
	m.oauthClientCacheMu.Unlock()
	return client, nil
}

func validCIMDClientID(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Path != "" && u.Path != "/" && u.User == nil && u.Fragment == ""
}

func fetchTimeoutForOAuth() time.Duration {
	return 10 * time.Second
}

func (m *Managed) setOAuthFetcher(fetcher *oauthserver.Fetcher) {
	if fetcher == nil {
		fetcher = oauthserver.NewFetcher()
	}
	m.oauthClientCacheMu.Lock()
	m.oauthFetcher = fetcher
	m.oauthClientCache = map[string]oauthCachedClient{}
	m.oauthClientCacheMu.Unlock()
}

func (m *Managed) oauthHTTPFetcher() *oauthserver.Fetcher {
	m.oauthClientCacheMu.Lock()
	defer m.oauthClientCacheMu.Unlock()
	if m.oauthFetcher == nil {
		m.oauthFetcher = oauthserver.NewFetcher()
	}
	return m.oauthFetcher
}

func (m *Managed) rememberOAuthTransaction(tx *oauthAuthorizationTransaction) (string, error) {
	if tx == nil || tx.Browser == "" {
		return "", errors.New("invalid OAuth transaction")
	}
	state, err := randomSecret()
	if err != nil {
		return "", err
	}
	now := m.now().UTC()
	m.oauthTransactionsMu.Lock()
	defer m.oauthTransactionsMu.Unlock()
	for id, existing := range m.oauthTransactions {
		if !now.Before(existing.ExpiresAt) {
			delete(m.oauthTransactions, id)
		}
	}
	if len(m.oauthTransactions) >= 256 {
		return "", errors.New("too many pending OAuth logins")
	}
	m.oauthTransactions[state] = tx
	return state, nil
}

func (m *Managed) oauthTransaction(state string) *oauthAuthorizationTransaction {
	if state == "" {
		return nil
	}
	now := m.now().UTC()
	m.oauthTransactionsMu.Lock()
	defer m.oauthTransactionsMu.Unlock()
	tx := m.oauthTransactions[state]
	if tx == nil || !now.Before(tx.ExpiresAt) {
		delete(m.oauthTransactions, state)
		return nil
	}
	return tx
}

func (m *Managed) consumeOAuthTransaction(state string) *oauthAuthorizationTransaction {
	if state == "" {
		return nil
	}
	now := m.now().UTC()
	m.oauthTransactionsMu.Lock()
	defer m.oauthTransactionsMu.Unlock()
	tx := m.oauthTransactions[state]
	delete(m.oauthTransactions, state)
	if tx == nil || !now.Before(tx.ExpiresAt) {
		return nil
	}
	return tx
}

func (m *Managed) clearOAuthTransactions() {
	m.oauthTransactionsMu.Lock()
	clear(m.oauthTransactions)
	m.oauthTransactionsMu.Unlock()
}

func (m *Managed) enableOAuthTestMode() {
	fetcher := oauthserver.NewFetcher()
	fetcher.AllowPrivate = true
	m.setOAuthFetcher(fetcher)
}

func oauthNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func (m *Managed) allowOAuthAttempt(r *http.Request, action string, limit int) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		host = "unknown"
	}
	now := m.now().UTC()
	key := action + "\x00" + host
	m.oauthRateMu.Lock()
	defer m.oauthRateMu.Unlock()
	bucket := m.oauthRate[key]
	if bucket.window.IsZero() || !now.Before(bucket.window.Add(time.Minute)) {
		bucket = oauthRateBucket{window: now}
	}
	bucket.count++
	m.oauthRate[key] = bucket
	if len(m.oauthRate) > 2048 {
		for id, candidate := range m.oauthRate {
			if !now.Before(candidate.window.Add(time.Minute)) {
				delete(m.oauthRate, id)
			}
		}
	}
	return bucket.count <= limit
}
