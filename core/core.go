// Package core assembles a termcp instance: the sessions, the shells, the MCP
// surface, the REST entrance, and optionally the browser UI.
//
// # Why this exists
//
// main() used to be the only way to get an instance. That is fine for a binary
// and wrong for a library: a Go program that wants to embed termcp had to fork
// the repo, reimplement the wiring, or shell out to the binary. Everything the
// binary did was reachable, but only through a command line.
//
// This package is that wiring, lifted out unchanged. It takes a Config, builds
// the same objects in the same order, and gives back a *Server that can be
// started and stopped. main() is now flag parsing plus one call into here.
//
// # Two ways to run it
//
// Over HTTP, which is what the binary does:
//
//	srv, err := core.New(core.Options{Config: cfg})
//	if err != nil { return err }
//	defer srv.Close()
//	return srv.Start()          // binds cfg.Host:cfg.Port
//
// Or in-process, with no listener at all:
//
//	srv, err := core.New(core.Options{Config: cfg, NoListener: true})
//	...
//	reply := srv.Term(ctx, "ls session", nil)   // the TERM/1 envelope
//
// The second form is the point of the exercise: an embedder that already has a
// transport (its own HTTP mux, a queue consumer, a test) does not need termcp to
// open a port, and should not have to reach through a socket to run a command.
//
// # What it does not assume
//
// Nothing here reads ~/.termcp unless Config says so. Config.DataDir is
// honoured as given; the CLI is what fills in a default. An embedder that
// leaves it empty gets storage rooted at "", which storage.New treats as the
// current directory — legal, and the embedder's choice to make, so it is
// documented rather than guessed at.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/config"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/daemon"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/jrpc"
	"github.com/open-mcp-ai/termcp/internal/logansi"
	mcpi "github.com/open-mcp-ai/termcp/internal/mcp"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/internal/webui"
	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// Options configures an instance. Config is required; everything else is a
// choice about how this instance runs rather than what it is.
type Options struct {
	// Config is the instance's settings. Required.
	Config *config.Config

	// Version is what `-version`, the MCP handshake and the Web UI report. The
	// binary passes its ldflags value; an embedder passes its own.
	Version string

	// NoListener runs the instance without binding a port. The MCP and REST
	// surfaces still exist and are reachable through Term and the handlers on
	// Server, but nothing is served. Start then only starts the background
	// machinery and returns.
	//
	// This is the SDK form. It exists because an embedder usually already has a
	// transport, and an instance that insists on a port cannot be used inside
	// one without inventing a free port, waiting for a bind, and hoping.
	NoListener bool

	// Headless omits the browser UI entirely: no static assets, no /api/sessions,
	// no WebSocket. The MCP tool surface and POST /api/term remain, which is
	// everything an agent needs. Ignored (and reported as such by Start) in a
	// `no_webui` build, where the UI is not compiled in at all.
	Headless bool

	// DaemonChild marks an instance launched by `termcp daemon`, which changes
	// two things: GET /api/daemon reports the idle countdown, and the idle
	// watcher may end the process. A library embedder leaves this false.
	DaemonChild bool

	// IdleTimeout overrides the daemon idle countdown when DaemonChild is set.
	// Zero means daemon.DefaultIdleTimeout; the CLI passes --idle-timeout here.
	IdleTimeout time.Duration

	// IdleTimeoutSet records that IdleTimeout came from an explicit flag, so a
	// deliberate `0` (disabled) is distinguishable from "not given".
	IdleTimeoutSet bool
}

// Server is a running instance. It owns everything it built; Close releases it
// in the order that lets in-flight work finish first.
type Server struct {
	opts Options
	cfg  *config.Config

	httpSrv *http.Server
	mux     *http.ServeMux
	addr    string

	store      *storage.Store
	msgMgr     *message.Manager
	sessMgr    *session.Manager
	sshStore   *sshconfig.Store
	sshSrv     *sshserver.Server
	forwardMgr *forward.ForwardManager
	extensions *extensions.Reconciler
	mcp        *mcpi.Server
	webui      *webui.Handler
	verifier   *auth.Verifier

	uiEnabled bool

	started  atomic.Bool
	shutdown atomic.Bool
	once     sync.Once
	// ready is closed once the background machinery is up, so an embedder can
	// wait for "usable" without polling or sleeping. Start closes it before it
	// blocks, which is the first moment Term can answer.
	ready chan struct{}
	// stopCh lets a caller that is not the binary (no OS signal, no daemon
	// countdown) ask Start to return. The binary's own triggers — SIGINT and the
	// idle watcher — run through Close.
	stopCh   chan struct{}
	serveErr chan error
}

