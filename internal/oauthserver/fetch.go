package oauthserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxMetadataBytes = 64 << 10
	maxAvatarBytes   = 1 << 20
	fetchTimeout     = 8 * time.Second
)

type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	ApplicationType         string   `json:"application_type,omitempty"`
}

type FetchResult[T any] struct {
	Value      T
	CacheUntil time.Time
}

// Fetcher bounds outbound requests and blocks private network targets by
// default. Tests may set AllowPrivate explicitly; production auth never does.
type Fetcher struct {
	Client       *http.Client
	AllowPrivate bool
}

func NewFetcher() *Fetcher {
	f := &Fetcher{}
	f.Client = &http.Client{
		Timeout:   fetchTimeout,
		Transport: f.transport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return f.validateURL(req.URL)
		},
	}
	return f
}

func (f *Fetcher) transport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, addr := range addrs {
				if !f.AllowPrivate && !publicAddress(addr) {
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
				if err == nil {
					return conn, nil
				}
			}
			return nil, errors.New("target resolved only to disallowed network addresses")
		},
	}
}

func publicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, blocked := range nonPublicPrefixes {
		if blocked.Contains(addr) {
			return false
		}
	}
	return true
}

// 198.18.0.0/15 is intentionally not listed: transparent DNS proxies commonly
// return that fake range for ordinary public HTTPS hosts, and TLS still validates
// the requested hostname. Literal-IP HTTPS targets without a matching certificate
// therefore fail even when they use this range.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func (f *Fetcher) validateURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("metadata URL must be absolute HTTPS without credentials or fragment")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("metadata URL has an invalid port")
		}
	}
	if f.AllowPrivate {
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		return errors.New("metadata host could not be resolved")
	}
	for _, addr := range addrs {
		if !publicAddress(addr) {
			return errors.New("metadata host resolves to a private or special network address")
		}
	}
	return nil
}

func (f *Fetcher) get(ctx context.Context, raw string, limit int64) ([]byte, http.Header, error) {
	u, err := url.Parse(raw)
	if err != nil || f.validateURL(u) != nil {
		if err == nil {
			err = f.validateURL(u)
		}
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json, image/png, image/jpeg, image/gif")
	response, err := f.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, fmt.Errorf("fetch returned HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("response exceeded size limit")
	}
	return data, response.Header.Clone(), nil
}

func (f *Fetcher) ClientMetadata(ctx context.Context, raw string) (FetchResult[ClientMetadata], error) {
	if _, err := url.ParseRequestURI(raw); err != nil {
		return FetchResult[ClientMetadata]{}, errors.New("invalid client metadata document URL")
	}
	data, headers, err := f.get(ctx, raw, maxMetadataBytes)
	if err != nil {
		return FetchResult[ClientMetadata]{}, err
	}
	var metadata ClientMetadata
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&metadata); err != nil {
		return FetchResult[ClientMetadata]{}, errors.New("invalid client metadata document")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return FetchResult[ClientMetadata]{}, errors.New("client metadata document contains trailing data")
	}
	if err := ValidateClientMetadata(raw, metadata); err != nil {
		return FetchResult[ClientMetadata]{}, err
	}
	return FetchResult[ClientMetadata]{Value: metadata, CacheUntil: cacheUntil(headers, 5*time.Minute)}, nil
}

func ValidateClientMetadata(raw string, metadata ClientMetadata) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" || u.Path == "/" || u.User != nil || u.Fragment != "" {
		return errors.New("client_id must be an HTTPS URL with a path")
	}
	if metadata.ClientID != raw {
		return errors.New("client metadata client_id does not match its document URL")
	}
	if strings.TrimSpace(metadata.ClientName) == "" || len(metadata.ClientName) > 256 {
		return errors.New("client metadata requires client_name")
	}
	if strings.ContainsFunc(metadata.ClientName, func(r rune) bool { return r == '\r' || r == '\n' || r == '\x00' }) {
		return errors.New("client metadata client_name contains control characters")
	}
	if len(metadata.RedirectURIs) == 0 || len(metadata.RedirectURIs) > 16 {
		return errors.New("client metadata requires redirect_uris")
	}
	for _, redirect := range metadata.RedirectURIs {
		if !ValidRedirect(redirect) {
			return fmt.Errorf("invalid redirect URI %q", redirect)
		}
	}
	if len(metadata.GrantTypes) > 0 && !containsString(metadata.GrantTypes, "authorization_code") {
		return errors.New("client metadata must allow authorization_code")
	}
	if len(metadata.ResponseTypes) > 0 && !containsString(metadata.ResponseTypes, "code") {
		return errors.New("client metadata must allow code responses")
	}
	if metadata.TokenEndpointAuthMethod != "" && metadata.TokenEndpointAuthMethod != "none" {
		return errors.New("only public clients using token_endpoint_auth_method=none are supported")
	}
	return nil
}

// Avatar downloads a raster image, bounds its decoded dimensions, and re-encodes
// it as PNG. The returned bytes never preserve attacker-controlled metadata.
func (f *Fetcher) Avatar(ctx context.Context, raw string) ([]byte, string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, "", errors.New("avatar URL is empty")
	}
	data, _, err := f.get(ctx, raw, maxAvatarBytes)
	if err != nil {
		return nil, "", err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 1024 || config.Height > 1024 || config.Width*config.Height > 1024*1024 {
		return nil, "", errors.New("avatar is not a bounded supported raster image")
	}
	var decoded image.Image
	switch format {
	case "png":
		decoded, err = png.Decode(bytes.NewReader(data))
	case "jpeg":
		decoded, err = jpeg.Decode(bytes.NewReader(data))
	case "gif":
		decoded, err = gif.Decode(bytes.NewReader(data))
	default:
		err = errors.New("unsupported avatar image")
	}
	if err != nil {
		return nil, "", err
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, decoded); err != nil {
		return nil, "", err
	}
	if encoded.Len() > maxAvatarBytes {
		return nil, "", errors.New("normalized avatar exceeded size limit")
	}
	return encoded.Bytes(), "image/png", nil
}

func cacheUntil(headers http.Header, fallback time.Duration) time.Time {
	now := time.Now().UTC()
	cacheControl := strings.ToLower(headers.Get("Cache-Control"))
	if strings.Contains(cacheControl, "no-store") || strings.Contains(cacheControl, "no-cache") {
		return now
	}
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "max-age=") {
			continue
		}
		seconds, err := strconv.Atoi(strings.TrimPrefix(part, "max-age="))
		if err == nil && seconds >= 0 {
			lifetime := time.Duration(seconds) * time.Second
			if lifetime > time.Hour {
				lifetime = time.Hour
			}
			return now.Add(lifetime)
		}
	}
	return now.Add(fallback)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
