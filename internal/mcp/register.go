package mcp

import (
	"context"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftpengine"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/task"
	mcpgo "github.com/open-mcp-ai/termcp/mcp"
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
	// A peer (mcp/cdp/jrpc) tunnels through the session's SSH channel, so a DEAD
	// session leaves it a socket to nowhere that still holds its frame ring. Same
	// cascade as forwards, for the same reason.
	if sessMgr != nil {
		sessMgr.AddOnDeadHook(closePeersBySession)
	}
	s := &Server{
		sessMgr:     sessMgr,
		msgMgr:      msgMgr,
		sshConfigs:  sshConfigs,
		forwardMgr:  forwardMgr,
		connections: newConnRegistry(),
		tasks:       task.NewManager(),
		ptyQuiet:    newPtyQuietTracker(sessMgr),
		sftpEngines: sftpengine.NewPool(sessMgr),
	}
	for _, opt := range opts {
		opt(s)
	}

	mcpServer := mcpgo.NewServer("termcp", version, mcpServerInstructions, mcpgo.Hooks{
		// Who is attached. The transport registers a session before the client has
		// said anything, so the register hook can only record the session object;
		// the client's name arrives with initialize.
		OnRegisterSession: []func(ctx context.Context, session mcpgo.Session){
			func(_ context.Context, session mcpgo.Session) {
				s.connections.add(session)
				s.notifyConnectionsChanged()
			},
		},
		OnUnregisterSession: []func(ctx context.Context, session mcpgo.Session){
			func(_ context.Context, session mcpgo.Session) {
				s.connections.remove(session)
				s.notifyConnectionsChanged()
			},
		},
		OnAfterInitialize: []func(ctx context.Context, req *mcpgo.InitializeRequest, res *mcpgo.InitializeResult){
			func(ctx context.Context, _ *mcpgo.InitializeRequest, _ *mcpgo.InitializeResult) {
				// The caller's namespace claim arrives on the initialize request's
				// context, which is how a per-request fact is tied to the session row.
				if session := mcpgo.SessionFromContext(ctx); session != nil {
					s.connections.noteNamespace(session.SessionID(), callerNamespace(ctx))
				}
			},
		},
		OnAfterListTools: []func(ctx context.Context, req *mcpgo.ListToolsRequest, res *mcpgo.ListToolsResult){
			func(ctx context.Context, _ *mcpgo.ListToolsRequest, result *mcpgo.ListToolsResult) {
				s.decorateTools(ctx, result)
			},
		},
	})
	// Outermost of the tool middlewares, so it also covers a panic in the logging
	// wrapper itself.
	mcpServer.Use(withPanicRecovery())

	registerTermTools(mcpServer, s)

	s.mcpServer = mcpServer
	// Both transports lift the request's own origin into the context, so a tools/list
	// answer can name the address that client actually reached — and the caller's
	// namespace claim, which is what scopes its sessions.
	s.sseServer = mcpgo.NewSSEServer(mcpServer, mcpgo.WithSSEContextFunc(withRequestContext))
	// Streamable HTTP (MCP spec): mount at /stream for clients such as Open WebUI.
	s.streamServer = mcpgo.NewStreamableHTTPServer(mcpServer, mcpgo.WithHTTPContextFunc(withRequestContext))
	return s
}
