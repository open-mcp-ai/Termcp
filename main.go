package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
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
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
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

func bindHostIsAll(bind string) bool {
	switch strings.TrimSpace(bind) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}

// nonLoopbackUnicastIPv4s lists unique IPv4 addresses on up, non-loopback interfaces.
func nonLoopbackUnicastIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			s := v4.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
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

// runDaemonBrief is the bare subcommand's output: the actions only, with a
// pointer at the detailed help behind `termcp daemon --help`. It explains and
// exits; it never starts or stops anything.
func runDaemonBrief(w io.Writer) {
	daemonHelpHeader(w)
	fmt.Fprintln(w, "  termcp daemon status   report the instance answering at --host/--port")
	fmt.Fprintln(w, "  termcp daemon start    make sure one is running there")
	fmt.Fprintln(w, "  termcp daemon stop     stop the instance gracefully over HTTP")
	fmt.Fprintln(w, "  termcp daemon stdio    ensure an instance there, then bridge stdin/stdout MCP to it")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `termcp daemon --help` for the parameters and details.")
}

// daemonHelpHeader is the opening shared by the brief index and the detailed
// help: the title, and the line that pins where the action goes.
func daemonHelpHeader(w io.Writer) {
	fmt.Fprintln(w, "termcp daemon - manage the instance answering at --host/--port")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions (the action goes right after the subcommand, before its flags):")
}

// runDaemonHelp is the detailed help behind `termcp daemon --help` (and the
// help word): the actions and the parameters that influence them. Like the
// brief form it only explains, it never starts or stops anything by itself.
func runDaemonHelp(w io.Writer) {
	daemonHelpHeader(w)
	fmt.Fprintln(w, "  termcp daemon status   report the instance answering at --host/--port")
	fmt.Fprintln(w, "  termcp daemon start    make sure one is running there: reuse whatever answers")
	fmt.Fprintln(w, "                         (even an instance started manually), else launch a")
	fmt.Fprintln(w, "                         detached one and wait until it serves")
	fmt.Fprintln(w, "  termcp daemon stop     stop the instance gracefully over HTTP")
	fmt.Fprintln(w, "  termcp daemon stdio    ensure an instance like start does, then keep this")
	fmt.Fprintln(w, "                         process on as the stdio bridge to it")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every action goes through the endpoint itself, so an instance is found")
	fmt.Fprintln(w, "wherever it listens, no matter which data dir or platform started it. Only")
	fmt.Fprintln(w, "instances started as daemons (termcp daemon start, termcp daemon stdio) accept")
	fmt.Fprintln(w, "the stop; a manually started one is reported as running and must be stopped")
	fmt.Fprintln(w, "where it was started.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions other than stdio are non-blocking: they run, report, and exit. stdio")
	fmt.Fprintln(w, "turns the process into the bridge and blocks; a running bridge counts as a")
	fmt.Fprintln(w, "connection, so the instance it fronts stays up for as long as the bridge lives.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Parameters that influence these commands:")
	fmt.Fprintln(w, "  --host, --port          the endpoint itself (http://127.0.0.1:18765 by")
	fmt.Fprintln(w, "                          default): where an instance listens and where every")
	fmt.Fprintln(w, "                          action and the bridge address it. A wildcard bind")
	fmt.Fprintln(w, "                          such as 0.0.0.0 or :: is reached through loopback.")
	fmt.Fprintln(w, "  --auth-token, --auth-hash")
	fmt.Fprintln(w, "                          credential presented to the endpoint, matching what")
	fmt.Fprintln(w, "                          the instance expects ($TERMCP_AUTH_TOKEN /")
	fmt.Fprintln(w, "                          $TERMCP_AUTH_HASH); a hash-configured instance also")
	fmt.Fprintln(w, "                          accepts the hash string itself.")
	fmt.Fprintln(w, "  --idle-timeout          (start, stdio) how long the launched instance may sit")
	fmt.Fprintln(w, "                          idle before exiting on its own: 30s by default for")
	fmt.Fprintln(w, "                          stdio, none for start; 0 disables.")
	fmt.Fprintln(w, "  --data-dir              (start, stdio) where the launched instance keeps its")
	fmt.Fprintln(w, "                          data; its log is written to <data-dir>/termcp.log.")
	fmt.Fprintln(w, "  --assets, --log-level, --no-internal, --mcp-manage-ssh-configs,")
	fmt.Fprintln(w, "  --mcp-defer-tools, --disable-auth")
	fmt.Fprintln(w, "                          (start, stdio) inherited by the launched instance, so")
	fmt.Fprintln(w, "                          it serves what `termcp` with the same flags would.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The stdio bridge is a standalone command: a stdio-to-HTTP MCP bridge between")
	fmt.Fprintln(w, "this process's stdin/stdout and the termcp HTTP MCP endpoint at --host/--port")
	fmt.Fprintln(w, "(http://127.0.0.1:18765/stream by default; an instance must be answering")
	fmt.Fprintln(w, "there). It blocks until its input closes. The daemon subcommand's stdio")
	fmt.Fprintln(w, "action is the combined form that first makes sure an instance does:")
	fmt.Fprintln(w, "  termcp stdio                 standalone: relay stdin/stdout to the endpoint")
	fmt.Fprintln(w, "  termcp daemon stdio          combined: ensure an instance first, then relay")
	fmt.Fprintln(w, "The endpoint is the word right after stdio (or after the daemon action);")
	fmt.Fprintln(w, "without one the default above is bridged. A path form targets the same")
	fmt.Fprintln(w, "instance and picks the transport - /sse or /stream (bare words work too);")
	fmt.Fprintln(w, "a full http(s) URL targets any MCP HTTP endpoint, one whose path ends in")
	fmt.Fprintln(w, "/sse speaking the SSE transport:")
	fmt.Fprintln(w, "  termcp stdio /sse            the same instance, over the SSE transport")
	fmt.Fprintln(w, "  termcp stdio <http(s) URL>   any endpoint, e.g. a remote instance")
	fmt.Fprintln(w, "  termcp daemon stdio sse      combined, the same instance, over SSE")
	fmt.Fprintln(w, "Only an endpoint on the --host/--port instance combines with the daemon")
	fmt.Fprintln(w, "action. The bridge presents the same --auth-token / --auth-hash credential.")
}

