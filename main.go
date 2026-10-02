package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/logansi"
	mcpmod "github.com/open-mcp-ai/termcp/internal/mcp"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/internal/webui"
)

// Build metadata. Release builds override these with -ldflags (the Makefile's
// LDFLAGS_VERSION does), e.g.
//
//	go build -ldflags "-X main.version=v1.2.3 -X main.commit=$(git rev-parse --short HEAD) \
//	  -X main.date=$(date -u +%FT%TZ)"
//
// When they are not injected, versionString falls back to the module version
// the Go toolchain embeds: the release tag for a `go install module@vX.Y.Z`
// build, a vX.Y.Z-0.<time>-<commit> pseudo-version (`+dirty` on a modified
// tree) for a `go build` inside a checkout. `dev` is what remains when no
// module version was embedded at all — `-buildvcs=false`, an unpacked source
// tarball, or `go run`, which records build settings but no version. So
// `termcp -version` always says something truthful, and `dev` means "this
// binary carries no version information", not "this is a git checkout".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// versionString returns the build version: the ldflags-injected one when present,
// else the Go toolchain's embedded module version (the release tag for a
// `go install module@version` build). `dev` is the last resort — no version was
// injected and none was embedded, which is also the value `version` starts at.
func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

// printVersion writes the version line to w.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "termcp %s", versionString())
	if commit != "" {
		fmt.Fprintf(w, " (commit %s)", commit)
	}
	if date != "" {
		fmt.Fprintf(w, " built %s", date)
	}
	fmt.Fprintln(w)
}

func logHTTPMain(base string, port int, lanIPv4 []string) {
	slog.Info(base + "/")
	for _, ip := range lanIPv4 {
		slog.Info(fmt.Sprintf("http://%s:%d/", ip, port))
	}
}

