package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/webui"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// callMCP dispatches one JSON-RPC request through the MCP server's message
// handler and returns the raw "result" payload.
func callMCP(t *testing.T, s *Server, method string, params map[string]any) json.RawMessage {
	t.Helper()
	return callMCPCtx(t, s, context.Background(), method, params)
}

// callMCPCtx is callMCP with a caller-supplied context, so a test can carry the
// request origin the transports install (see originContext).
func callMCPCtx(t *testing.T, s *Server, ctx context.Context, method string, params map[string]any) json.RawMessage {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	return dispatchMCP(t, s, ctx, req)
}

// dispatchMCP runs one already-built JSON-RPC request under ctx.
func dispatchMCP(t *testing.T, s *Server, ctx context.Context, req map[string]any) json.RawMessage {
	t.Helper()
	res, err := dispatchMCPErr(t, s, ctx, req)
	if err != "" {
		t.Fatalf("%v failed: %s", req["method"], err)
	}
	return res
}

// dispatchMCPErr runs one request and returns the error message (or empty on success).
func dispatchMCPErr(t *testing.T, s *Server, ctx context.Context, req map[string]any) (json.RawMessage, string) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg := s.mcpServer.HandleMessage(ctx, raw)
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatalf("bad JSON-RPC envelope %s: %v", b, err)
	}
	if envelope.Error != nil {
		return nil, envelope.Error.Message
	}
	return envelope.Result, ""
}

// docsTestServer returns a server wired with the shipped docs assets and a
// fixed base URL, without starting the HTTP listener.
func docsTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.SetDocsFS(webui.Assets())
	s.baseURL = "http://127.0.0.1:18765"
	s.registerDocs()
	return s
}

// TestDocsAssetsExist guards against a documentation asset being renamed or
// dropped: the HTTP-served reference must exist and be non-empty.
func TestDocsAssetsExist(t *testing.T) {
	b, err := fs.ReadFile(webui.Assets(), "api.md")
	if err != nil {
		t.Fatalf("api.md missing from embedded assets: %v (run `make sync-assets`)", err)
	}
	if len(b) == 0 {
		t.Fatal("api.md is empty")
	}
}

