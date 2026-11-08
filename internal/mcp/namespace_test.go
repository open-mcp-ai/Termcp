package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/mcp"

	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// namespaceContext fakes what the transports' context hook installs for a caller
// that claimed a namespace.
func namespaceContext(name string) context.Context {
	if name == "" {
		return context.Background()
	}
	return context.WithValue(context.Background(), callerNSKey{}, ns.ForCaller(name))
}

// namespaceSession starts a session tagged with the given namespace, the way
// session_start does for a caller that claimed one.
func namespaceSession(t *testing.T, s *Server, callerName string) string {
	t.Helper()
	sess, err := s.sessMgr.Create(session.Config{
		Command:   testShell(),
		Mode:      api.ModePipe,
		Name:      "ns-test-" + callerName,
		SSHConfig: "internal",
		Rows:      24,
		Cols:      80,
		Namespace: ns.ForCaller(callerName),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// The claim itself: a well-formed header becomes a namespace, an absent one is
// the operator, and a malformed one must not silently become the operator —
// that would hand a caller that mistyped its name the shared view, which is the
// opposite of what it asked for.
func TestCallerNamespaceFromHeader(t *testing.T) {
	hdr := func(value string) *http.Request {
		r := &http.Request{Header: http.Header{}}
		if value != "" {
			r.Header.Set(NamespaceHeader, value)
		}
		return r
	}
	resolve := func(value string) string {
		return callerNamespace(withCallerNamespace(context.Background(), hdr(value)))
	}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"unset is the operator", resolve(""), ""},
		{"a name is namespaced", resolve("alice"), "ns:alice"},
		{"surrounding space is trimmed", resolve("  bob  "), "ns:bob"},
		// A name that cannot be honoured must not quietly become "no namespace":
		// that would hand the caller the shared view, the opposite of the claim.
		{"a malformed name isolates rather than widens", resolve("bad/name"), invalidNamespace},
		{"an over-long name isolates too", resolve(strings.Repeat("a", 65)), invalidNamespace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("namespace = %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// A caller's list is its own sessions and nothing else: not a peer's, and not the
// extension layer's — an infrastructure session belongs to the subsystem that
// keeps it warm, so a tenant's list must not grow one just because a tenant asked.
func TestSessionListIsScopedToTheCaller(t *testing.T) {
	s := newTestServer(t)
	alice := namespaceSession(t, s, "alice")
	bob := namespaceSession(t, s, "bob")
	plainSess, err := s.sessMgr.Create(session.Config{
		Command: testShell(), Mode: api.ModePipe, Name: "plain", SSHConfig: "internal", Rows: 24, Cols: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := plainSess.ID
	infraSess, err := s.sessMgr.Create(session.Config{
		Command: testShell(), Mode: api.ModePipe, Name: "ext", SSHConfig: "internal", Rows: 24, Cols: 80,
		Namespace: ns.ForExtensions("internal"),
	})
	if err != nil {
		t.Fatal(err)
	}
	infra := infraSess.ID

	list := func(ctx context.Context) map[string]bool {
		t.Helper()
		res, err := withLogging("session_list", s.handleListSessions, s)(ctx, makeRequest(nil))
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, sess := range parseResult(t, res)["sessions"].([]any) {
			ids[sess.(map[string]any)["id"].(string)] = true
		}
		return ids
	}

	t.Run("alice sees only hers", func(t *testing.T) {
		ids := list(namespaceContext("alice"))
		if !ids[alice] || ids[bob] || ids[plain] || ids[infra] {
			t.Errorf("alice's list = %v, want exactly %s", ids, alice)
		}
	})
	t.Run("bob sees only his", func(t *testing.T) {
		ids := list(namespaceContext("bob"))
		if !ids[bob] || ids[alice] || ids[plain] || ids[infra] {
			t.Errorf("bob's list = %v, want exactly %s", ids, bob)
		}
	})
	// The operator is the person who owns the instance: everything an agent asked
	// for is their list's business — hiding their own agent's sessions from them
	// would be multi-tenancy rather than the "two agents do not collide" this is —
	// and infrastructure stays out of it, because it has a column of its own.
	t.Run("the operator sees every ordinary session", func(t *testing.T) {
		ids := list(context.Background())
		if !ids[alice] || !ids[bob] || !ids[plain] {
			t.Errorf("the operator's list = %v, want alice, bob and plain", ids)
		}
		if ids[infra] {
			t.Error("an infrastructure session belongs in its subsystem's column, not the session list")
		}
	})
}

// A namespaced caller may not act on a resource outside its namespace, even when
// it knows the id. Filtering listings alone would be a facade: an id handed over
// once — from a log, a screenshot, an earlier conversation — would be enough to
// drive somebody else's terminal.
func TestGuardRefusesForeignNamespace(t *testing.T) {
	s := newTestServer(t)
	alice := namespaceSession(t, s, "alice")

	check := func(t *testing.T, ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		return (toolGuard{server: s}).check(ctx, tool, lookupGate(tool), makeRequest(args))
	}

	t.Run("the owner may drive it", func(t *testing.T) {
		if bad := check(t, namespaceContext("alice"), "shell_input", map[string]any{"shell_id": alice}); bad != nil {
			_, msg := decodeToolError(t, bad)
			t.Errorf("alice's own session was refused: %s", msg)
		}
	})
	t.Run("a peer may not", func(t *testing.T) {
		bad := check(t, namespaceContext("bob"), "shell_input", map[string]any{"shell_id": alice})
		if bad == nil {
			t.Fatal("bob must not reach alice's session")
		}
		code, msg := decodeToolError(t, bad)
		// The code is `forbidden` and not `session_not_found`: the session exists,
		// this caller simply may not touch it, and a "not found" would send a reader
		// looking for a bug in their ids.
		if code != CodeForbidden {
			t.Errorf("code = %q, want %q", code, CodeForbidden)
		}
		if !strings.Contains(msg, "namespace bob") {
			t.Errorf("refusal should name the caller's namespace: %s", msg)
		}
		if strings.Contains(msg, "alice") {
			t.Errorf("refusal must not leak the owner's namespace: %s", msg)
		}
	})
	t.Run("and not by a locator or the other argument", func(t *testing.T) {
		// Every spelling resolves to the same session, so every spelling has to
		// meet the same gate; a refusal a caller can walk around by respelling the
		// id is not a refusal.
		for _, args := range []map[string]any{
			{"session_id": alice},
			{"shell_id": "termcp://#" + alice},
			{"session_id": "termcp://#" + alice},
		} {
			if bad := check(t, namespaceContext("bob"), "shell_output", args); bad == nil {
				t.Errorf("bob reached alice's session through %v", args)
			}
		}
	})
	t.Run("the operator may", func(t *testing.T) {
		// The browser and the operator's own agent claimed nothing; they are the
		// owner of the instance rather than a tenant in it.
		if bad := check(t, context.Background(), "shell_input", map[string]any{"shell_id": alice}); bad != nil {
			_, msg := decodeToolError(t, bad)
			t.Errorf("the operator was refused their own instance: %s", msg)
		}
	})
}

// An extension session is infrastructure, so a namespaced caller may not reach it
// even though the operator can: without this, a tenant could drive the MCP server
// every other extension shares.
func TestGuardRefusesInfrastructureToNamespacedCallers(t *testing.T) {
	s := newTestServer(t)
	infra, err := s.sessMgr.Create(session.Config{
		Command: testShell(), Mode: api.ModePipe, Name: "ext", SSHConfig: "internal", Rows: 24, Cols: 80,
		Namespace: ns.ForExtensions("internal"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if bad := (toolGuard{server: s}).check(namespaceContext("alice"), "shell_input", lookupGate("shell_input"),
		makeRequest(map[string]any{"shell_id": infra.ID})); bad == nil {
		t.Error("a namespaced caller must not reach an extension's session")
	}
	if bad := (toolGuard{server: s}).check(context.Background(), "shell_input", lookupGate("shell_input"),
		makeRequest(map[string]any{"shell_id": infra.ID})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("the operator holds extension handles by design: %s", msg)
	}
}

// A session started by a namespaced caller carries the caller's namespace, which
// is what makes the isolation outlive the request that created it.
func TestSessionStartTagsTheCallersNamespace(t *testing.T) {
	s := newTestServer(t)
	res, err := withLogging("session_start", s.handleStartSession, s)(namespaceContext("alice"), makeRequest(map[string]any{
		"ssh_config": "internal",
		"command":    testShell(),
		"mode":       "pipe",
	}))
	if err != nil || res.IsError {
		t.Fatalf("session_start failed: %v %+v", err, res)
	}
	id := parseResult(t, res)["session_id"].(string)
	sess := s.sessMgr.Get(id)
	if sess == nil {
		t.Fatal("session not found")
	}
	if got := sess.Info().Namespace; got != ns.ForCaller("alice") {
		t.Errorf("namespace = %q, want %q", got, ns.ForCaller("alice"))
	}
}

// The rule itself, in the two halves that differ — reach and listing — pinned
// without a server, because it is the vocabulary every surface asks and a
// loosening here would silently widen all of them.
func TestNamespaceRules(t *testing.T) {
	cases := []struct {
		namespace string
		caller    string
		reachable bool
		listable  bool
	}{
		// The operator: everything ordinary is theirs — including an agent's own
		// sessions, which are ordinary sessions with a tag — while infrastructure is
		// hidden from the listing but still reachable by a handle they already hold.
		{namespace: "", caller: "", reachable: true, listable: true},
		{namespace: ns.ForCaller("alice"), caller: "", reachable: true, listable: true},
		{namespace: ns.ForExtensions("internal"), caller: "", reachable: true, listable: false},
		// A tenant: exactly its own, whichever half is asked.
		{namespace: ns.ForCaller("alice"), caller: ns.ForCaller("alice"), reachable: true, listable: true},
		{namespace: ns.ForCaller("bob"), caller: ns.ForCaller("alice"), reachable: false, listable: false},
		{namespace: "", caller: ns.ForCaller("alice"), reachable: false, listable: false},
		{namespace: ns.ForExtensions("internal"), caller: ns.ForCaller("alice"), reachable: false, listable: false},
		// A claim that could not be honoured matches nothing, so a mistyped name
		// isolates its owner rather than handing them the shared view.
		{namespace: "", caller: invalidNamespace, reachable: false, listable: false},
		{namespace: ns.ForCaller("alice"), caller: invalidNamespace, reachable: false, listable: false},
	}
	for _, tc := range cases {
		t.Run(tc.namespace+"|"+tc.caller, func(t *testing.T) {
			if got := ns.Reachable(tc.namespace, tc.caller); got != tc.reachable {
				t.Errorf("Reachable(%q, %q) = %v, want %v", tc.namespace, tc.caller, got, tc.reachable)
			}
			if got := ns.Listable(tc.namespace, tc.caller); got != tc.listable {
				t.Errorf("Listable(%q, %q) = %v, want %v", tc.namespace, tc.caller, got, tc.listable)
			}
		})
	}
}

// A caller name travels in a header and lands in a stored manifest, so the
// charset is narrow on purpose; an absent name is the default and not an error.
func TestValidCallerName(t *testing.T) {
	for _, ok := range []string{"", "  ", "alice", "build-agent-2", "a.b_c", "A9"} {
		if err := ns.ValidCallerName(ok); err != nil {
			t.Errorf("ValidCallerName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a/b", "a b", "a:b", "ns:alice", "über", string(make([]byte, 65))} {
		if err := ns.ValidCallerName(bad); err == nil {
			t.Errorf("ValidCallerName(%q) = nil, want a refusal", bad)
		}
	}
}

// The infrastructure root and the caller root are disjoint, so an agent that asks
// for the extension layer's namespace by name cannot be handed it.
func TestCallerNamespaceIsDisjointFromInfrastructure(t *testing.T) {
	if ns.ForCaller(ns.Extensions) != ns.ForExtensions("")[:0]+ns.Callers+":"+ns.Extensions {
		t.Fatal("the caller root must prefix every caller namespace")
	}
	if ns.IsExtensions(ns.ForCaller("alice")) {
		t.Error("a caller namespace must not read as an extension namespace")
	}
	if _, ok := ns.CallerOf(ns.ForExtensions("internal")); ok {
		t.Error("an extension namespace must not read as a caller namespace")
	}
	if name, ok := ns.CallerOf(ns.ForCaller("alice")); !ok || name != "alice" {
		t.Errorf("CallerOf(ns:alice) = %q, %v; want alice, true", name, ok)
	}
}
