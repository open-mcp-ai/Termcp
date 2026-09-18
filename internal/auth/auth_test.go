package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func basicHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func TestHash_RoundTrip(t *testing.T) {
	const token = "correct horse battery staple"
	h, err := Hash(token)
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}
	if !strings.HasPrefix(h, "sha256-") {
		t.Fatalf("hash %q lacks sha256- prefix", h)
	}
	v, err := NewVerifier("", h)
	if err != nil {
		t.Fatalf("NewVerifier() error: %v", err)
	}
	if !v.Verify(token) {
		t.Fatal("Verify() = false for the hashed token")
	}
	if v.Verify(token + "x") {
		t.Fatal("Verify() = true for a wrong token")
	}
}

func TestHash_UniqueSalt(t *testing.T) {
	a, err := Hash("same")
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}
	b, err := Hash("same")
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same token are identical; salt is not random")
	}
}

func TestHash_RejectsEmptyToken(t *testing.T) {
	if _, err := Hash(""); err == nil {
		t.Fatal("Hash(\"\") succeeded, want error")
	}
}

func TestNewVerifier_RejectsBothAndNeither(t *testing.T) {
	if _, err := NewVerifier("tok", "sha256-aa-bb"); err == nil {
		t.Fatal("both token and hash accepted, want error")
	}
	if _, err := NewVerifier("", ""); err == nil {
		t.Fatal("neither token nor hash accepted, want error")
	}
	if _, err := NewVerifier("", "   "); err == nil {
		t.Fatal("blank hash accepted, want error")
	}
}

func TestNewVerifier_RejectsMalformedHash(t *testing.T) {
	for _, bad := range []string{
		"plaintext",
		"md5-aa-bb",
		"sha256-aa",
		"sha256--bb",
		"sha256-zz-bb",
		"sha256-aabb-cc", // digest too short
	} {
		if _, err := NewVerifier("", bad); err == nil {
			t.Fatalf("malformed hash %q accepted, want error", bad)
		}
	}
}

func TestNewVerifier_PlainTokenVerification(t *testing.T) {
	v, err := NewVerifier("secret", "")
	if err != nil {
		t.Fatalf("NewVerifier() error: %v", err)
	}
	if !v.Verify("secret") {
		t.Fatal("Verify(correct) = false")
	}
	if v.Verify("wrong") || v.Verify("") || v.Verify("SeCrEt") {
		t.Fatal("Verify() accepted an invalid token")
	}
}

func TestMiddleware_MissingAndInvalidCredentialsGet401(t *testing.T) {
	v, _ := NewVerifier("secret", "")
	h := Middleware(v, okHandler())

	cases := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodGet, "/", nil),
	}
	cases[1].Header.Set("Authorization", "Bearer wrong")
	cases[2].Header.Set("Authorization", "NotAScheme xyz")

	for i, req := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("case %d: status = %d, want 401", i, rr.Code)
		}
		if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
			t.Fatalf("case %d: WWW-Authenticate = %q, want Basic challenge", i, got)
		}
	}
}

func TestMiddleware_BearerAccepted(t *testing.T) {
	v, _ := NewVerifier("secret", "")
	h := Middleware(v, okHandler())

	for _, scheme := range []string{"Bearer", "bearer"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", scheme+" secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("scheme %s: status = %d, want 200", scheme, rr.Code)
		}
	}
}

func TestMiddleware_BasicAcceptedAndSetsCookie(t *testing.T) {
	v, _ := NewVerifier("secret", "")
	h := Middleware(v, okHandler())

	// Username is arbitrary; the password carries the token.
	req := httptest.NewRequest(http.MethodGet, "/api/ui/state", nil)
	req.Header.Set("Authorization", basicHeader("ignored-user", "secret"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	cookies := rr.Result().Cookies()
	var c *http.Cookie
	for _, ck := range cookies {
		if ck.Name == CookieName {
			c = ck
		}
	}
	if c == nil {
		t.Fatalf("no %s cookie set, got %v", CookieName, rr.Header().Values("Set-Cookie"))
	}
	if c.Value != "secret" {
		t.Fatalf("cookie value = %q, want the token", c.Value)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v must be HttpOnly with SameSite=Strict", c)
	}
}

func TestMiddleware_CookieAccepted(t *testing.T) {
	v, _ := NewVerifier("secret", "")
	h := Middleware(v, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "secret"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "wrong"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: status = %d, want 401", rr.Code)
	}
}

func TestMiddleware_StaleAuthorizationFallsBackToCookie(t *testing.T) {
	// Browsers resend a stale/wrong Basic header; a valid cookie must still win.
	v, _ := NewVerifier("secret", "")
	h := Middleware(v, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", basicHeader("u", "old-password"))
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "secret"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestMiddleware_HashVerifierAcceptsTokenOverAllChannels(t *testing.T) {
	hstr, err := Hash("secret")
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}
	v, err := NewVerifier("", hstr)
	if err != nil {
		t.Fatalf("NewVerifier() error: %v", err)
	}
	h := Middleware(v, okHandler())

	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodGet, "/", nil),
	}
	requests[0].Header.Set("Authorization", "Bearer secret")
	requests[1].AddCookie(&http.Cookie{Name: CookieName, Value: "secret"})
	for i, req := range requests {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("case %d: status = %d, want 200", i, rr.Code)
		}
	}
}