// New builds an instance and its dependencies, but does not serve. Nothing here
// opens a listening socket; Start does that (and only when a listener is
// wanted), so a construction error is always a configuration error and never a
// half-started server.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("core: Options.Config is required")
	}
	cfg := opts.Config
	// A port is only a constraint when something is going to bind it. With
	// NoListener there is no listener to validate, and demanding one would force
	// every embedder to invent a free port it never uses — the exact thing
	// NoListener exists to avoid.
	if !opts.NoListener {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	} else if cfg.DataDir == "" {
		return nil, errors.New("core: Config.DataDir is required")
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	s := &Server{
		opts: opts, cfg: cfg,
		stopCh:   make(chan struct{}),
		serveErr: make(chan error, 1),
		ready:    make(chan struct{}),
	}

	if cfg.AuthToken != "" || cfg.AuthHash != "" {
		v, err := auth.NewVerifier(cfg.AuthToken, cfg.AuthHash)
		if err != nil {
			return nil, fmt.Errorf("core: invalid auth config: %w", err)
		}
		s.verifier = v
	}

	// The in-process SSH server is the transport every local session rides: it
	// is loopback-only and has no listening port of its own.
	if !cfg.NoInternal {
		s.sshSrv = sshserver.New()
	}

	s.store = storage.New(cfg.DataDir)
	s.msgMgr = message.NewManager(s.store)
	s.sessMgr = session.NewManager(s.msgMgr, s.store, s.sshSrv)
	if err := s.sessMgr.RestoreDead(); err != nil {
		slog.Warn("failed to restore previous DEAD sessions", "err", err)
	}
	s.sshStore = sshconfig.NewStore(cfg.DataDir)
	s.forwardMgr = forward.NewForwardManager()

	s.addr = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	s.mux = http.NewServeMux()
	s.httpSrv = &http.Server{Addr: s.addr, Handler: s.mux}

	// The extension reconciler keeps every enabled extension's session warm and
	// reports its state. Its loop's context is cancelled by Stop, and Close stops
	// it before the sessions are marked dead, so an in-flight probe cannot spawn
	// a connection the process is about to leave.
	s.extensions = extensions.NewReconciler(
		extensions.NewStore(cfg.DataDir), s.sessMgr, s.msgMgr, s.sshStore, s.forwardMgr,
	)
	s.extensions.NoInternal = cfg.NoInternal

	s.mcp = mcpi.New(s.sessMgr, s.msgMgr, s.sshStore, s.forwardMgr, opts.Version)
	// The embedded docs feed the `learn-api` prompt. In a no_webui build they are
	// the only asset left, which is deliberate: an instance that serves an HTTP
	// API should be able to hand an agent its reference for that API.
	s.mcp.SetDocsFS(webui.Assets())
	s.mcp.NoInternal = cfg.NoInternal
	s.mcp.ManageSSHConfigs = cfg.MCPManageSSHConfigs

	s.mux.Handle("GET /sse", s.mcp.SSEHandler())
	s.mux.Handle("POST /message", s.mcp.MessageHandler())
	s.mux.Handle("/stream", s.mcp.StreamableHTTPHandler())
	// One MCP endpoint per extension (issue #234): /stream above serves termcp's
	// own tool surface, so an MCP client that does not know the directive language
	// cannot reach an extension's tools. This exposes that extension's own server
	// verbatim while termcp keeps the lifecycle and the policy.
	s.mux.Handle("/ext/{name}/stream", s.mcp.ExtMCPHandler())
	// The REST entrance to the same command space: POST /api/term takes the term
	// tool's argument object and answers the same envelope. Mounted here rather
	// than by the Web UI because admission and the namespace claim live in
	// internal/mcp — a second copy on the other side of the import cycle would be
	// a second place to get admission wrong. A pattern the Web UI owns wins over
	// this one (it registers later, and ServeMux refuses overlaps rather than
	// overriding), so no existing route can be shadowed.
	s.mux.Handle("POST /api/term", s.mcp.TermHandler())

	s.wireUI()

	if s.verifier != nil {
		s.httpSrv.Handler = auth.Middleware(s.verifier, s.mux)
	}
	return s, nil
}