func main() {
	cfg := config.Default()
	flag.StringVar(&cfg.Host, "host", cfg.Host, "HTTP bind address (127.0.0.1 = loopback default; 0.0.0.0 = all interfaces)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data directory for JSON storage (default: $TERMCP_DATA_DIR or ~/.termcp)")
	flag.StringVar(&cfg.AssetsDir, "assets", cfg.AssetsDir, "External static assets directory: files there override the embedded Web UI and docs; missing files fall back to the embed (default: $TERMCP_ASSETS_DIR or ~/.termcp/assets)")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log verbosity: debug|info|warn|error")
	flag.BoolVar(&cfg.NoInternal, "no-internal", cfg.NoInternal, "Disable the built-in loopback SSH profile (no internal connection)")
	flag.BoolVar(&cfg.MCPManageSSHConfigs, "mcp-manage-ssh-configs", cfg.MCPManageSSHConfigs, "Enable MCP tools to create/edit/delete SSH configs (off by default; passwords/keys are never exposed)")
	flag.StringVar(&cfg.AuthToken, "auth-token", cfg.AuthToken, "HTTP authentication token (or $TERMCP_AUTH_TOKEN)")
	flag.StringVar(&cfg.AuthHash, "auth-hash", cfg.AuthHash, "Salted SHA-256 HTTP token hash (or $TERMCP_AUTH_HASH; generate with 'termcp --gen-auth-hash')")
	flag.BoolVar(&cfg.DisableAuth, "disable-auth", cfg.DisableAuth, "Disable HTTP authentication entirely, even on non-loopback binds (or TERMCP_DISABLE_AUTH_TOKEN=1). Errors out if a token or hash is also configured.")
	flag.BoolVar(&cfg.MCPDeferTools, "mcp-defer-tools", cfg.MCPDeferTools, "Mark low-frequency MCP tools with defer_loading so clients fetch them on demand. Off by default: every tool is listed eagerly, which is what clients without deferred-tool support (e.g. Codex behind a gateway) need.")
	var genAuthHash bool
	flag.BoolVar(&genAuthHash, "gen-auth-hash", false, "Generate the salted SHA-256 hash of a token for --auth-hash / $TERMCP_AUTH_HASH, then exit (token from an argument, or from stdin without echo on a terminal)")
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "Print version, commit, and build date, then exit")
	var idleTimeoutArg string
	flag.StringVar(&idleTimeoutArg, "idle-timeout", "", "How long a daemon instance may stay fully idle before it exits on its own (e.g. 10m; 0 disables). Defaults: 30s for termcp daemon stdio, none for termcp daemon start. Only meaningful when a daemon instance is launched.")
	flag.Usage = usage
	// The subcommand leads the command line and is picked off before flag
	// parsing, which stops at the first positional argument.
	subName, action, rest, err := parseSubcommand(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// daemonCmd scopes the daemon action: set only when the daemon
	// subcommand led the command line.
	daemonCmd := ""
	if subName == "daemon" {
		daemonCmd = action
	}
	if subName == "stdio" && action == actionHelp {
		usage()
		return
	}
	// Both bridge forms carry their optional endpoint as a plain word right
	// after the name or action (`termcp stdio sse`, `termcp daemon stdio
	// sse`); split it off before the flag parser sees it. bridging keeps this
	// process on as the stdio bridge; ensure makes sure an instance answers
	// at --host/--port first.
	bridging := subName == "stdio" || daemonCmd == actionStdio
	ensure := daemonCmd == actionStart || daemonCmd == actionStdio
	endpoint := ""
	if bridging {
		endpoint, rest = splitStdioEndpoint(rest)
	}
	_ = flag.CommandLine.Parse(rest)

	if showVersion {
		printVersion(os.Stdout)
		return
	}

	if genAuthHash {
		if err := runGenAuthHash(flag.Args()); err != nil {
			fmt.Fprintf(os.Stderr, "gen-auth-hash: %v\n", err)
			os.Exit(2)
		}
		return
	}
	cfg.ApplyEnv()

	if args := flag.Args(); len(args) > 0 {
		if sub, ok := findSubcommand(args[0]); ok {
			// A word that is both a subcommand and an action of the namespace
			// already in play (`termcp daemon start stdio`) is a misplaced
			// action, not a misplaced subcommand; fall through to the
			// suggestion below, which points at `termcp daemon stdio`.
			if name, isAction := findAction(args[0]); !isAction || name != subName {
				// The subcommand must lead, but the flag parser already
				// stopped before it; everything behind it is still unparsed.
				fmt.Fprintf(os.Stderr, "the %s subcommand must come first, before any other argument: termcp %s %s [flags]\n",
					sub.name, sub.name, sub.argHint)
				os.Exit(2)
			}
		}
		fmt.Fprintf(os.Stderr, "unknown arguments: %s\n", strings.Join(args, " "))
		if len(args) == 1 {
			if name, ok := findAction(args[0]); ok {
				fmt.Fprintf(os.Stderr, "did you mean `termcp %s %s`?\n", name, args[0])
			}
		}
		os.Exit(2)
	}

	// The marker environment variable is set by the front-end that spawned
	// this process; it is what turns on the idle countdown and the daemon
	// identity this instance reports at GET /api/daemon.
	isDaemonChild := os.Getenv(daemon.EnvChild) != ""
	var idleTimeout time.Duration
	if idleTimeoutArg != "" {
		if daemonCmd != actionStart && daemonCmd != actionStdio && !isDaemonChild {
			fmt.Fprintln(os.Stderr, "--idle-timeout only applies when a daemon instance is launched (termcp daemon start or termcp daemon stdio)")
			os.Exit(2)
		}
		d, err := time.ParseDuration(idleTimeoutArg)
		if err != nil || d < 0 {
			fmt.Fprintf(os.Stderr, "invalid --idle-timeout %q: expected a duration like 10m, or 0 to disable\n", idleTimeoutArg)
			os.Exit(2)
		}
		idleTimeout = d
	}

	// The stdio forms' optional endpoint value names the endpoint the bridge
	// dials; resolve it once so the `stdio` subcommand and the daemon
	// subcommand's stdio action work from the same target.
	var target bridgeTarget
	if bridging {
		t, err := resolveBridgeTarget(daemon.BaseURL(cfg.Host, cfg.Port), endpoint)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// `daemon stdio` can only guarantee the instance at --host/--port; a
		// bridge to somewhere else is a contradiction, not a combination.
		if daemonCmd == actionStdio && !targetOnLocalInstance(cfg, t.URL) {
			fmt.Fprintf(os.Stderr, "daemon stdio can only ensure the instance at %s; to bridge to %s, run `termcp daemon start` and `termcp stdio %s` as two steps\n",
				daemon.BaseURL(cfg.Host, cfg.Port), t.URL, endpoint)
			os.Exit(2)
		}
		target = t
	}

	// Data dir precedence: --data-dir flag > $TERMCP_DATA_DIR > ~/.termcp.
	// A fixed per-user location keeps storage in one predictable place no
	// matter where the binary is installed or from which directory it runs.
	if cfg.DataDir == "" {
		dir, err := config.DefaultDataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve default data dir: %v\n", err)
			os.Exit(1)
		}
		cfg.DataDir = dir
	}
	// Management actions need nothing but the endpoint (status/stop) or a
	// printer (help); dispatch before anything creates files, so a status
	// query stays a pure HTTP probe.
	if subName == "daemon" && daemonCmd == "" {
		runDaemonBrief(os.Stdout)
		return
	}
	if daemonCmd == actionHelp {
		runDaemonHelp(os.Stdout)
		return
	}
	if daemonCmd == actionStop || daemonCmd == actionStatus {
		runDaemonManagement(cfg, daemonCmd == actionStop)
		return
	}

	// Fail fast when the data directory cannot be created or written.
	if err := ensureWritableDir(cfg.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "data dir %q is not writable: %v\n", cfg.DataDir, err)
		os.Exit(1)
	}

	// Assets precedence: --assets flag > $TERMCP_ASSETS_DIR > ~/.termcp/assets.
	// Unlike the data dir this one is never created and never required: an absent
	// directory simply leaves every asset served from the embed, so a stock
	// install behaves exactly as if the option did not exist.
	if cfg.AssetsDir == "" {
		dir, err := config.DefaultAssetsDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve default assets dir: %v\n", err)
			os.Exit(1)
		}
		cfg.AssetsDir = dir
	}
	// An existing path that is a file can never serve assets; refusing it beats
	// starting with an override that silently does nothing.
	if st, err := os.Stat(cfg.AssetsDir); err == nil && !st.IsDir() {
		fmt.Fprintf(os.Stderr, "assets %q is not a directory\n", cfg.AssetsDir)
		os.Exit(1)
	}
	// Must run before any consumer reads Assets(): the MCP docs FS and the Web UI
	// static server both resolve through it.
	webui.SetAssetsDir(cfg.AssetsDir)

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(2)
	}

	if ensure || bridging {
		runDaemonFront(cfg, ensure, bridging, idleTimeoutArg, idleTimeout, target)
		return
	}

	var verifier *auth.Verifier
	if cfg.AuthToken != "" || cfg.AuthHash != "" {
		v, err := auth.NewVerifier(cfg.AuthToken, cfg.AuthHash)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid auth config: %v\n", err)
			os.Exit(2)
		}
		verifier = v
	}

	slog.SetDefault(slog.New(buildLogHandler(cfg)))
	slog.Info("termcp server started", "version", versionString())

	// Start internal SSH server (in-process, no TCP port) unless disabled.
	var sshSrv *sshserver.Server
	if !cfg.NoInternal {
		sshSrv = sshserver.New()
		if err := sshSrv.Start(); err != nil {
			slog.Error("failed to start SSH server", "err", err)
			os.Exit(1)
		}
	}
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")

	if verifier != nil {
		slog.Info("HTTP authentication: enabled (API/MCP: Authorization: Bearer <token>; browsers: native login prompt, token as password)")
	} else if cfg.DisableAuth {
		slog.Warn("HTTP authentication: DISABLED on purpose (--disable-auth / $" + config.EnvDisableAuth + "); anyone who can reach this port can drive every session")
	} else {
		slog.Info("HTTP authentication: disabled (loopback-only bind)")
	}

	slog.Info("MCP HTTP:")
	slog.Info("    /sse SSE transport")
	slog.Info("    /stream (streamable HTTP per MCP spec, e.g. Open WebUI)")
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")

	// Initialize storage and managers
	store := storage.New(cfg.DataDir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, sshSrv)
	if err := sessMgr.RestoreDead(); err != nil {
		slog.Warn("failed to restore previous DEAD sessions", "err", err)
	}

	sshStore := sshconfig.NewStore(cfg.DataDir)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	mux := http.NewServeMux()
	mainSrv := &http.Server{Addr: addr, Handler: mux}

	forwardMgr := forward.NewForwardManager()

	mcpOpts := []mcpmod.Option{mcpmod.WithHTTPServer(mainSrv)}
	if cfg.MCPDeferTools {
		// Opt-in: tag low-frequency tools defer_loading so clients fetch their
		// schemas on demand. Off by default because clients that drop the marker
		// (any Codex talking through a gateway, for instance) would lose those
		// tools entirely rather than merely load them later.
		mcpOpts = append(mcpOpts, mcpmod.DeferTools())
	}
	mcpSrv := mcpmod.New(sessMgr, msgMgr, sshStore, forwardMgr, versionString(), mcpOpts...)
	// Same embedded docs the Web UI serves over HTTP become MCP resources/prompts.
	mcpSrv.SetDocsFS(webui.Assets())
	mcpSrv.NoInternal = cfg.NoInternal
	if cfg.MCPManageSSHConfigs {
		mcpSrv.RegisterSSHConfigWriteTools()
	}
	mux.Handle("GET /sse", mcpSrv.SSEHandler())
	mux.Handle("POST /message", mcpSrv.MessageHandler())
	mux.Handle("/stream", mcpSrv.StreamableHTTPHandler())
	webuiH := &webui.Handler{Sessions: sessMgr, SSH: sshStore, ForwardMgr: forwardMgr, NotifyMgr: mcpSrv.NotifyManager(), NoInternal: cfg.NoInternal, Version: versionString(), Daemon: isDaemonChild, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	// The effective countdown is published at GET /api/daemon so the stdio
	// bridge can ping faster than it (see daemon.InstanceInfo.IdleTimeoutMS).
	if isDaemonChild {
		webuiH.DaemonLog = daemon.LogPath(cfg.DataDir)
		webuiH.IdleTimeout = daemon.DefaultIdleTimeout
		if idleTimeoutArg != "" {
			webuiH.IdleTimeout = idleTimeout
		}
	}
	// File transfers and port forwards are held by the same review queue as
	// command lines, but the operations live with the MCP server (it owns the
	// SFTP and forward machinery). The Web UI decides; this is how its decision
	// gets replayed where the operation is implemented.
	webuiH.ExecuteOperation = mcpSrv.ExecuteApprovedOperation
	webuiH.Register(mux)
	// Bridge the MCP notify_user tool to the browser UI (toast/highlight push).
	mcpSrv.SetUINotifier(webuiH.BroadcastUINotify)

	// An approval decision must also wake an agent parked on shell_notify, so it
	// learns the outcome without polling. Fan-out stays in one place: the web UI
	// keeps its own listener (installed in Register) and this one forwards the
	// same transitions to the notify subsystem's rules.
	sessMgr.AddApprovalListener(func(sessionID string, req approval.Request) {
		if req.ShellID != "" {
			mcpSrv.NotifyManager().OnApprovalChange(req.ShellID, "approval "+string(req.State))
		}
	})
	if verifier != nil {
		mainSrv.Handler = auth.Middleware(verifier, mux)
	}

	host := strings.TrimSpace(cfg.Host)
	base := fmt.Sprintf("http://%s:%d", host, cfg.Port)
	var lan []string
	if bindHostIsAll(host) {
		lan = nonLoopbackUnicastIPv4s()
	}
	logHTTPMain(base, cfg.Port, lan)

	var shuttingDown atomic.Bool
	var shutdownOnce sync.Once

	// gracefulShutdown is the one exit path for both a signal and a daemon's
	// idle countdown; once it has run, a second trigger is a no-op.
	gracefulShutdown := func(reason string) {
		shutdownOnce.Do(func() {
			shuttingDown.Store(true)
			slog.Info("shutting down", "reason", reason)
			// Disconnect ≠ delete: DEAD all running sessions (retain history) instead
			// of killing/clearing every shell.
			sessMgr.MarkAllDead()
			if sshSrv != nil {
				sshSrv.Stop()
			}
			mcpSrv.Stop()
			// The store keeps a shell's log open for appending; close it so the last
			// bytes are durable and no handle outlives the process.
			if err := store.Close(); err != nil {
				slog.Warn("closing output logs", "err", err)
			}
		})
	}

	// The daemon stop endpoint asks for the same exit path a signal takes.
	webuiH.StopDaemon = func() { gracefulShutdown("daemon stop") }

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		gracefulShutdown("signal")
	}()

	// Daemon children carry the idle countdown that is their normal way of
	// ending. The watcher starts counting at creation — a fresh instance has
	// seen no activity yet — and management probes are exempt, so a status
	// query never keeps alive the instance it is asking about. The predicate
	// lives with the probe itself (daemon.IsProbe): the probe falls back to the
	// public /api.md fingerprint when /api/daemon is rejected, and that
	// fallback must be exempt too, or a status query on a guarded instance
	// would feed the very countdown it reports on.
	if isDaemonChild {
		timeout := daemon.DefaultIdleTimeout
		if idleTimeoutArg != "" {
			timeout = idleTimeout
		}
		watcher := daemon.NewIdleWatcher(timeout, func() { gracefulShutdown("idle timeout") })
		watcher.Exempt = daemon.IsProbe
		mainSrv.Handler = watcher.Track(mainSrv.Handler)
		slog.Info("daemon instance active", "pid", os.Getpid(), "idle_timeout", timeout)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- mcpSrv.Start(addr) }()

	if err := <-serveErr; err != nil {
		if shuttingDown.Load() && errors.Is(err, http.ErrServerClosed) {
			slog.Info("server stopped")
			return
		}
		slog.Error("failed to start MCP server", "err", err)
		os.Exit(1)
	}
}

func buildLogHandler(cfg *config.Config) slog.Handler {
	var minLevel slog.Level
	switch cfg.LogLevel {
	case "debug":
		minLevel = slog.LevelDebug
	case "warn":
		minLevel = slog.LevelWarn
	case "error":
		minLevel = slog.LevelError
	default:
		minLevel = slog.LevelInfo
	}
	color := term.IsTerminal(int(os.Stderr.Fd()))
	return logansi.NewTextHandler(os.Stderr, logansi.Options{MinLevel: minLevel, Color: color})
}
