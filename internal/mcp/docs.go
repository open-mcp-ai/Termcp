package mcp

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Agent-facing documentation surface: MCP resources + prompts.
//
// termcp ships its own docs inside the binary and serves them over HTTP
// (internal/webui/assets: /api.md, /skills.md).
// The same bytes are published as MCP resources so a client that supports
// resources can list and read them without an HTTP call, and a client that
// does not can curl the identical URL. Resources are registered from Start()
// once baseURL is known, so URIs are real, instance-specific HTTP addresses.

// docResource describes one documentation asset exposed to agents.
type docResource struct {
	path        string // asset path inside the docs FS
	name        string
	description string
}

// docResources is the full agent documentation set. Every entry is also served
// over HTTP at "/" + path on the same origin as the MCP endpoints.
var docResources = []docResource{
	{
		path:        "api.md",
		name:        "termcp HTTP API reference",
		description: "Authoritative REST + WebSocket reference for this termcp instance: endpoints, auth, request/response bodies, output-cursor semantics. Read before scripting against the HTTP API.",
	},
	{
		path:        "skills.md",
		name:        "termcp HTTP skill (curl recipes)",
		description: "Agent skill for driving termcp over HTTP with curl + jq only: session lifecycle, output polling (live and closed sessions), input/keys/resize, files, forwards, pitfalls. Installable into a skills directory.",
	},
}

// SetDocsFS attaches the embedded documentation FS (webui.Assets()). Call
// before Start; without it no resources/prompts are registered.
func (s *Server) SetDocsFS(fsys fs.FS) {
	s.docsFS = fsys
}

// registerDocs publishes documentation resources and prompts. It runs from
// Start() after baseURL is resolved, and is a no-op without a docs FS.
func (s *Server) registerDocs() {
	if s.docsFS == nil {
		return
	}
	for _, doc := range docResources {
		doc := doc
		uri := s.docURL(doc.path)
		s.mcpServer.AddResource(
			mcpgo.NewResource(uri, doc.name,
				mcpgo.WithResourceDescription(doc.description),
				mcpgo.WithMIMEType("text/markdown"),
			),
			func(ctx context.Context, _ mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
				// mcp-go routes by exact URI, so this closure only ever serves doc.
				text, err := s.readDoc(doc.path)
				if err != nil {
					return nil, err
				}
				return []mcpgo.ResourceContents{mcpgo.TextResourceContents{
					// The address of the caller's own request, matching what
					// resources/list advertised and what tools/list publishes.
					URI:      s.docURLFor(ctx, doc.path),
					MIMEType: "text/markdown",
					Text:     text,
				}}, nil
			})
	}
	s.registerLearnAPIPrompt()
}

// docURL returns the canonical (startup) resource URI for a doc. It is the URI
// the resource is registered under, and the fallback when a request carries no
// origin of its own.
func (s *Server) docURL(p string) string {
	base := s.baseURL
	if base == "" {
		base = "http://127.0.0.1"
	}
	return strings.TrimSuffix(base, "/") + "/" + p
}

// docURLFor returns the URI for a doc as **this** caller should see it: the
// address the request arrived on (see originFor), else the canonical one. One
// instance behind several addresses publishes one doc under several URIs, and
// each client is told the address it actually reached.
func (s *Server) docURLFor(ctx context.Context, p string) string {
	if origin := s.originFor(ctx); origin != "" {
		return origin + "/" + p
	}
	return s.docURL(p)
}

// canonicalDocURI maps any-address doc URI back to the URI the resource is
// registered under. mcp-go routes resources by exact string, so a client that
// lists `http://lan.example:9000/api.md` and then reads it would miss the
// registration made at the startup baseURL. This is the one place the mapping
// happens; the read hook below applies it.
func (s *Server) canonicalDocURI(raw string) string {
	if p := docPathOf(raw); p != "" {
		return s.docURL(p)
	}
	return raw
}

// docPathOf returns the asset path a doc resource URI names ("api.md"), or ""
// when the URI is not one of this instance's docs. Only the path matters: the
// origin varies per request, the document does not.
func docPathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	for _, doc := range docResources {
		if u.Path == "/"+doc.path {
			return doc.path
		}
	}
	return ""
}

// decorateResources rewrites the listed doc URIs to the address of the request
// being answered, so resources/list and tools/list agree on where this instance
// is (see decorateTools).
func (s *Server) decorateResources(ctx context.Context, result *mcpgo.ListResourcesResult) {
	for i := range result.Resources {
		if p := docPathOf(result.Resources[i].URI); p != "" {
			result.Resources[i].URI = s.docURLFor(ctx, p)
		}
	}
}

// readDoc reads one documentation asset.
func (s *Server) readDoc(p string) (string, error) {
	b, err := fs.ReadFile(s.docsFS, p)
	if err != nil {
		return "", fmt.Errorf("read embedded doc %s: %w", p, err)
	}
	return string(b), nil
}

// registerLearnAPIPrompt publishes the "learn-api" prompt: a user-triggered
// playbook that points the agent at this instance's own API reference.
func (s *Server) registerLearnAPIPrompt() {
	s.mcpServer.AddPrompt(
		mcpgo.NewPrompt("learn-api",
			mcpgo.WithPromptDescription("Load this instance's HTTP API reference (and the curl skill) and report what termcp can do. Use before scripting against termcp over REST."),
			mcpgo.WithArgument("task", mcpgo.ArgumentDescription("Optional concrete task to plan once the API is loaded")),
		),
		func(ctx context.Context, req mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
			// Resolved per request: a client that reached this instance by a LAN
			// name, a proxy or a tunnel is handed the address it can actually open.
			apiURL := s.docURLFor(ctx, "api.md")
			skillURL := s.docURLFor(ctx, "skills.md")
			task := strings.TrimSpace(req.Params.Arguments["task"])

			var b strings.Builder
			fmt.Fprintf(&b, "Learn this termcp instance's HTTP API before acting.\n\n")
			fmt.Fprintf(&b, "1. Read the MCP resource %s (or fetch %s if you cannot read resources).\n", apiURL, apiURL)
			fmt.Fprintf(&b, "2. If you will drive termcp with curl, also read %s (or fetch %s).\n", skillURL, skillURL)
			b.WriteString("The documents are served by the running instance, so they always match its actual API.\n\n")
			if task != "" {
				fmt.Fprintf(&b, "Then plan and execute this task using that API: %s\n", task)
			} else {
				b.WriteString("Then summarize: session-lifecycle endpoints, terminal I/O (input/output polling) endpoints, auth, and the exact curl calls for starting an interactive session and reading its output.\n")
			}
			b.WriteString("Prefer MCP tools when the client has them; use REST when it does not.")

			return &mcpgo.GetPromptResult{
				Description: "termcp API learning prompt",
				Messages: []mcpgo.PromptMessage{{
					Role:    mcpgo.RoleUser,
					Content: mcpgo.NewTextContent(b.String()),
				}},
			}, nil
		})
}