func TestMiddleware_MalformedBasicHeader(t *testing.T) {
	v, _ := NewVerifier("secret", "")
	h := Middleware(v, okHandler())

	for _, hdr := range []string{
		"Basic !!!not-base64!!!",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("no-colon")),
		"Basic",
		"Basic ",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", hdr)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: status = %d, want 401", hdr, rr.Code)
		}
	}
}

func TestMiddleware_NilVerifierFailsClosed(t *testing.T) {
	h := Middleware(nil, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for nil verifier", rr.Code)
	}
}

func TestMiddleware_BasicTokenWithColonAcceptedInEitherSplit(t *testing.T) {
	// A token containing a colon works whether the client puts the whole token
	// in the password field (:AAA:BBB -> AAA:BBB) or splits it across the two
	// fields (curl -u AAA:BBB -> the join reproduces the token verbatim).
	v, _ := NewVerifier("AAA:BBB", "")
	h := Middleware(v, okHandler())
	for name, header := range map[string]string{
		"whole-token-as-password": basicHeader("", "AAA:BBB"),
		"nonempty-username":       basicHeader("ignored-user", "AAA:BBB"),
		"curl-style-split":        basicHeader("AAA", "BBB"),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", header)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", name, rr.Code)
		}
	}
}

func TestMiddleware_BasicSplitMustReproduceTokenExactly(t *testing.T) {
	v, _ := NewVerifier("AAA:BBB", "")
	h := Middleware(v, okHandler())
	for name, header := range map[string]string{
		"split-on-wrong-colon": basicHeader("A", "AA:BBB"),
		"trailing-garbage":     basicHeader("", "AAA:BBBx"),
		"different-token":      basicHeader("AAA", "ccc"),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", header)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rr.Code)
		}
	}
}

func TestMiddleware_BasicPasswordMayContainColon(t *testing.T) {
	v, _ := NewVerifier("se:cret", "")
	h := Middleware(v, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", basicHeader("user", "se:cret"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

// TestMiddleware_PublicDocsNeedNoCredentials pins the docs exception: the two
// read-only documents are fetchable without a token (that is how an agent
// without MCP, or a script, learns the API), while every other surface keeps
// requiring credentials — including non-GET methods on those same paths.
func TestMiddleware_PublicDocsNeedNoCredentials(t *testing.T) {
	v, err := NewVerifier("secret", "")
	if err != nil {
		t.Fatal(err)
	}
	h := Middleware(v, okHandler())

	for _, p := range []string{"/api.md", "/skills.md"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(method, p, nil))
			if rr.Code != http.StatusOK {
				t.Errorf("%s %s = %d, want 200 without credentials", method, p, rr.Code)
			}
		}
		// Write methods on the same paths are not part of the exception.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, p, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("POST %s = %d, want 401", p, rr.Code)
		}
	}

	// Everything else still needs the token.
	for _, p := range []string{"/", "/api.html", "/api/sessions", "/sse", "/stream", "/api.md/../api/sessions"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", p, rr.Code)
		}
	}

	// With the token the docs (and everything else) still work.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api.md", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("GET /api.md with token = %d, want 200", rr.Code)
	}
}

// TestMiddleware_PublicDocsFailClosedWithoutVerifier: even with no verifier
// configured, only the docs bypass (nil verifier still denies data paths).
func TestMiddleware_PublicDocsFailClosedWithoutVerifier(t *testing.T) {
	h := Middleware(nil, okHandler())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("nil verifier: GET /api/sessions = %d, want 401", rr.Code)
	}
}