// TestNoResourceCapability pins the decision that resources are gone.
//
// MCP used to publish api.md as a resource. With one tool whose argument is a
// command line that describes itself, a client has nothing to list: it sends
// `help`, it does not read the REST endpoint table. Claiming the capability while
// listing nothing would be worse than not claiming it -- a client that branches on
// the capability would be sent down a path with no content behind it.
func TestNoResourceCapability(t *testing.T) {
	s := docsTestServer(t)

	// The capability must not be advertised at initialize. Asserted on the
	// protocol reply rather than on a server getter: what a client branches on is
	// the JSON it received.
	var init struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(callMCP(t, s, "initialize", nil), &init); err != nil {
		t.Fatal(err)
	}
	if _, ok := init.Capabilities["resources"]; ok {
		t.Error("initialize advertises a resources capability but no resources are served")
	}

	// And a listing, if a client asks anyway, is refused by the protocol layer
	// rather than answered with an empty list: an empty list says "this server has
	// no docs", a refusal says "this server does not do resources".
	_, errMsg := dispatchMCPErr(t, s, context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	if errMsg == "" {
		t.Error("resources/list must be refused now that no resources are served")
	}
}

// TestLearnAPIPrompt verifies the prompt exists and points at this instance's docs.
func TestLearnAPIPrompt(t *testing.T) {
	s := docsTestServer(t)

	var prompts struct {
		Prompts []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"prompts"`
	}
	if err := json.Unmarshal(callMCP(t, s, "prompts/list", nil), &prompts); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range prompts.Prompts {
		if p.Name == "learn-api" {
			found = true
			if !strings.Contains(p.Description, "termcp") {
				t.Errorf("learn-api description looks empty: %q", p.Description)
			}
		}
	}
	if !found {
		t.Fatal("prompts/list misses learn-api")
	}

	get := callMCP(t, s, "prompts/get", map[string]any{
		"name":      "learn-api",
		"arguments": map[string]string{"task": "restart nginx on prod"},
	})
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(get, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) == 0 {
		t.Fatalf("prompts/get returned no messages: %s", get)
	}
	text := got.Messages[0].Content.Text
	// The prompt used to hand the agent a written curl skill. It now points at the
	// instance's own command space instead, so what it must contain is the entrance
	// and the self-describing calls — a written tutorial is exactly the thing that
	// goes stale.
	for _, want := range []string{"http://127.0.0.1:18765/api.md", "/api/term", `{"cmd":"help"}`, "restart nginx on prod"} {
		if !strings.Contains(text, want) {
			t.Errorf("learn-api prompt misses %q:\n%s", want, text)
		}
	}
	if got.Messages[0].Role != string(mcpgo.RoleUser) {
		t.Errorf("expected user role, got %q", got.Messages[0].Role)
	}
}

// originContext fakes what the transports' context hook installs: the origin of
// the request being answered.
func originContext(origin string) context.Context {
	return context.WithValue(context.Background(), originKey{}, origin)
}

// originFromRequest is the whole derivation: where the request was dialed (or
// what a proxy recorded in X-Forwarded-Host after rewriting Host) and the scheme
// it arrived on.
func TestOriginFromRequest(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		fwdHost string
		tls     bool
		proto   string
		want    string
	}{
		{name: "plain host", host: "127.0.0.1:18765", want: "http://127.0.0.1:18765"},
		{name: "lan ip", host: "192.168.1.9:9000", want: "http://192.168.1.9:9000"},
		{name: "no port", host: "termcp.example.com", want: "http://termcp.example.com"},
		{name: "tls", host: "termcp.example.com", tls: true, want: "https://termcp.example.com"},
		{name: "forwarded proto", host: "termcp.example.com", proto: "https", want: "https://termcp.example.com"},
		{name: "forwarded proto case", host: "termcp.example.com", proto: "HTTPS", want: "https://termcp.example.com"},
		{name: "forwarded http stays http", host: "termcp.example.com", proto: "http", want: "http://termcp.example.com"},
		{name: "forwarded host wins", host: "127.0.0.1:18765", fwdHost: "public.example.com", want: "http://public.example.com"},
		{name: "forwarded host with port", host: "127.0.0.1:18765", fwdHost: "public.example.com:8443", want: "http://public.example.com:8443"},
		{name: "forwarded host first of a list", host: "127.0.0.1:18765", fwdHost: "public.example.com, inner.example", want: "http://public.example.com"},
		{name: "forwarded host padded", host: "127.0.0.1:18765", fwdHost: "  public.example.com  ", want: "http://public.example.com"},
		{name: "empty forwarded host falls back", host: "b.example", fwdHost: " ", want: "http://b.example"},
		{name: "empty host", host: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodPost, "http://placeholder/stream", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = c.host
			if c.fwdHost != "" {
				r.Header.Set("X-Forwarded-Host", c.fwdHost)
			}
			if c.proto != "" {
				r.Header.Set("X-Forwarded-Proto", c.proto)
			}
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := originFromRequest(r); got != c.want {
				t.Errorf("originFromRequest(host=%q fwd=%q tls=%v proto=%q) = %q, want %q", c.host, c.fwdHost, c.tls, c.proto, got, c.want)
			}
		})
	}
}

// listToolsRaw dispatches tools/list and hands back the raw result, for callers
// that need the payload itself rather than the parsed tools.
func listToolsRaw(t *testing.T, s *Server, ctx context.Context) json.RawMessage {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s.mcpServer.HandleMessage(ctx, raw))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatalf("bad JSON-RPC envelope %s: %v", b, err)
	}
	return envelope.Result
}

// listTools runs tools/list under ctx and returns the tools.
func listTools(t *testing.T, s *Server, ctx context.Context) []mcpgo.Tool {
	t.Helper()
	var result struct {
		Tools []mcpgo.Tool `json:"tools"`
	}
	if err := json.Unmarshal(listToolsRaw(t, s, ctx), &result); err != nil {
		t.Fatal(err)
	}
	return result.Tools
}

// notifyDescription returns the notify_user tool description as the model would
// receive it under ctx, and fails if the tool is missing from the listing at all.
func notifyDescription(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	return directiveHelpText(t, s, "notify")
}

// directiveHelpText is the model-facing rule text for one directive. It replaces
// the per-tool description the specialised surface used to carry: with one tool
// the rule lives in `help <directive>`, reached at the moment of use instead of
// paid for on every tools/list.
func directiveHelpText(t *testing.T, s *Server, op string) string {
	t.Helper()
	text := toolResultText(termCall(t, s, map[string]any{"cmd": "help " + op}))
	if !strings.Contains(text, "TERM/1 200 OK") {
		t.Fatalf("help %s failed: %s", op, text)
	}
	return text
}

// TestNotifyHelpRequiresProactiveHumanAlert keeps the behavioral rule in the
// text a caller reads when it looks the directive up.
//
// The carrier changed with issue #117: there is one tool, so there is no
// per-operation description left to hang a rule on. It lives in `help notify`,
// reached at the moment of use instead of paid for on every tools/list. The
// assertions are on the RULE, not its old wording.
func TestNotifyHelpRequiresProactiveHumanAlert(t *testing.T) {
	desc := notifyDescription(t, docsTestServer(t), originContext("http://127.0.0.1:18765"))
	for _, want := range []string{
		"BEFORE asking the human for anything",
		"password", "sudo", "MFA", "passphrase",
		"confirmation", "approval",
		"--sticky",
		"long task ends or fails",
		"never poll",
		// The await rule is useless without knowing what not to do while waiting.
		"never poll",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("help notify misses %q: "+want, desc)
		}
	}
	if !strings.Contains(mcpServerInstructions, "before any password/sudo/passphrase/MFA") ||
		!strings.Contains(mcpServerInstructions, "notify FIRST") ||
		!strings.Contains(mcpServerInstructions, "HARD BOUNDARY") {
		t.Errorf("initialize instructions no longer require notify before human input:%s", mcpServerInstructions)
	}
}

// Review mode's rule belongs in the initialize instructions: the reply it
// describes (review_pending) answers a call the model just made, so the rule has
// to be in context before that call. The tool RESULT repeats the operational half
// on purpose, but the tool LISTING must not: notify_user's description is sent on
// every tools/list and is not where a held call is explained.
func TestInstructionsCarryReviewPendingRule(t *testing.T) {
	for _, want := range []string{"review_pending", "approval_id", "do NOT poll", "resubmit"} {
		if !strings.Contains(mcpServerInstructions, want) {
			t.Errorf("instructions no longer explain review mode %q:\n%s", want, mcpServerInstructions)
		}
	}

	desc := notifyDescription(t, docsTestServer(t), originContext("http://127.0.0.1:18765"))
	for _, gone := range []string{"review_pending", "resubmit"} {
		if strings.Contains(desc, gone) {
			t.Errorf("help notify repeats the review-mode rule %q; it belongs in the instructions and the tool result", gone)
		}
	}
}

// The rule reaches the model in three places, each doing one job: the initialize
// instructions state it as a rule, and the two tool results restate it for the
// moment it is met. The two replies are NOT duplicates of each other though --
// they need opposite next steps, so they share the retry rule (reviewWaitTail)
// and differ everywhere else: staged text still needs its ending key, queued or
// held work must not be retried and is checked later via shell_output.
func TestReviewPendingRepliesStateTheNextStep(t *testing.T) {
	held := toolResultText(reviewPendingResult(false))
	if !strings.Contains(held, "ending key") {
		t.Errorf("the staged reply must ask for the ending key:\n%s", held)
	}
	// The retry rule belongs to the queued case only: a model that read it after
	// staging text would abandon a command line it still has to finish.
	if strings.Contains(held, reviewWaitTail) {
		t.Errorf("the staged reply must not forbid the ending key it just asked for:\n%s", held)
	}

	queued := toolResultText(reviewPendingResult(true))
	if !strings.Contains(queued, "approval_id") || !strings.Contains(queued, "do not poll") || !strings.Contains(queued, "WAIT") {
		t.Errorf("the queued reply must forbid a retry loop:\n%s", queued)
	}
	// A held operation shares the queued wording, so one constant keeps the two
	// paths from forbidding the same retry loop in two different ways.
	if op := toolResultText(reviewPendingOperationResult("write /etc/hosts", nil)); !strings.Contains(op, reviewWaitTail) {
		t.Errorf("a held operation must carry the same retry rule as a held command line:\n%s", op)
	}
	// The two replies differ: a model that reads "do not retry" after staging
	// text would abandon a command line it still has to finish.
	if held == queued {
		t.Error("staged and queued replies must differ; they need opposite next steps")
	}
}

// The tool listing is the one channel guaranteed to reach the model: a client
// that drops it cannot call any tool. So the instance address rides on the
// description of the single entry -- unlike the instructions, which are optional
// in MCP and routinely discarded.
func TestToolListingCarriesInstanceAddress(t *testing.T) {
	const origin = "http://10.1.2.3:18994"
	s := New(nil, nil, nil, nil, "test")

	// There is one tool, so "only this one carries the address" is checked as
	// "exactly one tool, and it carries it": the decoration must not spread.
	tools := listTools(t, s, originContext(origin))
	if len(tools) != 1 {
		t.Fatalf("%d tools; the surface is one entry", len(tools))
	}
	if !strings.Contains(tools[0].Description, origin) {
		t.Errorf("the %s description misses the address %q:\n%s", tools[0].Name, origin, tools[0].Description)
	}
}

// The decoration must be computed per request and never accumulate: repeated
// listings under different hosts each carry exactly their own address.
func TestToolListingAddressIsPerRequestAndNotAccumulated(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	for _, origin := range []string{"http://a.example:1", "http://b.example:2", "http://a.example:1"} {
		tools := listTools(t, s, originContext(origin))
		if len(tools) != 1 {
			t.Fatalf("%d tools", len(tools))
		}
		desc := tools[0].Description
		if n := strings.Count(desc, "This instance's Web UI:"); n != 1 {
			t.Fatalf("address appended %d times for %s:\n%s", n, origin, desc)
		}
		if !strings.Contains(desc, origin+"/") {
			t.Errorf("listing for %s carries the wrong address:\n%s", origin, desc)
		}
		if strings.Contains(desc, "a.example") && strings.Contains(desc, "b.example") {
			t.Errorf("listing mixes two origins:\n%s", desc)
		}
	}
}

// With no host anywhere the address falls back to the discovered bind address
// rather than a dangling label or an invented name. (An in-process call reaches
// this path too, which is why the fallback exists at all.)
func TestToolListingFallsBackToBindAddress(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	s.baseURL = "http://127.0.0.1:18765"

	if desc := termDescriptionFor(t, s, nil); !strings.Contains(desc, "http://127.0.0.1:18765/") {
		t.Errorf("description misses the bind address:\n%s", desc)
	}
}

// termDescriptionFor is the single tool's description as the model receives it.
func termDescriptionFor(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	tools := listTools(t, s, ctx)
	if len(tools) != 1 {
		t.Fatalf("%d tools; the surface is one entry", len(tools))
	}
	return tools[0].Description
}

// No address at all (no host to answer, no bind address yet) must leave the
// description exactly as written rather than gain a dangling label.
func TestToolListingUnchangedWithoutOrigin(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	if desc := termDescriptionFor(t, s, nil); strings.Contains(desc, "This instance's Web UI:") {
		t.Errorf("description advertises an address that was never set:\n%s", desc)
	}
}

// The instructions are the fixed rule set and must NOT carry the instance
// address: they are optional in MCP, frequently dropped, and a per-request value
// baked into them would be a second source of truth. The address lives on the
// `term` description (see the tests above) and nowhere else.
func TestInstructionsDoNotCarryInstanceAddress(t *testing.T) {
	for _, want := range []string{"This instance:", "http://127.0.0.1", "http://localhost"} {
		if strings.Contains(mcpServerInstructions, want) {
			t.Errorf("instructions carry instance-address machinery %q; it belongs on the tool listing", want)
		}
	}
}

// The unit tests above exercise the hook directly; this one holds the wire the
// hook hangs on. A transport that never installs it would keep every other test
// green while no real client ever saw its own address.
func TestStreamableTransportAdvertisesRequestHost(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	req := func(id int, method string, session string) *http.Request {
		t.Helper()
		r, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(
			`{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"method":"`+method+`","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Host = "termcp.lan.example:9000" // what the client dialed
		if session != "" {
			r.Header.Set("Mcp-Session-Id", session)
		}
		return r
	}

	rr, err := ts.Client().Do(req(1, "initialize", ""))
	if err != nil {
		t.Fatal(err)
	}
	session := rr.Header.Get("Mcp-Session-Id")
	initBody, err := io.ReadAll(rr.Body)
	rr.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	body := string(initBody)
	if session == "" {
		t.Fatalf("initialize returned no session id: %s", body)
	}
	if strings.Contains(body, "termcp.lan.example") {
		t.Errorf("the initialize reply carries the request address; the instructions must stay a fixed rule set:\n%s", body)
	}

	rr2, err := ts.Client().Do(req(2, "tools/list", session))
	if err != nil {
		t.Fatal(err)
	}
	listBody, err := io.ReadAll(rr2.Body)
	rr2.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	listing := string(listBody)
	if !strings.Contains(listing, "This instance's Web UI: http://termcp.lan.example:9000/") {
		t.Errorf("tools/list does not carry the address the client dialed:\n%s", listing)
	}
}

// The unit tests above exercise the rule directly; this one holds the wire it
// hangs on. A transport that never installed the namespace hook would keep every
// other test green while no real client was ever scoped — and the failure mode is
// the worst kind: isolation that silently is not one, because every caller keeps
// seeing and reaching everything.
func TestStreamableTransportScopesByNamespaceHeader(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	// One MCP session per caller, each carrying its own header. The header is what
	// a client sets once and expects to hold for the whole conversation, so a test
	// that varies it per request would not be measuring the thing that matters.
	caller := func(name string) *mcpSession {
		t.Helper()
		return openMCPSession(t, ts, name)
	}

	alice := caller("alice")
	bob := caller("bob")
	owner := openMCPSession(t, ts, "")

	aliceSid := alice.startSession(t)
	bobSid := bob.startSession(t)

	// Each caller lists its own and only its own.
	t.Run("listings are disjoint", func(t *testing.T) {
		got := alice.listSessions(t)
		if !got[aliceSid] || got[bobSid] {
			t.Errorf("alice's list = %v, want exactly %s", got, aliceSid)
		}
		got = bob.listSessions(t)
		if !got[bobSid] || got[aliceSid] {
			t.Errorf("bob's list = %v, want exactly %s", got, bobSid)
		}
	})

	// The owner sees both: an agent's session is an ordinary session with a tag,
	// and the person running the instance is not a tenant in it.
	t.Run("the owner sees both", func(t *testing.T) {
		got := owner.listSessions(t)
		if !got[aliceSid] || !got[bobSid] {
			t.Errorf("the owner's list = %v, want both agent sessions", got)
		}
	})

	t.Run("a peer cannot drive the other's session", func(t *testing.T) {
		// The shell text is a VALUE: `input` takes exactly one variable reference.
		res := bob.callTermArgs(t, "input "+aliceSid+":1 $1", []any{"echo pwned"})
		if !res.IsError {
			t.Fatal("bob wrote to alice's session")
		}
		text := res.Content[0].(mcpgo.TextContent).Text
		if !strings.Contains(text, "ns:bob") && !strings.Contains(text, "namespace bob") {
			t.Errorf("refusal does not name the caller's namespace: %s", text)
		}
	})

	t.Run("the owner may drive either", func(t *testing.T) {
		res := owner.callTerm(t, "get "+aliceSid)
		if res.IsError {
			t.Fatalf("the owner was refused: %+v", res)
		}
	})
}

// mcpSession is one caller's conversation with the instance: its own HTTP client
// state (the MCP session id) and its own namespace header.
type mcpSession struct {
	t       *testing.T
	ts      *httptest.Server
	session string
	name    string
	lastID  int
}

func openMCPSession(t *testing.T, ts *httptest.Server, name string) *mcpSession {
	t.Helper()
	c := &mcpSession{t: t, ts: ts, name: name}
	r := c.request(map[string]any{
		"jsonrpc": "2.0", "id": c.nextID(), "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "nstest", "version": "1"},
		},
	})
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	c.session = resp.Header.Get("Mcp-Session-Id")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if c.session == "" {
		t.Fatal("initialize returned no session id")
	}
	if name != "" {
		// The claim has to be accepted on the FIRST request too, or the namespace a
		// caller asked for would apply only from its second message — and the session
		// it started while unnamed would be somebody else's.
		if err := ns.ValidCallerName(name); err != nil {
			t.Fatalf("test asked for an invalid namespace %q: %v", name, err)
		}
	}
	return c
}

func (c *mcpSession) nextID() int {
	c.lastID++
	return c.lastID
}

// request builds one JSON-RPC POST with this caller's namespace header, exactly
// as the stdio bridge and an SDK client do.
func (c *mcpSession) request(payload map[string]any) *http.Request {
	c.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, c.ts.URL, strings.NewReader(string(body)))
	if err != nil {
		c.t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		r.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.name != "" {
		r.Header.Set(NamespaceHeader, c.name)
	}
	return r
}

// callTool performs one tool call and returns the parsed result. Only the
// JSON-body reply shape is handled: these tests never register a notification,
// so the transport has no reason to answer in SSE.
func (c *mcpSession) callTool(tool string, args map[string]any) *mcpgo.CallToolResult {
	c.t.Helper()
	r := c.request(map[string]any{
		"jsonrpc": "2.0", "id": c.nextID(), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	resp, err := c.ts.Client().Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var envelope struct {
		Result *mcpgo.CallToolResult      `json:"result"`
		Error  *mcpgo.JSONRPCErrorDetails `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		c.t.Fatalf("decoding %s reply: %v (%s)", tool, err, body)
	}
	if envelope.Error != nil {
		c.t.Fatalf("%s failed at the JSON-RPC level: %s", tool, envelope.Error.Message)
	}
	if envelope.Result == nil {
		c.t.Fatalf("%s returned no result: %s", tool, body)
	}
	return envelope.Result
}

func (c *mcpSession) startSession(t *testing.T) string {
	t.Helper()
	res := c.callTerm(t, "connect internal --argv "+quoteArgv(append([]any{testShell()}, testShellIdleArgs()...)))
	if res.IsError {
		t.Fatalf("pipe failed: %+v", res)
	}
	// The reply's Handle is now always `<session>:<index>` — a handle names a
	// channel, including the primary one, so that a channel handle and a session
	// id have different shapes. These tests scope by SESSION id, so strip the
	// index here once rather than at every use.
	text := res.Content[0].(mcpgo.TextContent).Text
	for _, line := range strings.Split(text, "\n") {
		if h, ok := strings.CutPrefix(line, "Handle: "); ok {
			if sid, _, found := strings.Cut(h, ":"); found {
				return sid
			}
			return h
		}
	}
	t.Fatalf("pipe reply carries no Handle:\n%s", text)
	return ""
}

// listSessions returns the set of session ids this caller can see, read from the
// `ps` listing (one handle per line, first column).
func (c *mcpSession) listSessions(t *testing.T) map[string]bool {
	t.Helper()
	res := c.callTerm(t, "ls")
	if res.IsError {
		t.Fatalf("ls failed: %+v", res)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	out := map[string]bool{}
	body := text
	if i := strings.Index(text, "\n\n"); i >= 0 {
		body = text[i+2:]
	}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		h := fields[0]
		if sid, _, ok := strings.Cut(h, ":"); ok {
			h = sid
		}
		out[h] = true
	}
	return out
}

// callTerm drives the single tool through the wire.
func (c *mcpSession) callTerm(t *testing.T, cmd string) *mcpgo.CallToolResult {
	t.Helper()
	return c.callTool(termToolName, map[string]any{"cmd": cmd})
}

// callTermArgs is callTerm for a payload that has to arrive as a VALUE: `run` and
// `input` take exactly one variable reference, so the shell line travels in args.
func (c *mcpSession) callTermArgs(t *testing.T, cmd string, args []any) *mcpgo.CallToolResult {
	t.Helper()
	return c.callTool(termToolName, map[string]any{"cmd": cmd, "args": args})
}

// The learn-api prompt names the address the client dialed, so a client that
// reached the instance through a proxy or LAN name is given the address it can
// actually open.
func TestLearnAPIPromptFollowsRequestOrigin(t *testing.T) {
	const origin = "https://termcp.example.com"
	s := docsTestServer(t)
	ctx := originContext(origin)

	get := callMCPCtx(t, s, ctx, "prompts/get", map[string]any{"name": "learn-api"})
	var got struct {
		Messages []struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(get, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) == 0 {
		t.Fatalf("prompts/get returned nothing: %s", get)
	}
	text := got.Messages[0].Content.Text
	for _, want := range []string{origin + "/api.md", "/api/term"} {
		if !strings.Contains(text, want) {
			t.Errorf("learn-api prompt misses %s: %s", want, text)
		}
	}
	if strings.Contains(text, "127.0.0.1:18765") {
		t.Errorf("learn-api prompt names the bind address instead of the caller's: %s", text)
	}
}

func TestInstructionsPairWaitingOnAHumanWithNotify(t *testing.T) {
	for _, want := range []string{"Human at a prompt", "notify"} {
		if !strings.Contains(mcpServerInstructions, want) {
			t.Errorf("initialize instructions no longer tell the agent how to wait on a human: missing %q", want)
		}
	}
	// Both halves must be in the boundary rule's neighbourhood: the waking rule is
	// only correct when it follows the notification, and an agent reading one
	// without the other would either poll or stay silent.
	if strings.Index(mcpServerInstructions, "Human at a prompt") < strings.Index(mcpServerInstructions, "HARD BOUNDARY") {
		t.Error("the wait-on-a-human rule moved above the notify boundary it depends on")
	}
}

// messageDescription returns the span directive's rule text, which now decodes
// the letters: there is one tool, so the decoding lives in `help span`.
func messageDescription(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	return directiveHelpText(t, s, "span")
}

// Issue #87: the agent can only tell whether a human has touched the terminal by
// reading the timeline index, and every span it gets back carries a one-letter
// status. The letters are terse by design (they are written to log.jsonl for
// every status change), so a description that shows the fields without decoding
// them leaves the agent holding data it cannot interpret -- `i` versus `a` is
// precisely the question the issue asks, and nothing on the wire explained it.
//
// The decoding is checked against the live handlers, not only against the prose:
// the assertions below drive an AI input and a human input through the real
// paths and require the documented letters to be what comes back. A description
// that drifted from the statuses the server emits would still satisfy a string
// match, and that drift is the whole risk here.
func TestMessageDescriptionDecodesTheStatusLetters(t *testing.T) {
	desc := messageDescription(t, docsTestServer(t), originContext("http://127.0.0.1:18765"))
	for _, want := range []string{
		"o output",   // the shell's own bytes
		"a AI input", // the agent's own writes
		"i human input",
		// The reading the issue is about: which spans prove a person operated
		// the terminal, given that both sources echo identically.
		"mean a person operated",
		// Zero-length marks: an agent that expects bytes here would read the
		// wrong span for the input it is looking for. The pointer is now a directive
		// with a range flag, not a per-tool parameter.
		"--cursor",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("help span misses %q:\n%s", want, desc)
		}
	}

	// The letters the description names must be the letters the handlers record.
	s, _, _, _ := newTestServerWithHistory(t)
	sid, shellID := startTestSession(t, s)

	// An agent's line, through the MCP path.
	if _, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID, "text": "echo ai-was-here",
	})); err != nil {
		t.Fatalf("handleSendInput: %v", err)
	}
	if _, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID, "key": "enter",
	})); err != nil {
		t.Fatalf("handlePressKey: %v", err)
	}

	// A human's line, through the same call the browser's WebSocket makes.
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session vanished")
	}
	shell := sess.GetChildShell(shellID)
	if shell == nil {
		t.Fatal("shell vanished")
	}
	if err := shell.SendTerminalBytes([]byte("echo human-was-here"), true); err != nil {
		t.Fatalf("the human input path failed: %v", err)
	}

	res, err := s.handleMessageOps(context.Background(), makeRequest(map[string]any{
		"action": "list", "session_id": sid, "shell_id": shellID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("message(list) failed: %v %+v", err, res)
	}
	spans, _ := parseResult(t, res)["spans"].([]any)
	seen := map[string]bool{}
	for _, raw := range spans {
		span, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		status, _ := span["status"].(string)
		seen[status] = true
		// Input spans are marks, not byte ranges: the description promises this,
		// and a reader paging with offset=start would loop forever otherwise.
		if status == string(api.LogAIInput) || status == string(api.LogAPIInput) {
			start := getFloat64(span, "start", -1)
			end := getFloat64(span, "end", -1)
			if start != end {
				t.Errorf("input span %q is not a zero-length mark: start=%v end=%v", status, start, end)
			}
		}
	}
	if !seen[string(api.LogAIInput)] {
		t.Errorf("no %q span was recorded for the agent's input; spans: %v", api.LogAIInput, seen)
	}
	if !seen[string(api.LogAPIInput)] {
		t.Errorf("no %q span was recorded for the human's input, so the description's claim is unverifiable; spans: %v", api.LogAPIInput, seen)
	}
}
