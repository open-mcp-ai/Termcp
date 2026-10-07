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

	"github.com/open-mcp-ai/termcp/internal/locator"
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
		t.Fatalf("%v failed: %s", req["method"], envelope.Error.Message)
	}
	return envelope.Result
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
// dropped: every registered resource must resolve to a non-empty file.
func TestDocsAssetsExist(t *testing.T) {
	assets := webui.Assets()
	if len(docResources) == 0 {
		t.Fatal("no doc resources registered")
	}
	for _, doc := range docResources {
		b, err := fs.ReadFile(assets, doc.path)
		if err != nil {
			t.Fatalf("doc %s missing from embedded assets: %v (run `make sync-assets`)", doc.path, err)
		}
		if len(b) == 0 {
			t.Fatalf("doc %s is empty", doc.path)
		}
		if doc.name == "" || doc.description == "" {
			t.Fatalf("doc %s needs a name and description", doc.path)
		}
	}
}

// TestResourcesListAndRead verifies the docs are discoverable and readable with
// URIs that are the instance's real HTTP addresses (same strings work for curl).
func TestResourcesListAndRead(t *testing.T) {
	s := docsTestServer(t)

	var list struct {
		Resources []struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			MIMEType    string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(callMCP(t, s, "resources/list", nil), &list); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"http://127.0.0.1:18765/api.md":    false,
		"http://127.0.0.1:18765/skills.md": false,
	}
	for _, r := range list.Resources {
		if _, ok := want[r.URI]; ok {
			want[r.URI] = true
		}
		if r.MIMEType != "text/markdown" {
			t.Errorf("resource %s mimeType = %q, want text/markdown", r.URI, r.MIMEType)
		}
	}
	for uri, seen := range want {
		if !seen {
			t.Errorf("resources/list misses %s", uri)
		}
	}

	var read struct {
		Contents []struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	result := callMCP(t, s, "resources/read", map[string]any{"uri": "http://127.0.0.1:18765/api.md"})
	if err := json.Unmarshal(result, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !strings.Contains(read.Contents[0].Text, "Termcp HTTP API") {
		t.Fatalf("unexpected resources/read payload: %s", result)
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
	for _, want := range []string{"http://127.0.0.1:18765/api.md", "http://127.0.0.1:18765/skills.md", "restart nginx on prod"} {
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
	for _, tool := range listTools(t, s, ctx) {
		if tool.Name == "notify_user" {
			return tool.Description
		}
	}
	t.Fatal("notify_user missing from tools/list")
	return ""
}

// TestNotifyUserDescriptionRequiresProactiveHumanAlert keeps the behavioral rule in
// the compact, model-facing description. The longer registration text is replaced
// by compactToolDescriptions before tools/list is sent, so testing tools.go alone
// would miss a regression that silently removes the instruction from the model.
func TestNotifyUserDescriptionRequiresProactiveHumanAlert(t *testing.T) {
	desc := notifyDescription(t, docsTestServer(t), originContext("http://127.0.0.1:18765"))
	for _, want := range []string{
		"CALL IT BEFORE ASKING THE HUMAN FOR ANYTHING",
		"password/sudo/MFA/passphrase",
		"confirmation, approval",
		"duration_seconds=0",
		"long task ends or fails",
		"delivered=0",
		// The asking half of the rule is stating the ask; the other half is not
		// inventing the answer while it is missing. A held review request relies on
		// this one too: the review replies only forbid retrying a REVIEWED call.
		"Once you have asked, WAIT",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("notify_user model-facing description misses %q:\n%s", want, desc)
		}
	}
	if !strings.Contains(mcpServerInstructions, "before any password/sudo/passphrase/MFA") ||
		!strings.Contains(mcpServerInstructions, "notify_user FIRST") ||
		!strings.Contains(mcpServerInstructions, "HARD BOUNDARY") {
		t.Errorf("initialize instructions no longer require notify_user before human input:\n%s", mcpServerInstructions)
	}
}

// Review mode's rule belongs in the initialize instructions: the reply it
// describes (review_pending) answers a call the model just made, so the rule has
// to be in context before that call. The tool RESULT repeats the operational half
// on purpose, but the tool LISTING must not: notify_user's description is sent on
// every tools/list and is not where a held call is explained.
func TestInstructionsCarryReviewPendingRule(t *testing.T) {
	for _, want := range []string{"review_pending", "no id to poll", "resubmit"} {
		if !strings.Contains(mcpServerInstructions, want) {
			t.Errorf("instructions no longer explain review mode %q:\n%s", want, mcpServerInstructions)
		}
	}

	desc := notifyDescription(t, docsTestServer(t), originContext("http://127.0.0.1:18765"))
	for _, gone := range []string{"review_pending", "resubmit"} {
		if strings.Contains(desc, gone) {
			t.Errorf("the notify_user description repeats the review-mode rule %q; it belongs in the instructions and the tool result", gone)
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
	if !strings.Contains(queued, "no id to poll") || !strings.Contains(queued, "WAIT") {
		t.Errorf("the queued reply must forbid a retry loop:\n%s", queued)
	}
	// A held operation shares the queued wording, so one constant keeps the two
	// paths from forbidding the same retry loop in two different ways.
	if op := toolResultText(reviewPendingOperationResult("write /etc/hosts")); !strings.Contains(op, reviewWaitTail) {
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
// description of the tool that exists to reach the human -- unlike the
// instructions, which are optional in MCP and routinely discarded.
func TestToolListingCarriesInstanceAddress(t *testing.T) {
	const origin = "http://10.1.2.3:18994"
	s := New(nil, nil, nil, nil, "test")

	for _, tool := range listTools(t, s, originContext(origin)) {
		if tool.Name == "notify_user" {
			continue
		}
		if strings.Contains(tool.Description, origin) {
			t.Errorf("tool %s carries the address; only notify_user should:\n%s", tool.Name, tool.Description)
		}
	}
	if notify := notifyDescription(t, s, originContext(origin)); !strings.Contains(notify, origin) {
		t.Errorf("notify_user description misses the address %q:\n%s", origin, notify)
	}
}

// The decoration must be computed per request and never accumulate: repeated
// listings under different hosts each carry exactly their own address.
func TestToolListingAddressIsPerRequestAndNotAccumulated(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	for _, origin := range []string{"http://a.example:1", "http://b.example:2", "http://a.example:1"} {
		notify := notifyDescription(t, s, originContext(origin))
		if n := strings.Count(notify, "This instance's Web UI:"); n != 1 {
			t.Fatalf("address appended %d times for %s:\n%s", n, origin, notify)
		}
		if !strings.Contains(notify, origin+"/") {
			t.Errorf("listing for %s carries the wrong address:\n%s", origin, notify)
		}
		if strings.Contains(notify, "a.example") && strings.Contains(notify, "b.example") {
			t.Errorf("listing mixes two origins:\n%s", notify)
		}
	}
}

// With no host anywhere the address falls back to the discovered bind address
// rather than a dangling label or an invented name. (An in-process call reaches
// this path too, which is why the fallback exists at all.)
func TestToolListingFallsBackToBindAddress(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	s.baseURL = "http://127.0.0.1:18765"

	if notify := notifyDescription(t, s, nil); !strings.Contains(notify, "http://127.0.0.1:18765/") {
		t.Errorf("description misses the bind address:\n%s", notify)
	}
}

// No address at all (no host to answer, no bind address yet) must leave the
// description exactly as written rather than gain a dangling label.
func TestToolListingUnchangedWithoutOrigin(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	if notify := notifyDescription(t, s, nil); strings.Contains(notify, "This instance's Web UI:") {
		t.Errorf("description advertises an address that was never set:\n%s", notify)
	}
}

// The instructions are the fixed rule set and must NOT carry the instance
// address: they are optional in MCP, frequently dropped, and a per-request value
// baked into them would be a second source of truth. The address lives on the
// notify_user description (see the tests above) and nowhere else.
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

// The resource listing travels the same wire as the tool listing, so it must
// name the same address. A transport that installed the origin hook for tools
// only would leave resources pointing at the bind address.
func TestStreamableTransportAdvertisesRequestHostInResources(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	s.SetDocsFS(webui.Assets())
	s.baseURL = "http://127.0.0.1:18765"
	s.registerDocs()
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	post := func(id int, method, session string) (string, string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"` + method + `","params":{}}`
		r, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Host = "termcp.lan.example:9000" // what the client dialed
		if session != "" {
			r.Header.Set("Mcp-Session-Id", session)
		}
		rr, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer rr.Body.Close()
		b, err := io.ReadAll(rr.Body)
		if err != nil {
			t.Fatal(err)
		}
		return rr.Header.Get("Mcp-Session-Id"), string(b)
	}

	session, _ := post(1, "initialize", "")
	if session == "" {
		t.Fatal("initialize returned no session id")
	}
	_, listing := post(2, "resources/list", session)
	if !strings.Contains(listing, "http://termcp.lan.example:9000/api.md") {
		t.Errorf("resources/list does not carry the address the client dialed:\n%s", listing)
	}
	if strings.Contains(listing, "127.0.0.1:18765") {
		t.Errorf("resources/list names the bind address instead of the caller's:\n%s", listing)
	}
}

// termcp:// is the locator scheme: termcp://<name> parses as an SSH entry, so an
// "instance" resource under it would read as a host called `instance` and
// collide with the namespace users paste into ssh_config args. The address is
// published through the notify_user description instead — never as a locator.
func TestNoLocatorShapedInstanceResource(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	s.SetDocsFS(webui.Assets())
	s.registerDocs()

	var list struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(callMCP(t, s, "resources/list", nil), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Resources) == 0 {
		t.Fatal("resources/list returned nothing; the guard would pass vacuously")
	}
	for _, r := range list.Resources {
		if strings.HasPrefix(r.URI, locator.Scheme) && !strings.Contains(r.URI, "shells/") {
			t.Errorf("resource %q lives in the locator namespace; it would parse as an SSH entry named %q", r.URI, strings.TrimPrefix(r.URI, locator.Scheme))
		}
	}
}

// One instance can be reached by several addresses at once, so every published
// address must follow the request that asked for it: resources/list, the
// resources/read reply and the learn-api prompt must name the same origin the
// notify_user description does (see TestToolListingCarriesInstanceAddress).
func TestResourcesFollowRequestOrigin(t *testing.T) {
	const origin = "https://termcp.example.com"
	s := docsTestServer(t)
	ctx := originContext(origin)

	var list struct {
		Resources []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(listResourcesRaw(t, s, ctx), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Resources) == 0 {
		t.Fatal("resources/list returned nothing")
	}
	for _, r := range list.Resources {
		if !strings.HasPrefix(r.URI, origin+"/") {
			t.Errorf("resource %s lists as %q, want it under the caller's origin %s", r.Name, r.URI, origin)
		}
	}

	// A client that reads the URI it was just handed must be routed to the
	// right document -- mcp-go matches resource URIs by exact string.
	var read struct {
		Contents []struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(callMCPCtx(t, s, ctx, "resources/read", map[string]any{"uri": origin + "/api.md"}), &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !strings.Contains(read.Contents[0].Text, "Termcp HTTP API") {
		t.Fatalf("reading %s/api.md returned %s", origin, read.Contents)
	}
	if read.Contents[0].URI != origin+"/api.md" {
		t.Errorf("resources/read echoed %q, want the address the caller used", read.Contents[0].URI)
	}

	// The learn-api prompt points at the same instance by the same address.
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
	for _, want := range []string{origin + "/api.md", origin + "/skills.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("learn-api prompt misses %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "127.0.0.1:18765") {
		t.Errorf("learn-api prompt names the bind address instead of the caller's:\n%s", text)
	}
}

// With no request origin at all (an in-process call, HTTP/1.0 without Host) the
// canonical startup address is still published, so the listing never goes blank.
func TestResourcesFallBackToBindAddress(t *testing.T) {
	s := docsTestServer(t)
	var list struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(listResourcesRaw(t, s, nil), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Resources) == 0 {
		t.Fatal("resources/list returned nothing")
	}
	for _, r := range list.Resources {
		if !strings.HasPrefix(r.URI, "http://127.0.0.1:18765/") {
			t.Errorf("resource URI %q does not fall back to the bind address", r.URI)
		}
	}
}

// listResourcesRaw dispatches resources/list under ctx and returns the result.
func listResourcesRaw(t *testing.T, s *Server, ctx context.Context) json.RawMessage {
	t.Helper()
	return callMCPCtx(t, s, ctx, "resources/list", nil)
}

// shellNotifyDescription returns the shell_notify tool description as the model
// receives it, failing if the tool is missing from the listing.
func shellNotifyDescription(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	for _, tool := range listTools(t, s, ctx) {
		if tool.Name == "shell_notify" {
			return tool.Description
		}
	}
	t.Fatal("shell_notify missing from tools/list")
	return ""
}

// Issue #82: the agent held a terminal at a sudo prompt and had no rule telling
// it that shell_notify is how it waits for the human, so it polled shell_output
// in a loop instead. The tool description is where the rule has to live — it is
// what the model reads when it considers the tool — and the payload is asserted
// through tools/list because compactToolDescriptions replaces the long-form text
// in tools.go before it reaches the wire, so checking either source file alone
// would pass while the model saw nothing.
func TestShellNotifyDescriptionTeachesWaitingOnAHuman(t *testing.T) {
	desc := shellNotifyDescription(t, docsTestServer(t), originContext("http://127.0.0.1:18765"))
	for _, want := range []string{
		// The situation: a prompt the human, not the agent, has to answer.
		"sudo/password",
		// The order notify_user and shell_notify stand in: tell the human it is
		// their turn first, then arm the wake-up. Selecting the wrong event is the
		// same trap the silence event sets (it fires on nobody speaking, which is
		// exactly the state the agent is in while it waits).
		"notify_user",
		"event=output",
		// Without this the agent still has a reason to keep polling.
		"stop polling",
		// The rule must not become permanent: the wake-up is one task's, not the
		// shell's.
		"unregister",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("shell_notify model-facing description misses %q:\n%s", want, desc)
		}
	}
}

// The initialize instructions carry the same rule in one line, because a model
// that never opens the shell_notify description still has to know that waiting
// on a human is a supported move rather than a hang. Rule 5 states the asking
// half (notify_user first); this asserts the answering half is next to it.
func TestInstructionsPairWaitingOnAHumanWithNotifyUser(t *testing.T) {
	for _, want := range []string{"Human at a prompt", "notify_user", "event=output"} {
		if !strings.Contains(mcpServerInstructions, want) {
			t.Errorf("initialize instructions no longer tell the agent how to wait on a human: missing %q", want)
		}
	}
	// Both halves must be in rule 5's neighbourhood: the waking rule is only
	// correct when it follows the notification, and an agent reading one without
	// the other would either poll or stay silent.
	if strings.Index(mcpServerInstructions, "Human at a prompt") < strings.Index(mcpServerInstructions, "HARD BOUNDARY") {
		t.Error("the wait-on-a-human rule moved above the notify_user boundary it depends on")
	}
}

// messageDescription returns the message tool description as the model receives
// it, failing if the tool is missing from the listing.
func messageDescription(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	for _, tool := range listTools(t, s, ctx) {
		if tool.Name == "message" {
			return tool.Description
		}
	}
	t.Fatal("message missing from tools/list")
	return ""
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
		"i/q = human operated",
		// Zero-length marks: an agent that expects bytes here would read the
		// wrong span for the input it is looking for.
		"Bytes via shell_output",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("message model-facing description misses %q:\n%s", want, desc)
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
