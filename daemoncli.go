package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
)

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
