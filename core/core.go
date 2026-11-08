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
//	defer srv.Close("done")
//	if err := srv.Start(); err != nil { return err }   // brings the instance up
//	return srv.Listen()                                // binds cfg.Host:cfg.Port
//
// Or in-process, with no listener at all — the difference is WHICH CALLS YOU
// MAKE, not a flag:
//
//	srv, err := core.New(core.Options{Config: cfg})
//	if err != nil { return err }
//	defer srv.Close("done")
//	if err := srv.Start(); err != nil { return err }
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

	started   atomic.Bool
	listening atomic.Bool
	finished  atomic.Bool
	shutdown  atomic.Bool
	once      sync.Once
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
// opens a listening socket; Listen does that, so a construction error is always a
// configuration error and never a half-started server.
//
// THE PORT IS NOT CHECKED HERE. Whether a port is bound is now a question about
// a CALL (Listen), not about a field, so validating it at construction would
// reject a perfectly good instance on behalf of a step nobody asked for. The port
// is checked by Listen, which is the only code that needs it.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("core: Options.Config is required")
	}
	cfg := opts.Config
	if err := cfg.ValidateInstance(); err != nil {
		return nil, err
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

	// THE ASSET DIRECTORIES ARE APPLIED HERE, from Config, rather than expected of
	// the caller: the line below reads webui.Assets() for the MCP docs FS, so a
	// caller that configured Config.AssetsDir and called New would otherwise get
	// the embedded bytes — a silently wrong answer to a question it asked
	// correctly. Setting it once here is what makes the field a setting instead of
	// a global the caller must remember to poke first.
	// ponytail: process-wide, so two instances in one program share one override.
	webui.SetAssetsDir(cfg.AssetsDir)
	webui.SetThemesDir(cfg.ThemesDir)

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
	if s.cfg.Headless {
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
// After a Start that returned nil it answers immediately — Start is synchronous
// now, so this is a belt for callers that launch it in a goroutine rather than a
// requirement. It stays because those callers exist and because "has anything
// gone wrong yet" deserves an answer that is not a sleep.
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

// Start brings the instance up: the internal SSH transport, the MCP surface
// (including the documentation prompt), the extension reconciler, and the
// readiness signal. It returns as soon as the instance can answer Term.
//
// It opens no socket. That is Listen's job, and it is a separate call precisely so
// that "I do not want a port" is expressed by not making that call rather than by
// a flag that decides for everyone what building the instance means.
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

	// THE MCP SURFACE COMES UP HERE, WITH THE OTHER SURFACES, because it is not a
	// socket. This call resolves the base URL the documentation prompt advertises
	// and registers that prompt — work an embedder needs and a port has nothing to
	// do with. It used to sit inside the listening branch, so an instance that never
	// bound a port served MCP without its docs (see Listen's comment).
	if err := s.mcp.Start(s.addr); err != nil {
		s.finish()
		return err
	}

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

	return nil
}

// Listen binds the configured address and serves until the instance stops. It
// returns nil for a clean shutdown and an error otherwise.
//
// Listen blocks: an embedder that wants a background instance should run it in a
// goroutine, the same way the binary does.
//
// THE DECISION TO SERVE LIVES HERE, NOT IN A FLAG. This used to be
// Options.NoListener, one boolean cutting Start into two shapes, and the cut was
// wrong in both directions: the flag that meant "do not open a socket" also
// skipped mcp.Start, which registers the documentation prompt and resolves the
// base URL — neither of which has anything to do with binding a port. So an SDK
// instance served MCP without its docs, and the log line the operator read said
// "listening: no" about a call that was never going to happen. Now the surfaces
// come up in Start and only the bind is a separate step, which a caller declines
// by not taking it.
//
// A program that never calls Listen has no address to advertise, and Addr keeps
// reporting what Config names. That is a statement about the configuration, not a
// promise that something is listening: the code that opens the socket is the same
// code that could say otherwise, and it is this function.
func (s *Server) Listen() error {
	if !s.started.Load() {
		return errors.New("core: Listen requires Start, which brings the surfaces up")
	}
	// The port is validated HERE, at the one point its value is a constraint, so
	// that an instance which is never bound is not rejected for a port it never uses.
	//
	// BEFORE the flag is claimed: a Listen that failed on its arguments left the
	// instance marked as serving, and Close then deferred the release to a server
	// loop that never started — reconciler probing and store open, forever.
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	if !s.listening.CompareAndSwap(false, true) {
		return errors.New("core: Listen called twice on one instance")
	}
	// A CLOSED INSTANCE DOES NOT OPEN ITS PORT — and this has to be checked AFTER
	// the flag, not before it. Close sets shutdown and then asks whether anything is
	// listening; whoever claims the flag must re-read shutdown, or the two can
	// interleave into a server bound on a released store. Checking shutdown first
	// only narrows the window: Close could still run between that read and this CAS,
	// decide nobody is listening, finish, and leave this call to start serving.
	//
	// The finish here is what keeps the hand-off complete: in the interleaving where
	// Close saw the flag already claimed, it deliberately did not release, so this
	// path must. finish is idempotent, so the other interleaving costs nothing.
	if s.shutdown.Load() {
		s.finish()
		return errors.New("core: Listen after Close; this instance is already released")
	}

	// The MCP surface is mounted on the shared mux, so this server is the only
	// listener: one ListenAndServe carries every route (MCP, REST, Web UI) on that
	// mux.
	//
	// It used to be the SSE transport that blocked here, with WithHTTPServer
	// handing it s.httpSrv — so the whole instance was served by a dependency's
	// Serve call. Now that the transports are handlers, serving is this server's
	// own job, which is also what makes `Handler()` (embedding, tests) and
	// ListenAndServe two ways of running the same thing.
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
		// Close does not wait for Listen, and an unaddressed http.Server is a
		// listener. Without this the graceful stop is a no-op on the socket, the
		// port stays claimed, and the process that only meant to embed termcp cannot
		// explain why. It runs HERE rather than in finish because this is the call
		// that is serving, and it is bounded for the same reason: a handler wedged on
		// the network must not turn a shutdown into a hang.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(ctx)
		s.finish()
		slog.Info("server stopped")
		return nil
	}
}

// Close is the one exit path: a signal, the daemon countdown, an embedder's
// defer, and a stop request all land here, and the first one wins. reason is
// only for the log.
//
// It does not wait. That is what makes it usable from a request handler — the
// daemon's stop endpoint calls it and must be able to answer first — and the cost
// is that Close returning says the instance was ASKED to stop, not that it has
// stopped. Who releases the resources depends on whether anyone is blocked on that
// answer:
//
//	Listen running   Listen observes stopCh, shuts the socket down, finishes
//	Listen absent    Close finishes, because nothing else is left to notice
//
// Without the second half an instance that was started and never served would keep
// its reconciler probing and its store open after Close returned — a shutdown that
// looks complete and is not.
func (s *Server) Close(reason string) {
	s.once.Do(func() {
		s.shutdown.Store(true)
		slog.Info("shutting down", "reason", reason)
		// Unblock Listen if it is waiting.
		close(s.stopCh)
		if !s.listening.Load() {
			s.finish()
		}
	})
}

// finish releases everything, in the order that lets in-flight work land first:
// the reconciler stops probing, sessions are marked DEAD (retaining history),
// the transports close, and only then are the logs closed so the last bytes are
// durable.
//
// Guarded rather than documented-as-once, because the two owners above are
// different goroutines and a double Close is already legal.
func (s *Server) finish() {
	if !s.finished.CompareAndSwap(false, true) {
		return
	}

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
// and a per-request lease would pay a handshake for each. It uses the same
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