// runDaemonManagement implements the daemon stop and status actions; both
// address the configured endpoint over HTTP only.
func runDaemonManagement(cfg *config.Config, stop bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := daemon.BaseURL(cfg.Host, cfg.Port)
	if stop {
		res, err := daemon.StopEndpoint(ctx, cfg.Host, cfg.Port, daemon.Credential(cfg))
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
			os.Exit(1)
		}
		switch {
		case res.Stopped:
			fmt.Printf("termcp daemon stopped (pid %d)\n", res.PID)
		case res.NotDaemon:
			fmt.Println("termcp daemon: not running")
			fmt.Printf("  note:    a manually started instance answers at %s; stop it where it was started\n", url)
		default:
			fmt.Println("termcp daemon: not running")
		}
		return
	}
	rep := daemon.InspectEndpoint(ctx, cfg.Host, cfg.Port, daemon.Credential(cfg))
	switch {
	case rep.Info.Daemon:
		fmt.Println("termcp daemon: running")
		fmt.Printf("  pid:     %d\n", rep.Info.PID)
		fmt.Printf("  url:     %s\n", url)
		if rep.Info.Version != "" {
			fmt.Printf("  version: %s\n", rep.Info.Version)
		}
		if rep.Info.Log != "" {
			fmt.Printf("  log:     %s\n", rep.Info.Log)
		}
	case rep.Unauthorized:
		fmt.Println("termcp daemon: running")
		fmt.Printf("  url:     %s\n", url)
		fmt.Println("  note:    authentication is required for details (pass --auth-token or --auth-hash)")
	case rep.Termcp:
		// A manually started instance serves the endpoint but has no
		// background lifecycle to report.
		fmt.Println("termcp daemon: running (manually started instance — not a background daemon)")
		if rep.Info.PID > 0 {
			fmt.Printf("  pid:     %d\n", rep.Info.PID)
		}
		fmt.Printf("  url:     %s\n", url)
		if rep.Info.Version != "" {
			fmt.Printf("  version: %s\n", rep.Info.Version)
		}
	default:
		fmt.Println("termcp daemon: not running")
	}
}

// termcp's two MCP HTTP endpoints, and where the stdio bridge lands by default.
const (
	mcpStreamPath = "/stream" // streamable HTTP, POST/GET/DELETE on one path
	mcpSSEPath    = "/sse"    // SSE transport: GET event stream, JSON-RPC to the endpoint event's URL
)

// bridgeTarget is the resolved stdio-form destination.
type bridgeTarget struct {
	URL string // full endpoint URL the bridge speaks to
	SSE bool   // the endpoint speaks the SSE transport
}

