package extensions

// Issue #321: an extension may publish this instance's HTTP surface over ssh -R.
//
// THE ASSERTIONS HERE ARE THE ONES THAT DECIDE WHETHER THE FEATURE IS SAFE, not whether
// it is wired up: a whitelist that leaks, a template that silently exposes nothing, and a
// whitelist change that never reaches the listener.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
)

// A PATH THE DECLARATION DID NOT NAME MUST NOT BE ANSWERED, and it must not be answered
// with 403 either: "forbidden" confirms the route exists.
func TestWhitelistAnswers404OffTheList(t *testing.T) {
	h := Whitelist([]string{"/api/term"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached " + r.URL.Path))
	}))

	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/term", 200},
		{"/api.md", 404},
		{"/api/term/", 404},       // not a prefix: a trailing slash is a different path
		{"/api", 404},             //
		{"/api/term/../api", 404}, // no traversal out of the list
		{"/SSE", 404},             // exact case, exact spelling
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("%s answered %d, want %d", tc.path, rec.Code, tc.code)
		}
		if tc.code == 404 && strings.Contains(rec.Body.String(), "reached") {
			t.Errorf("%s reached the handler", tc.path)
		}
	}
}

// AN EXPOSE WITH NO PATHS IS REFUSED, not treated as "everything" and not as "nothing".
func TestExposeRequiresAtLeastOnePath(t *testing.T) {
	e := Extension{Name: "a", Target: "internal", Command: "sleep", Args: []string{"5"},
		Exposes: []ExposeDecl{{Transport: "http"}}}
	e.Normalize()
	err := e.Validate()
	if err == nil {
		t.Fatal("an expose with no paths validated")
	}
	if !strings.Contains(err.Error(), "at least one path") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}

// WILDCARDS ARE REFUSED AT SAVE TIME, because the matcher does not implement them and a
// config that looks like it supports them would be a lie the far side discovers.
func TestExposeRejectsWildcards(t *testing.T) {
	e := Extension{Name: "a", Target: "internal", Command: "sleep", Args: []string{"5"},
		Exposes: []ExposeDecl{{Paths: []string{"/api/*"}}}}
	e.Normalize()
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("a wildcard path validated (err=%v)", err)
	}

	// And a relative path is refused rather than normalised into something else.
	rel := Extension{Name: "a", Target: "internal", Command: "sleep", Args: []string{"5"},
		Exposes: []ExposeDecl{{Paths: []string{"api/term"}}}}
	rel.Normalize()
	if err := rel.Validate(); err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("a relative path validated (err=%v)", err)
	}
}

// A TEMPLATE THAT CANNOT BE PUBLISHED SAYS SO. Here the instance has no HTTP surface to
// publish, which is what an embedder that never called SetExposeHandler looks like.
func TestExposeWithoutHandlerIsReported(t *testing.T) {
	r := &Reconciler{}
	st := &extState{sessionID: "sess"}
	ext := &Extension{Name: "a", Exposes: []ExposeDecl{{Paths: []string{"/api.md"}}}}
	err := r.ensureExposes(ext, st)
	if err == nil || !strings.Contains(err.Error(), "cannot expose") {
		t.Fatalf("a missing handler was not reported: %v", err)
	}
}

// THE WHITELIST IS PART OF WHAT RUNS: editing it must rebuild the listener, or the far
// side keeps reaching the old set of paths while the template says otherwise.
func TestExposeFingerprintTracksPaths(t *testing.T) {
	base := Extension{Name: "a", Target: "internal", Command: "sleep",
		Exposes: []ExposeDecl{{Paths: []string{"/api/term"}}}}
	before := base.Fingerprint()

	base.Exposes[0].Paths = []string{"/api/term", "/api.md"}
	if after := base.Fingerprint(); after == before {
		t.Fatal("adding a path did not change the fingerprint: an edit would not reach the listener")
	}

	base.Exposes[0].Paths = []string{"/api/term"}
	if again := base.Fingerprint(); again != before {
		t.Fatal("restoring the paths did not restore the fingerprint")
	}
}

// THE HTTP HALF, WITHOUT SSH: the accepted connection is served directly, and cancelling
// the context closes the listener, which is what "stopping the extension makes it
// unreachable" means on this side.
func TestServeHTTPOnServesAndCloses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	forward.ServeHTTPOn(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + ln.Addr().String() + "/anything")
		if err == nil {
			buf := make([]byte, 64)
			n, _ := resp.Body.Read(buf)
			_ = resp.Body.Close()
			body = string(buf[:n])
			if resp.StatusCode == 200 && body == "served" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body != "served" {
		t.Fatalf("the handler was never reached over the listener (last body %q)", body)
	}

	cancel()
	// The listener must stop accepting. Polled rather than slept on: the close happens
	// in another goroutine, and the assertion is about the state, not the delay.
	closed := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond); err != nil {
			closed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !closed {
		t.Fatal("the listener still accepts connections after the context was cancelled")
	}
}
