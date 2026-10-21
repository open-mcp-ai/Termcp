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
		sessMgr.AddOnDeadHook(forwardMgr.CloseBySession)
	}
	s := &Server{
		sessMgr:     sessMgr,
		msgMgr:      msgMgr,
		sshConfigs:  sshConfigs,
		forwardMgr:  forwardMgr,
		connections: newConnRegistry(),
	}
	for _, opt := range opts {
		opt(s)
	}

	mcpServer := mcpserver.NewMCPServer("termcp", version,
		mcpserver.WithInstructions(mcpServerInstructions),
		// Validate tool arguments against the published schema. The args table
		// carries types, enums and bounds, so a client that sends `timeout: "abc"`
		// or an unknown overflow policy is refused at the protocol layer, before any
		// handler runs.
		//
		// This does NOT replace parseTermArgs' own checks, and the reason is the two
		// entrances: mcp-go validates on the MCP transport, while POST /api/term calls
		// Term directly and would bypass it. The parser is the one path both share,
		// so it keeps the same rules; this layer exists so a client sees the refusal
		// for the call it made, with its own schema in hand.
		mcpserver.WithInputSchemaValidation(),
		// Prompts are user-triggered playbooks (the `learn-api` one). Resources are
		// NOT claimed: the tool surface is one command line that describes itself,
		// so a client has nothing to list or read here (see docs.go).
		mcpserver.WithPromptCapabilities(false),
		// The address is NOT in the instructions (see mcpServerInstructions): it
		// rides on the listing, which must reach the model or no tool can be called.
		// Every published address follows the request that asked for it, so one
		// instance behind several addresses tells each client its own.
		mcpserver.WithHooks(&mcpserver.Hooks{
			// Who is attached. mcp-go registers a session before the client has
			// said anything, so the register hook can only record the session
			// object; the client's name arrives with initialize, and the caller's
			// namespace claim arrives in that same request's context.
			OnRegisterSession: []mcpserver.OnRegisterSessionHookFunc{
				func(_ context.Context, session mcpserver.ClientSession) {
					s.connections.add(session)
					s.notifyConnectionsChanged()
				},
			},
			OnUnregisterSession: []mcpserver.OnUnregisterSessionHookFunc{
				func(_ context.Context, session mcpserver.ClientSession) {
					s.connections.remove(session)
					s.notifyConnectionsChanged()
				},
			},
			OnAfterInitialize: []mcpserver.OnAfterInitializeFunc{
				func(ctx context.Context, _ any, _ *mcpgo.InitializeRequest, _ *mcpgo.InitializeResult) {
					// The session is on the context for the request being answered,
					// which is how a per-request fact (the namespace claim) is tied
					// to a session-level row.
					if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
						s.connections.noteNamespace(session.SessionID(), callerNamespace(ctx))
					}
				},
			},
			OnAfterListTools: []mcpserver.OnAfterListToolsFunc{
				func(ctx context.Context, _ any, _ *mcpgo.ListToolsRequest, result *mcpgo.ListToolsResult) {
					s.decorateTools(ctx, result)
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
	registerTermTools(mcpServer, s)

	s.mcpServer = mcpServer
	// Both transports lift the request's own origin into the context, so a tools/list
	// answer can name the address that client actually reached.
	// Both transports lift the request's own origin into the context, so a tools/list
	// answer can name the address that client actually reached — and the caller's
	// namespace claim, which is what scopes its sessions. mcp-go's option is a
	// single-slot setter, so the two are composed into one function rather than
	// passed one after the other: the second would replace the first.
	s.sseServer = mcpserver.NewSSEServer(mcpServer, append(s.sseOpts, mcpserver.WithSSEContextFunc(withRequestContext))...)
	// Streamable HTTP (MCP spec): mount at /stream for clients such as Open WebUI.
	// Do not use WithStreamableHTTPServer(mainSrv) here — Shutdown must not close the shared listener.
	s.streamServer = mcpserver.NewStreamableHTTPServer(mcpServer, mcpserver.WithHTTPContextFunc(withRequestContext))
	return s
}