// resolveBridgeTarget turns the stdio form's optional value into the endpoint
// the bridge dials. Empty means the default endpoint of the instance at origin.
// Path forms — /sse, /stream, or the bare words — address that same instance;
// a full http(s) URL addresses any endpoint, and one whose path ends in /sse
// selects the SSE transport.
func resolveBridgeTarget(origin, raw string) (bridgeTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bridgeTarget{URL: origin + mcpStreamPath}, nil
	}
	if !strings.Contains(raw, "://") {
		path := raw
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		switch path {
		case mcpStreamPath:
			return bridgeTarget{URL: origin + mcpStreamPath}, nil
		case mcpSSEPath:
			return bridgeTarget{URL: origin + mcpSSEPath, SSE: true}, nil
		}
		return bridgeTarget{}, fmt.Errorf("invalid endpoint %q for stdio: expected an http(s) URL, or /sse or /stream for the instance at --host/--port", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return bridgeTarget{}, fmt.Errorf("invalid endpoint %q for stdio: expected an http(s) URL, or /sse or /stream for the instance at --host/--port", raw)
	}
	return bridgeTarget{URL: u.String(), SSE: strings.HasSuffix(strings.TrimRight(u.Path, "/"), mcpSSEPath)}, nil
}

// targetOnLocalInstance reports whether a bridge target addresses the instance
// at --host/--port — the one `daemon stdio` can ensure, and the one the
// friendly "start one first" preflight speaks about. Loopback spellings of the
// same address count as the same instance.
func targetOnLocalInstance(cfg *config.Config, target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if u.Port() != strconv.Itoa(cfg.Port) {
		return false
	}
	want := strings.ToLower(daemon.ConnectHost(cfg.Host))
	got := strings.ToLower(u.Hostname())
	if got == want {
		return true
	}
	return loopbackHosts[got] && loopbackHosts[want]
}

// loopbackHosts lists the spellings of "this machine" that address the same
// instance when comparing bridge targets.
var loopbackHosts = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}

// runDaemonFront implements `daemon start`, `daemon stdio` and the stdio
// subcommand: ensure the background instance if asked, then optionally relay
// stdio to it. In the bridge forms all status lines go to stderr, because
// stdout carries MCP messages only.
func runDaemonFront(cfg *config.Config, ensureDaemon, stdio bool, idleTimeoutArg string, idleTimeout time.Duration, target bridgeTarget) {
	out := io.Writer(os.Stdout)
	if stdio {
		out = os.Stderr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if ensureDaemon {
		// The two launcher actions differ in lifecycle: the stdio action's
		// instance belongs to the bridge session and gets the default countdown
		// (the bridge keeps it alive while attached), while a `daemon start`
		// instance is meant to run until stopped — or until an explicit
		// --idle-timeout says otherwise.
		spawnIdle := idleTimeoutArg
		if spawnIdle == "" {
			spawnIdle = "0"
			if stdio {
				spawnIdle = daemon.DefaultIdleTimeout.String()
			}
		}
		res, err := daemon.Ensure(ctx, cfg, spawnIdle)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
			os.Exit(1)
		}
		verb := "already running"
		if res.Spawned {
			verb = "started"
		}
		if res.Info.PID > 0 {
			fmt.Fprintf(out, "termcp daemon %s (pid %d)\n", verb, res.Info.PID)
		} else {
			fmt.Fprintf(out, "termcp daemon %s\n", verb)
		}
		fmt.Fprintf(out, "  url:  %s\n", daemon.BaseURL(cfg.Host, cfg.Port))
		switch {
		case res.Spawned:
			fmt.Fprintf(out, "  log:  %s\n", daemon.LogPath(cfg.DataDir))
			fmt.Fprintf(out, "  %s\n", daemonIdleHint(stdio, idleTimeoutArg, idleTimeout))
		case res.Unauthorized:
			if stdio {
				// The bridge would 401 on every message; say why before it starts.
				fmt.Fprintln(os.Stderr, bridgeAuthHint(cfg))
				os.Exit(1)
			}
			fmt.Fprintln(out, "  note: authentication is required for details (pass --auth-token or --auth-hash)")
		case !res.Info.Daemon && res.Info.PID > 0:
			fmt.Fprintln(out, "  note: the instance was started manually, so it has no idle countdown; stop it where it was started")
		case res.Info.Log != "":
			fmt.Fprintf(out, "  log:  %s\n", res.Info.Log)
		}
	}

	if stdio {
		// The instance's *actual* countdown decides how often the bridge must
		// ping to count as activity. Ask it (GET /api/daemon reports the
		// effective value) instead of assuming the default: a pre-existing
		// instance started with --idle-timeout 6s would otherwise die under a
		// bridge that pings every 10s. A manually started instance reports 0 (no
		// countdown), which needs no pings at all.
		keepAlive := bridgeKeepAlive(ensureDaemon, idleTimeoutArg, idleTimeout)
		if targetOnLocalInstance(cfg, target.URL) {
			rep := daemon.InspectEndpoint(ctx, cfg.Host, cfg.Port, daemon.Credential(cfg))
			if !ensureDaemon {
				// The bridge never starts anything on its own: say so plainly when
				// nothing answers, instead of failing mid-handshake. A custom
				// remote endpoint is the bridge's own failure to report.
				if !rep.Termcp {
					fmt.Fprintf(os.Stderr, "stdio bridge: no Termcp instance answers at %s; start one first: termcp daemon start\n", daemon.BaseURL(cfg.Host, cfg.Port))
					os.Exit(1)
				}
			}
			if rep.Unauthorized {
				fmt.Fprintln(os.Stderr, bridgeAuthHint(cfg))
				os.Exit(1)
			}
			if rep.Termcp {
				keepAlive = keepAliveFor(rep.Info.IdleTimeout())
			}
		}
		if err := stdioBridge(ctx, cfg, target, keepAlive); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "stdio bridge: %v\n", err)
			os.Exit(1)
		}
	}
}