// wireUI mounts the browser UI unless this instance asked for a headless one.
// Split out so the headless decision is one readable branch instead of a dozen
// conditionals scattered through New.
func (s *Server) wireUI() {
	if s.opts.Headless {
		// The browser UI is gone, the agent-facing document is not: it is the only
		// hand-written reference for the transport layer (endpoints, bodies, cursor
		// semantics, auth), and `help` cannot cover it because `help` describes the
		// command language rather than HTTP routes. A `no_webui` build keeps exactly
		// this one file for the same reason, so the two forms agree.
		s.mux.Handle("/api.md", http.FileServer(http.FS(webui.Assets())))
		slog.Info("browser UI: disabled (headless)")
		return
	}
	if !webui.Embedded() {
		// Compiled without the UI: webui.Register mounts /api.md itself (see
		// handler.go), so there is nothing to add here.
		slog.Info("browser UI: not in this build (no_webui)")
		return
	}
	cfg := s.cfg
	h := &webui.Handler{
		Sessions: s.sessMgr, MsgMgr: s.msgMgr, SSH: s.sshStore, ForwardMgr: s.forwardMgr,
		Extensions: s.extensions, NoInternal: cfg.NoInternal, Version: s.opts.Version,
		Daemon: s.opts.DaemonChild, StartedAt: time.Now().UTC().Format(time.RFC3339),
		SFTP: s.mcp.SFTPPool(),
	}
	// The effective countdown is published at GET /api/daemon so the stdio bridge
	// can ping faster than it (see daemon.InstanceInfo.IdleTimeoutMS).
	if s.opts.DaemonChild {
		h.DaemonLog = daemon.LogPath(cfg.DataDir)
		h.IdleTimeout = daemon.DefaultIdleTimeout
		if s.opts.IdleTimeoutSet {
			h.IdleTimeout = s.opts.IdleTimeout
		}
	}
	// File transfers and port forwards are held by the same review queue as
	// command lines, but the operations live with the MCP server (it owns the SFTP
	// and forward machinery). The UI decides; this replays the decision where the
	// operation is implemented.
	h.ExecuteOperation = s.mcp.ExecuteApprovedOperation
	h.Register(s.mux)

	// The notify_user tool reaches the browser as a toast/highlight push.
	s.mcp.SetExtensionReconciler(s.extensions)
	s.mcp.SetUINotifier(h.BroadcastUINotify)
	// Who is attached: the UI lists MCP clients from the MCP server and its own
	// tabs from its own registry. The listener wakes the same broadcast the
	// session list uses, so a client connecting shows up without a poll.
	h.MCPConnections = func() []any {
		conns := s.mcp.Connections()
		out := make([]any, len(conns))
		for i, c := range conns {
			out[i] = c
		}
		return out
	}
	s.mcp.SetConnectionsListener(h.NotifyClientsChanged)
	// The daemon stop endpoint asks for the same exit path a signal takes.
	h.StopDaemon = func() { s.Close("daemon stop") }

	s.webui = h
	s.uiEnabled = true
}

// Embedded reports whether the browser UI was mounted.
func (s *Server) Embedded() bool { return s.uiEnabled }

