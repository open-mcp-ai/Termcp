package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/open-mcp-ai/termcp/config"
	"github.com/open-mcp-ai/termcp/core"
	"github.com/open-mcp-ai/termcp/internal/daemon"
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
	flag.BoolVar(&cfg.Headless, "headless", cfg.Headless, "Serve no browser UI: the MCP tool surface, POST /api/term and the agent docs remain. This is what `go build -tags no_webui` compiles in, selectable at run time.")
	flag.BoolVar(&cfg.MCPManageSSHConfigs, "mcp-manage-ssh-configs", cfg.MCPManageSSHConfigs, "Enable the profile write actions of the `profile` directive (create/edit/copy/delete) over MCP (off by default; passwords/keys are never exposed)")
	flag.StringVar(&cfg.AuthToken, "auth-token", cfg.AuthToken, "HTTP authentication token (or $TERMCP_AUTH_TOKEN)")
	flag.StringVar(&cfg.AuthHash, "auth-hash", cfg.AuthHash, "Salted SHA-256 HTTP token hash (or $TERMCP_AUTH_HASH; generate with 'termcp --gen-auth-hash')")
	flag.BoolVar(&cfg.DisableAuth, "disable-auth", cfg.DisableAuth, "Disable HTTP authentication entirely, even on non-loopback binds (or TERMCP_DISABLE_AUTH_TOKEN=1). Errors out if a token or hash is also configured.")
	var genAuthHash bool
	flag.BoolVar(&genAuthHash, "gen-auth-hash", false, "Generate the salted SHA-256 hash of a token for --auth-hash / $TERMCP_AUTH_HASH, then exit (token from an argument, or from stdin without echo on a terminal)")
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "Print version, commit, and build date, then exit")
	var idleTimeoutArg string
	flag.StringVar(&idleTimeoutArg, "idle-timeout", "", "How long a daemon instance may stay fully idle before it exits on its own (e.g. 10m; 0 disables). Defaults: 30s for termcp daemon stdio, none for termcp daemon start. Only meaningful when a daemon instance is launched.")
	var bridgeNamespace string
	flag.StringVar(&bridgeNamespace, "namespace", "", "stdio bridge: claim a caller namespace (letters/digits/-_. up to 64). The instance then lists and accepts only sessions in it, so several agents bridging to the same instance do not collide. Empty = the instance owner's view, which is the default.")
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
		// A FLAG THAT DOES NOT APPLY IS NOT AN ERROR HERE (issue #152).
		//
		// This used to os.Exit(2), and the failure was SILENT to whatever launched the
		// process: Start-Process returns a handle, the parent moves on, and the port
		// simply never opens. The only way to tell "still starting" from "refused to
		// start" was to poll the port.
		//
		// The distinction that matters is not "was this flag typed" but "can this
		// invocation honour it". A value that cannot be PARSED is still an error — the
		// caller asked for something specific and is not getting it. A value that parses
		// but has no effect on a foreground instance is a no-op with a warning, because
		// a launcher passing a common set of flags is the normal case and the cost of
		// honouring it is one line on stderr.
		d, err := time.ParseDuration(idleTimeoutArg)
		if err != nil || d < 0 {
			fmt.Fprintf(os.Stderr, "invalid --idle-timeout %q: expected a duration like 10m, or 0 to disable\n", idleTimeoutArg)
			os.Exit(2)
		}
		if daemonCmd != actionStart && daemonCmd != actionStdio && !isDaemonChild {
			fmt.Fprintf(os.Stderr,
				"note: --idle-timeout %s has no effect on a foreground instance; it only applies to "+
					"a launched daemon (termcp daemon start / termcp daemon stdio). Continuing.\n", idleTimeoutArg)
		} else {
			idleTimeout = d
		}
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
	// The themes directory is optional in the same way, and resolved here because
	// only the CLI knows a user's home layout. core applies both to the asset FS.
	if uiThemesDir, err := config.DefaultThemesDir(); err == nil {
		cfg.ThemesDir = uiThemesDir
	} else {
		fmt.Fprintf(os.Stderr, "cannot resolve themes dir: %v\n", err)
		os.Exit(1)
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(2)
	}

	if ensure || bridging {
		runDaemonFront(cfg, ensure, bridging, idleTimeoutArg, idleTimeout, target, bridgeNamespace)
		return
	}
	runServer(cfg, idleTimeoutArg, idleTimeout, isDaemonChild)
}

// runServer is the serving half of the command: it hands the configuration to
// core, which builds the runtime, and then serves until a signal or a daemon
// idle countdown asks it to stop.
//
// It is separate from main because the two halves have different failure models.
// main only classifies the command line and prints to stderr; from here on the
// flags are already accepted, so messages go through slog and a startup failure
// exits the process - there is nothing sensible left to fall back to.
//
// The assembly itself lives in core (see core.Options). It moved there so that
// an embedder gets the same instance this binary gets, instead of a
// reimplementation of it; what remains here is the part that is genuinely about
// being a process: install the logger, take the signals, and turn a shutdown
// request into core.Close.
func runServer(cfg *config.Config, idleTimeoutArg string, idleTimeout time.Duration, isDaemonChild bool) {
	slog.SetDefault(slog.New(core.LogHandler(cfg.LogLevel)))

	srv, err := core.New(core.Options{
		Config:         cfg,
		Version:        versionString(),
		DaemonChild:    isDaemonChild,
		IdleTimeout:    idleTimeout,
		IdleTimeoutSet: idleTimeoutArg != "",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot start: %v\n", err)
		os.Exit(2)
	}

	host := strings.TrimSpace(cfg.Host)
	base := fmt.Sprintf("http://%s:%d", host, cfg.Port)
	var lan []string
	if bindHostIsAll(host) {
		lan = nonLoopbackUnicastIPv4s()
	}
	logHTTPMain(base, cfg.Port, lan)

	// SIGINT/SIGTERM and the daemon's stop endpoint all land on core.Close, which
	// runs once however many times it is asked.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close("signal")
	}()

	if err := srv.Start(); err != nil {
		slog.Error("failed to start server", "err", err)
		os.Exit(1)
	}
}
