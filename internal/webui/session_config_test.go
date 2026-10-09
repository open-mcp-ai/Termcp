package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// An omitted ssh_config must be refused on REST, exactly as it already is on
// MCP.
//
// What this guards: the handler used to resolve an empty ssh_config to the
// built-in loopback profile, so a caller that forgot the field — or a client
// that dropped it — was handed a live shell on the machine Termcp runs on. Every
// other way of naming a connection fails loudly (a misspelled profile reports
// not-found); an empty one succeeding is the only case where the caller cannot
// tell that it asked for nothing, and the only one that lands on the host
// holding every stored credential.
//
// The MCP surface has always refused here (errMissingSSHConfig,
// CodeInvalidArgument — see TestHandleStartSession_MissingSSHConfig). A parameter
// answering differently on the two surfaces is a gap that only ever shows up in
// the dangerous direction, which is why this asserts the REST half of that same
// contract.
func TestCreateSessionRefusesMissingSSHConfig(t *testing.T) {
	e := newHostAttributionEnv(t)

	for _, tc := range []struct {
		name, body string
	}{
		{"omitted", `{"rows":24,"cols":80}`},
		{"blank", `{"ssh_config":"","rows":24,"cols":80}`},
		{"whitespace", `{"ssh_config":"   ","rows":24,"cols":80}`},
		// An empty body is the same mistake by another route: a client that sent
		// nothing at all must not get a shell either.
		{"empty body", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(tc.body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("POST /api/sessions (%s) = %d, want 400; this must not silently open a shell on the loopback host. body: %s",
					tc.name, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "ssh_config is required") {
				t.Errorf("the refusal should name the missing field; got %q", rr.Body.String())
			}
		})
	}

	// A refused request must not leave a session behind for a later list call to
	// show: the guard exists to prevent the session, not just to relabel it.
	if sessions := e.sessions(t); len(sessions) != 0 {
		t.Errorf("a refused create left %d session(s) behind", len(sessions))
	}
}

// Naming the loopback profile explicitly still works. The fix is that the caller
// must SAY it, not that the profile is gone: working on the local box is a real
// use case and stays available to anyone who asks for it by name.
func TestCreateSessionAcceptsExplicitLoopback(t *testing.T) {
	e := newHostAttributionEnv(t)
	rr := e.post(t, `{"ssh_config":"internal","rows":24,"cols":80}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit ssh_config=internal = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var created struct {
		SessionID string `json:"session_id"`
		SSHConfig string `json:"ssh_config"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.SSHConfig != "internal" {
		t.Errorf("ssh_config = %q, want the profile the caller named", created.SSHConfig)
	}
	if created.SessionID == "" {
		t.Error("no session_id in the response")
	}
}

// A locator is the other way to name a profile, so the rule has to sit where the
// name is RESOLVED, not in the request struct: otherwise the locator spelling
// would be a way around it. `termcp://` with an empty entry is the case this
// pins.
func TestCreateSessionRefusesLocatorWithNoEntry(t *testing.T) {
	e := newHostAttributionEnv(t)
	rr := e.post(t, `{"ssh_config":"termcp://","rows":24,"cols":80}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("ssh_config=termcp:// = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "internal") {
		t.Errorf("a malformed locator must not resolve to the loopback profile; got %q", rr.Body.String())
	}
}

// The refusal does not depend on --no-internal. A missing field is answered "say
// which host" whether or not the loopback profile happens to be enabled: tying
// the two together is how the previous message ended up reading "required when
// internal profile is disabled", which implies the field IS optional otherwise —
// the exact reading that put a host shell behind a forgotten parameter.
func TestCreateSessionRefusesMissingSSHConfigWithoutLoopback(t *testing.T) {
	e := newHostAttributionEnv(t)
	e.mux = http.NewServeMux()
	(&Handler{Sessions: e.sessMgr, SSH: e.store, NoInternal: true}).Register(e.mux)

	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty ssh_config with NoInternal = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ssh_config is required") {
		t.Errorf("got %q, want the same missing-field answer as the default build", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "no-internal") {
		t.Errorf("the refusal must not be phrased as a consequence of --no-internal; got %q", rr.Body.String())
	}

	// The flag keeps its own meaning: naming the loopback profile explicitly is
	// still refused when the profile is switched off.
	rr = httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions",
		strings.NewReader(`{"ssh_config":"internal","rows":24,"cols":80}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("explicit internal with NoInternal = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

// A misspelled profile still reports what it is: a lookup failure, not a missing
// field. The two must stay distinguishable, or a client cannot tell "you forgot
// the parameter" from "that profile does not exist".
func TestCreateSessionUnknownProfileIsNotAMissingField(t *testing.T) {
	e := newHostAttributionEnv(t)
	rr := e.post(t, `{"ssh_config":"definitely-not-here","rows":24,"cols":80}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "ssh_config is required") {
		t.Errorf("an unknown profile must not be reported as a missing field; got %q", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "definitely-not-here") {
		t.Errorf("the error should name the profile it could not find; got %q", rr.Body.String())
	}
}

// The rule is enforced in resolveSSH, the single place every session-starting
// route funnels through — not by one handler remembering to check. This pins the
// wiring, because the guard is only as good as the callers routing through it.
func TestSessionStartRoutesThroughTheMissingFieldGuard(t *testing.T) {
	src := readGoSource(t, "handler_session.go")
	if !strings.Contains(src, "h.resolveSSH(") {
		t.Error("handleCreateSession no longer resolves through resolveSSH, which is where the missing-field guard lives")
	}
	conn := readGoSource(t, "handler_conn.go")
	if !strings.Contains(conn, "errSSHConfigRequired") {
		t.Error("resolveSSH no longer refuses an empty ssh_config: an unnamed request would fall through to the store and open a local shell")
	}
	// The specific regression, stated as the assignment that caused it. A future
	// edit that reintroduces a fallback has to delete this line's target, and
	// this test is what makes that deletion loud.
	if strings.Contains(conn, `name = "internal"`) {
		t.Error("resolveSSH assigns the loopback profile to an unnamed request again — that is the bug this file pins")
	}
}

// The MCP half of the contract, asserted here too so the two surfaces cannot
// drift apart silently: if someone relaxes one, they see the other's expectation
// in the same package's tests. The behaviour itself is exercised by
// TestHandleStartSession_MissingSSHConfig; this only pins that REST refuses for
// the same stated reason.
func TestBothSurfacesStateTheSameMissingFieldRule(t *testing.T) {
	if errSSHConfigRequired.Error() != "ssh_config is required" {
		t.Errorf("the REST error reads %q; MCP answers with exactly %q, and the two must not diverge",
			errSSHConfigRequired.Error(), "ssh_config is required")
	}
	// The loopback profile is still what an explicit request resolves to, so this
	// change narrowed the default and left the capability intact.
	if ent := sshconfig.InternalEntry(); ent == nil || ent.Kind != sshconfig.KindInternal {
		t.Fatal("the built-in loopback profile no longer resolves; this fix is about the default, not about removing the profile")
	}
}
