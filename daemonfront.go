package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
)

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
