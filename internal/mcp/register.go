package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// New creates and configures the MCP server with all tools registered.
// sshConfigs may be nil (session_start / ssh_config(action=list) will error or return empty).
// version is the build version reported in initialize's serverInfo (main passes the
// same value `termcp --version` prints, which is never empty in practice). An empty
// string falls back to "dev" so an in-process caller cannot advertise nothing.
// opts tune the server: DeferTools() lazily loads low-frequency tools, and
// WithHTTPServer() attaches the shared http.Server to the SSE transport.
func New(sessMgr *session.Manager, msgMgr *message.Manager, sshConfigs *sshconfig.Store, forwardMgr *forward.ForwardManager, version string, opts ...Option) *Server {
	if version == "" {
		version = "dev"
	}
	// Port forwards are bound to a session's SSH transport: when a session goes
	// DEAD (terminate/exit/lost connection) every forward of that session must be
	// released, or its local listener lingers as a dead endpoint. The session
	// manager does not know the forward manager, so wire the cascade here at the
	// composition point shared by the MCP and Web UI surfaces.
	if sessMgr != nil && forwardMgr != nil {
		sessMgr.SetOnDeadHook(forwardMgr.CloseBySession)
	}
	s := &Server{
		sessMgr:    sessMgr,
		msgMgr:     msgMgr,
		sshConfigs: sshConfigs,
		forwardMgr: forwardMgr,
	}
	for _, opt := range opts {
		opt(s)
	}

	mcpServer := mcpserver.NewMCPServer("termcp", version,
		mcpserver.WithInstructions(mcpServerInstructions),
		// Resources are read-only docs (resources/list + resources/read) and the
		// list is fixed at startup, so no subscribe/listChanged: mcp-go v0.50 has
		// no subscribe handler, and claiming it would break clients that try.
		mcpserver.WithResourceCapabilities(false, false),
		mcpserver.WithPromptCapabilities(false),
		// The address is NOT in the instructions (see mcpServerInstructions): it
		// rides on the listing, which must reach the model or no tool can be called.
		// Every published address follows the request that asked for it, so one
		// instance behind several addresses tells each client its own.
		mcpserver.WithHooks(&mcpserver.Hooks{
			OnAfterListTools: []mcpserver.OnAfterListToolsFunc{
				func(ctx context.Context, _ any, _ *mcpgo.ListToolsRequest, result *mcpgo.ListToolsResult) {
					s.decorateTools(ctx, result)
				},
			},
			OnAfterListResources: []mcpserver.OnAfterListResourcesFunc{
				func(ctx context.Context, _ any, _ *mcpgo.ListResourcesRequest, result *mcpgo.ListResourcesResult) {
					s.decorateResources(ctx, result)
				},
			},
			// mcp-go routes resources by exact URI, so a read of the URI this
			// instance just advertised must be mapped back to the registration
			// before routing (see canonicalDocURI).
			OnBeforeReadResource: []mcpserver.OnBeforeReadResourceFunc{
				func(_ context.Context, _ any, request *mcpgo.ReadResourceRequest) {
					request.Params.URI = s.canonicalDocURI(request.Params.URI)
				},
			},
			OnAfterReadResource: []mcpserver.OnAfterReadResourceFunc{
				func(ctx context.Context, _ any, request *mcpgo.ReadResourceRequest, result *mcpgo.ReadResourceResult) {
					p := docPathOf(request.Params.URI)
					if p == "" {
						return
					}
					// ResourceContents is an interface; only the text variant is
					// served here (see registerDocs).
					for i, c := range result.Contents {
						if tc, ok := c.(mcpgo.TextResourceContents); ok {
							tc.URI = s.docURLFor(ctx, p)
							result.Contents[i] = tc
						}
					}
				},
			},
		}),
		// Outermost of the tool middlewares, so it also covers a panic in the
		// logging wrapper itself. See withPanicRecovery for why this is not
		// mcpserver.WithRecovery().
		withPanicRecovery(),
	)

	s.notifyMgr = notify.NewManager(s)
	if sessMgr != nil {
		sessMgr.SetNotifyHooks(s.notifyMgr.OnOutput, s.notifyMgr.OnExit, s.notifyMgr.ClearShell)
	}
	registerTools(mcpServer, s)

	s.mcpServer = mcpServer
	// Both transports lift the request's own origin into the context, so a tools/list
	// answer can name the address that client actually reached.
	s.sseServer = mcpserver.NewSSEServer(mcpServer, append(s.sseOpts, mcpserver.WithSSEContextFunc(withRequestOrigin))...)
	// Streamable HTTP (MCP spec): mount at /stream for clients such as Open WebUI.
	// Do not use WithStreamableHTTPServer(mainSrv) here — Shutdown must not close the shared listener.
	s.streamServer = mcpserver.NewStreamableHTTPServer(mcpServer, mcpserver.WithHTTPContextFunc(withRequestOrigin))
	return s
}
