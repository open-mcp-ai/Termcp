package mcp

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/webui"
)

// callMCP dispatches one JSON-RPC request through the MCP server's message
// handler and returns the raw "result" payload.
func callMCP(t *testing.T, s *Server, method string, params map[string]any) json.RawMessage {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg := s.mcpServer.HandleMessage(context.Background(), raw)
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
		t.Fatalf("%s failed: %s", method, envelope.Error.Message)
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
	if len(read.Contents) != 1 || !strings.Contains(read.Contents[0].Text, "termcp HTTP API") {
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