// WaitReady blocks until the instance can answer Term, or ctx ends. An embedder
// running Start in a goroutine calls this instead of sleeping: "the server is
// up" is a state, not a duration.
//
// It returns nil immediately if Start has already reached that point.
func (s *Server) WaitReady(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Addr is the address the instance serves on ("host:port"), whether or not it
// was asked to listen.
func (s *Server) Addr() string { return s.addr }

// Handler exposes the instance's HTTP surface, so an embedder can mount it on
// its own listener instead of letting termcp open one. Returns the same mux the
// instance serves when it does listen, including the auth middleware.
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

// MCP exposes the MCP protocol endpoint, so an embedder can register its own
// tools and prompts alongside termcp's.
//
// This is the integration point that is an object rather than a hook: *mcp.Server
// already has AddTool, AddPrompt, OnMethod and Use, so there is no plugin
// interface to design and nothing new to promise. Register before Start for the
// common case; registering later is visible to clients on their next tools/list.
func (s *Server) MCP() *mcp.Server { return s.mcp.MCP() }

// Term runs one command line through the instance and returns the TERM/1
// envelope, exactly as POST /api/term and the `term` tool do.
//
// This is the SDK's front door: no socket, no JSON round trip, no port. A caller
// that already has a request context should pass it, because the context carries
// cancellation and the caller's namespace.
func (s *Server) Term(ctx context.Context, cmd string, args map[string]any) envelope.Reply {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.mcp.Term(ctx, cmd, args)
}

// Start brings the instance up and, unless NoListener was set, serves until the
// server stops. It returns nil for a clean shutdown and an error otherwise.
//
// Start blocks: an embedder that wants a background instance should run it in a
// goroutine, the same way the binary does.
func (s *Server) Start() error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("core: instance already started")
	}

	slog.Info("termcp server started", "version", s.opts.Version)

	if s.sshSrv != nil {
		if err := s.sshSrv.Start(); err != nil {
			return fmt.Errorf("core: starting internal SSH server: %w", err)
		}
		// The in-process MCP bridge is a command the internal SSH server serves
		// itself, so an extension reaches an HTTP MCP server over its own channel
		// with no second binary and no PATH lookup. Registered after Start because
		// `--via <profile>` needs the profile store and the session manager to
		// answer "which machine does this connection leave from" (#233); it is a
		// sync.Map insert, so late registration is safe.
		mcpbridge.RegisterBuiltin(s.sshSrv, viaDialer(s.sessMgr, s.sshStore, s.cfg.NoInternal))
	}

	s.logSurfaces()

	if err := s.extensions.Start(context.Background()); err != nil {
		return fmt.Errorf("core: starting extension reconciler: %w", err)
	}
	// From here on Term can answer, so this is what "usable" means.
	close(s.ready)

	if s.opts.DaemonChild {
		timeout := daemon.DefaultIdleTimeout
		if s.opts.IdleTimeoutSet {
			timeout = s.opts.IdleTimeout
		}
		// The watcher starts counting at creation — a fresh instance has seen no
		// activity yet — and management probes are exempt, so a status query never
		// keeps alive the instance it is asking about. The predicate lives with the
		// probe itself: the probe falls back to the public /api.md fingerprint when
		// /api/daemon is rejected, and that fallback must be exempt too.
		watcher := daemon.NewIdleWatcher(timeout, func() { s.Close("idle timeout") })
		watcher.Exempt = daemon.IsProbe
		s.httpSrv.Handler = watcher.Track(s.httpSrv.Handler)
		slog.Info("daemon instance active", "pid", os.Getpid(), "idle_timeout", timeout)
	}

	if s.opts.NoListener {
		slog.Info("listening: no (NoListener); drive this instance through Term or Handler")
		<-s.stopCh
		s.finish()
		return nil
	}

	// The MCP surface is mounted on the shared mux, so this server is the only
	// listener: Start resolves the address the docs prompt advertises, then one
	// ListenAndServe carries every route (MCP, REST, Web UI) on that mux.
	//
	// It used to be the SSE transport that blocked here, with WithHTTPServer
	// handing it s.httpSrv — so the whole instance was served by a dependency's
	// Serve call. Now that the transports are handlers, serving is this server's
	// own job, which is also what makes `Handler()` (embedding, tests) and
	// ListenAndServe two ways of running the same thing.
	if err := s.mcp.Start(s.addr); err != nil {
		s.finish()
		return err
	}
	go func() { s.serveErr <- s.httpSrv.ListenAndServe() }()

	select {
	case err := <-s.serveErr:
		if err != nil && !(s.shutdown.Load() && errors.Is(err, http.ErrServerClosed)) {
			s.finish()
			return err
		}
		s.finish()
		slog.Info("server stopped")
		return nil
	case <-s.stopCh:
		s.finish()
		slog.Info("server stopped")
		return nil
	}
}

// Close is the one exit path: a signal, the daemon countdown, an embedder's
// defer, and a stop request all land here, and the first one wins. reason is
// only for the log.
func (s *Server) Close(reason string) {
	s.once.Do(func() {
		s.shutdown.Store(true)
		slog.Info("shutting down", "reason", reason)
		// Unblock Start if it is waiting.
		close(s.stopCh)
	})
}

