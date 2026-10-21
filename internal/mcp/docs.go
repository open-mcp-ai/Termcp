package mcp

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Agent-facing documentation surface: one MCP prompt, no MCP resources.
//
// WHY THERE ARE NO RESOURCES. termcp used to publish api.md as an MCP resource
// (`resources/list` + `resources/read`). That is now redundant: the tool surface is
// ONE tool whose argument is a command line, and the command space describes itself
// — an empty cmd returns the guide, `help` returns the whole directive table, and
// `help <directive>` returns one directive's shape and traps, all generated from
// the same table the tool uses. An MCP client therefore has no reason to read the
// REST endpoint list: it does not call POST /api/sessions, it sends `pty internal`.
//
// The document itself is still needed and still served — at `<origin>/api.md`, and
// it is the only hand-written reference for the TRANSPORT layer (endpoints, bodies,
// cursor semantics, auth), which `help` cannot cover because `help` describes the
// command language, not HTTP routes. A caller scripting against REST fetches that
// URL directly. Publishing the same bytes through a second access path bought
// nothing and cost a capability claim plus four hooks to keep the two in sync.
//
// The `learn-api` prompt stays: it is the one thing that cannot be derived from
// asking the instance, because an agent that does not yet know `cmd` is the whole
// surface will not know to send it.

// SetDocsFS attaches the embedded documentation FS (webui.Assets()). Call
// before Start; without it no documentation prompt is registered.
func (s *Server) SetDocsFS(fsys fs.FS) {
	s.docsFS = fsys
}

// registerDocs publishes the documentation prompt. It runs from Start() after
// baseURL is resolved, and is a no-op without a docs FS.
func (s *Server) registerDocs() {
	if s.docsFS == nil {
		return
	}
	s.registerLearnAPIPrompt()
}

// docURL returns the address of a doc on the startup origin. It is the fallback
// when a request carries no origin of its own.
func (s *Server) docURL(p string) string {
	base := s.baseURL
	if base == "" {
		base = "http://127.0.0.1"
	}
	return strings.TrimSuffix(base, "/") + "/" + p
}

// docURLFor returns the URI for a doc as **this** caller should see it: the
// address the request arrived on (see originFor), else the canonical one. One
// instance behind several addresses tells each client the address it reached.
func (s *Server) docURLFor(ctx context.Context, p string) string {
	if origin := s.originFor(ctx); origin != "" {
		return origin + "/" + p
	}
	return s.docURL(p)
}

// registerLearnAPIPrompt publishes the "learn-api" prompt: a user-triggered
// playbook that points the agent at this instance's own API reference.
func (s *Server) registerLearnAPIPrompt() {
	s.mcpServer.AddPrompt(
		mcpgo.NewPrompt("learn-api",
			mcpgo.WithPromptDescription("Load this instance's HTTP API reference and report what termcp can do. Use before scripting against termcp over REST."),
			mcpgo.WithArgument("task", mcpgo.ArgumentDescription("Optional concrete task to plan once the API is loaded")),
		),
		func(ctx context.Context, req mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
			// Resolved per request: a client that reached this instance by a LAN
			// name, a proxy or a tunnel is handed the address it can actually open.
			apiURL := s.docURLFor(ctx, "api.md")
			task := strings.TrimSpace(req.Params.Arguments["task"])

			var b strings.Builder
			fmt.Fprintf(&b, "Learn this termcp instance's HTTP API before acting.\n\n")
			fmt.Fprintf(&b, "1. Fetch %s if you will drive this instance over REST or WebSocket.\n", apiURL)
			b.WriteString("2. The API is ONE endpoint, POST /api/term, carrying ONE command language. Ask the\n")
			b.WriteString("   instance itself for the command space instead of reading a tutorial:\n")
			b.WriteString("     {\"cmd\":\"\"}         -> guide: what is here, what can I do\n")
			b.WriteString("     {\"cmd\":\"help\"}     -> every directive\n")
			b.WriteString("     {\"cmd\":\"help run\"} -> one directive's shape and traps\n")
			b.WriteString("   Those replies come from the same table the MCP term tool uses, so they cannot\n")
			b.WriteString("   go stale the way a written skill would.\n")
			b.WriteString("The document is served by the running instance, so it always matches its actual API.\n\n")
			if task != "" {
				fmt.Fprintf(&b, "Then plan and execute this task using that API: %s\n", task)
			} else {
				b.WriteString("Then summarize how to start an interactive session and read its output with\n")
				b.WriteString("POST /api/term, and report the directives the empty-cmd guide listed.\n")
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
