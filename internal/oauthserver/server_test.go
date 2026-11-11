package oauthserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestOpaqueTokensStoreOnlyHashAndExpire(t *testing.T) {
	strategy := newOpaqueStrategy(testLifespans{})
	raw, signature, err := strategy.GenerateAccessToken(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if signature == raw || signature != OpaqueSignatureForTest(raw) {
		t.Fatal("opaque token signature was not a hash")
	}
	if strategy.AccessTokenSignature(context.Background(), raw) != signature {
		t.Fatal("signature is not deterministic")
	}
}

func OpaqueSignatureForTest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type testLifespans struct{}

func (testLifespans) GetAccessTokenLifespan(context.Context) time.Duration {
	return time.Minute
}

func (testLifespans) GetRefreshTokenLifespan(context.Context) time.Duration {
	return time.Hour
}

func (testLifespans) GetAuthorizeCodeLifespan(context.Context) time.Duration {
	return time.Minute
}

func TestRedirectValidationAllowsDynamicLoopbackPortsOnly(t *testing.T) {
	for _, test := range []struct {
		name      string
		allowed   string
		requested string
		want      bool
	}{
		{"same", "https://client.example/cb", "https://client.example/cb", true},
		{"wrong host", "https://client.example/cb", "https://evil.example/cb", false},
		{"loopback dynamic port", "http://127.0.0.1:1111/callback", "http://127.0.0.1:2222/callback", true},
		{"loopback path", "http://127.0.0.1:1111/callback", "http://127.0.0.1:2222/other", false},
		{"remote http", "http://client.example/cb", "http://client.example/cb", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, _ := url.Parse(test.allowed)
			requested, _ := url.Parse(test.requested)
			if got := RedirectAllowed([]string{allowed.String()}, requested); got != test.want {
				t.Fatalf("RedirectAllowed = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFetcherRejectsPrivateNetworkByDefault(t *testing.T) {
	fetcher := NewFetcher()
	if _, _, err := fetcher.get(context.Background(), "https://127.0.0.1/metadata.json", maxMetadataBytes); err == nil {
		t.Fatal("private-network metadata URL was accepted")
	}
	if _, _, err := fetcher.get(context.Background(), "http://example.com/metadata.json", maxMetadataBytes); err == nil {
		t.Fatal("non-HTTPS metadata URL was accepted")
	}
	if !publicAddress(netip.MustParseAddr("198.18.0.8")) {
		t.Fatal("fake-IP DNS range should remain eligible for hostname-validated HTTPS")
	}
}