// finish releases everything, in the order that lets in-flight work land first:
// the reconciler stops probing, sessions are marked DEAD (retaining history),
// the transports close, and only then are the logs closed so the last bytes are
// durable.
func (s *Server) finish() {
	if s.extensions != nil {
		s.extensions.Stop()
	}
	if s.sessMgr != nil {
		s.sessMgr.MarkAllDead()
	}
	if s.sshSrv != nil {
		s.sshSrv.Stop()
	}
	if s.mcp != nil {
		_ = s.mcp.Stop()
	}
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			slog.Warn("closing output logs", "err", err)
		}
	}
}

// logSurfaces prints what this instance serves, so an operator can tell an
// instance that is not answering from one that is not listening.
func (s *Server) logSurfaces() {
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")
	if s.verifier != nil {
		slog.Info("HTTP authentication: enabled (API/MCP: Authorization: Bearer <token>; browsers: native login prompt, token as password)")
	} else if s.cfg.DisableAuth {
		slog.Warn("HTTP authentication: DISABLED on purpose (--disable-auth / $" + config.EnvDisableAuth + "); anyone who can reach this port can drive every session")
	} else {
		slog.Info("HTTP authentication: disabled (loopback-only bind)")
	}
	slog.Info("MCP HTTP:")
	slog.Info("    /sse SSE transport")
	slog.Info("    /stream (streamable HTTP per MCP spec, e.g. Open WebUI)")
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")
}

// viaDialer answers `--via <profile>` for the in-process MCP bridge: the dialer
// it returns makes its connections from THAT profile's machine, so a template can
// point at `http://127.0.0.1:8080` and mean the far host's loopback rather than
// this one (#233).
//
// The lease is held for the whole bridge lifetime and released when it ends, for
// the reason the bridge exists: one MCP session makes many requests to one host,
// and a per-request lease would pay a handshake for each. It borrows the same
// transport pool ordinary sessions use, so a template that reaches one host does
// not open a second connection to a host termcp is already connected to.
func viaDialer(sessMgr *session.Manager, sshStore *sshconfig.Store, noInternal bool) mcpbridge.ViaDialer {
	return func(ctx context.Context, profile string) (func(network, addr string) (net.Conn, error), func(), error) {
		profile = strings.TrimSpace(profile)
		if profile == "" {
			return nil, nil, errors.New("--via needs a profile name")
		}
		cfg := session.Config{Ctx: ctx}
		if !sshconfig.IsInternalName(profile) {
			entry, err := sshStore.Load(profile)
			if err != nil {
				if errors.Is(err, sshconfig.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
					return nil, nil, fmt.Errorf("%w: %q. `ls host` lists the saved profiles", sshconfig.ErrNotFound, profile)
				}
				return nil, nil, err
			}
			remote, err := sshconfig.RemoteFromEntry(entry, sshStore.ConfigDir(profile))
			if err != nil {
				return nil, nil, err
			}
			cfg.Remote = remote
			cfg.SSHConfig = profile
		} else if noInternal {
			return nil, nil, errors.New("the built-in loopback profile is disabled on this instance")
		}
		lease, err := sessMgr.AcquireTransport(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		if lease == nil || lease.Client == nil {
			if lease != nil && lease.Release != nil {
				lease.Release()
			}
			return nil, nil, fmt.Errorf("no SSH connection to %q", profile)
		}
		dial := func(network, addr string) (net.Conn, error) {
			conn, derr := lease.Client.Dial(network, addr)
			if derr != nil {
				return nil, derr
			}
			// An SSH channel carries no deadlines of its own, so the wrapper drops
			// them rather than letting a caller's HTTP deadline surface as an error
			// from SetDeadline. Same wrapper `mcp open` uses for the same connection.
			return jrpc.NoDeadlineConn(conn), nil
		}
		return dial, lease.Release, nil
	}
}

// LogHandler builds the slog handler the binary installs: one line per event,
// coloured when stderr is a terminal.
func LogHandler(level string) slog.Handler {
	var minLevel slog.Level
	switch level {
	case "debug":
		minLevel = slog.LevelDebug
	case "warn":
		minLevel = slog.LevelWarn
	case "error":
		minLevel = slog.LevelError
	default:
		minLevel = slog.LevelInfo
	}
	color := false
	if fi, err := os.Stderr.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		color = true
	}
	return logansi.NewTextHandler(os.Stderr, logansi.Options{MinLevel: minLevel, Color: color})
}