// keepAliveFor is the ping interval at which a streamable bridge counts as a
// live connection against a countdown of the given length. A third of the
// countdown leaves room for a slow round trip; 250ms is the floor so a very
// short countdown still gets a usable interval. A disabled countdown (0) needs
// no pings.
func keepAliveFor(idle time.Duration) time.Duration {
	if idle <= 0 {
		return 0
	}
	if iv := idle / 3; iv >= 250*time.Millisecond {
		return iv
	}
	return 250 * time.Millisecond
}

// daemonIdleHint describes how the instance will end. The default depends on
// the launcher: the stdio action's instance counts down once the bridge goes
// away, while a `daemon start` instance runs until stopped.
func daemonIdleHint(stdio bool, idleTimeoutArg string, idleTimeout time.Duration) string {
	switch {
	case idleTimeoutArg != "" && idleTimeout == 0:
		return "idle auto-exit disabled (--idle-timeout 0)"
	case idleTimeoutArg != "":
		return fmt.Sprintf("exits after %s with no connections", idleTimeout)
	case stdio:
		return fmt.Sprintf("exits after %s with no connections (--idle-timeout adjusts this, 0 disables)", daemon.DefaultIdleTimeout)
	default:
		return "no idle auto-exit: runs until stopped (--idle-timeout adds a countdown)"
	}
}

// bridgeAuthHint is the one wording for "the endpoint wants a credential this
// invocation does not hold": both pre-bridge checks — the instance that was just
// ensured and the one that was already there — must say the same thing.
func bridgeAuthHint(cfg *config.Config) string {
	return fmt.Sprintf("stdio bridge: the instance at %s requires authentication: pass its token with --auth-token, or its hash with --auth-hash", daemon.BaseURL(cfg.Host, cfg.Port))
}

// splitStdioEndpoint pulls the bridge's optional endpoint value — the word
// right after `stdio` (the subcommand or the daemon action), as in `termcp
// daemon stdio sse` — out of rest. Anything flag-shaped (or absent) leaves
// the default endpoint.
func splitStdioEndpoint(rest []string) (endpoint string, out []string) {
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		return rest[0], rest[1:]
	}
	return "", rest
}

// stdioBridge relays the process's stdio MCP stream to the target endpoint,
// presenting whichever credential the configuration holds (see
// daemon.Credential).
func stdioBridge(ctx context.Context, cfg *config.Config, target bridgeTarget, keepAlive time.Duration) error {
	return mcpbridge.Run(ctx, mcpbridge.Config{
		URL:       target.URL,
		SSE:       target.SSE,
		Token:     daemon.Credential(cfg),
		KeepAlive: keepAlive,
		In:        os.Stdin,
		Out:       os.Stdout,
		Err:       os.Stderr,
	})
}

// bridgeKeepAlive is the ping interval to assume when the instance's actual
// countdown is unknown: the effective --idle-timeout when this invocation
// started the instance, the default otherwise. runDaemonFront prefers the
// value the instance itself reports (see daemon.InstanceInfo.IdleTimeoutMS) and
// only falls back here for an endpoint it could not probe — a remote one, or a
// build predating the field.
func bridgeKeepAlive(ensureDaemon bool, idleTimeoutArg string, idleTimeout time.Duration) time.Duration {
	timeout := daemon.DefaultIdleTimeout
	if ensureDaemon && idleTimeoutArg != "" {
		timeout = idleTimeout
	}
	return keepAliveFor(timeout)
}

func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(probe)
}

func runGenAuthHash(args []string) error {
	var token string
	switch len(args) {
	case 0:
		t, err := readTokenFromStdin()
		if err != nil {
			return err
		}
		token = t
	case 1:
		token = args[0]
	default:
		return errors.New("too many arguments: expected at most one token")
	}
	if token == "" {
		return errors.New("token must not be empty")
	}
	hash, err := auth.Hash(token)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}

func readTokenFromStdin() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Token: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
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
