package mcpbridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
)

// BuiltinCommand is the name a client execs to get Run in-process instead of a
// process. The internal SSH server intercepts the name before any exec, so it
// never reaches a PATH lookup: a caller that asks for it gets this relay, and
// what it relays is entirely in the arguments.
//
// The point is the shape of the call, not the name. `termcp stdio <url>` is a
// second process reading its own stdin, which is what an extension template had
// to spawn to reach an HTTP MCP server; this is the same relay running inside
// the termcp that already owns the channel, so a template reaches a remote MCP
// server with no second binary, no PATH question, and no credential on a
// command line.
const BuiltinCommand = "termcp-mcp-bridge"

// CommandHandler parses BuiltinCommand's argv and runs the relay against the
// caller's stdio, returning the exit status the session should report.
//
// argv is everything after the command name: the endpoint URL, then zero or
// more `--header Name=Value` pairs. It is a plain io-based function — no ssh
// types — so the only package that knows both the transport and the relay is
// the one wiring them together.
func CommandHandler(ctx context.Context, in io.Reader, out io.Writer, errOut io.Writer, argv []string, via ViaDialer) int {
	cfg, err := ParseArgs(argv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", BuiltinCommand, err)
		return 2
	}
	// `--via <profile>` NAMES THE MACHINE THE CONNECTION LEAVES FROM (#233).
	// Without it every request leaves from this process, so a URL naming
	// 127.0.0.1 means the machine termcp runs on; with it the request is dialled
	// inside that host's SSH connection and 127.0.0.1 means THAT host. No port is
	// bound here, so two extensions reaching the same endpoint cannot collide and a
	// caller never has to pick a number.
	//
	// A resolver that is not wired is REFUSED, not ignored: falling back to a local
	// dial would quietly talk to a different server than the template asked for.
	if cfg.Via != "" {
		if via == nil {
			fmt.Fprintf(errOut, "%s: --via is not available on this instance (no SSH dialer is wired)\n", BuiltinCommand)
			return 2
		}
		dial, release, derr := via(ctx, cfg.Via)
		if derr != nil {
			fmt.Fprintf(errOut, "%s: cannot dial through %q: %v\n", BuiltinCommand, cfg.Via, derr)
			return 2
		}
		if release != nil {
			// The lease outlives every request this bridge makes and ends with it,
			// which is what the bridge's own lifetime already means.
			defer release()
		}
		cfg.DialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
			return dial(network, addr)
		}
	}
	cfg.In, cfg.Out, cfg.Err = in, out, errOut
	if err := Run(ctx, cfg); err != nil && ctx.Err() == nil {
		fmt.Fprintf(errOut, "%s: %v\n", BuiltinCommand, err)
		return 1
	}
	return 0
}

// ParseArgs turns BuiltinCommand's argv into a Config. The endpoint is the one
// positional argument; every flag is a header, because a header is the only
// thing this form needs beyond the URL. An endpoint whose path ends in /sse
// selects the SSE transport, the same rule the CLI applies to a pasted URL, so
// the two forms cannot disagree about what a URL means.
//
// A header is written either as `--header Name=Value` or as `--header=Name=Value`.
func ParseArgs(argv []string) (Config, error) {
	var cfg Config
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--header" || arg == "-header" {
			if i+1 >= len(argv) {
				return Config{}, fmt.Errorf("%s needs a Name=Value argument", arg)
			}
			i++
			if err := addHeader(&cfg, argv[i]); err != nil {
				return Config{}, err
			}
			continue
		}
		if arg == "--via" || arg == "-via" {
			if i+1 >= len(argv) {
				return Config{}, fmt.Errorf("%s needs a profile name after it", arg)
			}
			i++
			cfg.Via = strings.TrimSpace(argv[i])
			if cfg.Via == "" {
				return Config{}, fmt.Errorf("%s needs a profile name after it", arg)
			}
			continue
		}
		if rest, ok := cutViaPrefix(arg); ok {
			cfg.Via = strings.TrimSpace(rest)
			if cfg.Via == "" {
				return Config{}, fmt.Errorf("--via needs a profile name")
			}
			continue
		}
		if rest, ok := cutHeaderPrefix(arg); ok {
			if err := addHeader(&cfg, rest); err != nil {
				return Config{}, err
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return Config{}, fmt.Errorf("unknown argument %q (expected <url>, --header Name=Value, --via <profile>)", arg)
		}
		if cfg.URL != "" {
			return Config{}, fmt.Errorf("unexpected extra argument %q (only one endpoint is bridged)", arg)
		}
		cfg.URL = arg
	}
	if cfg.URL == "" {
		return Config{}, fmt.Errorf("an endpoint URL is required")
	}
	cfg.SSE = isSSEEndpoint(cfg.URL)
	return cfg, nil
}

// cutViaPrefix reports whether arg is the `--via=<profile>` spelling.
func cutViaPrefix(arg string) (string, bool) {
	if rest, ok := strings.CutPrefix(arg, "--via="); ok {
		return rest, true
	}
	return strings.CutPrefix(arg, "-via=")
}

// cutHeaderPrefix reports whether arg is the `--header=Name=Value` spelling and
// returns what follows the equals.
func cutHeaderPrefix(arg string) (string, bool) {
	if rest, ok := strings.CutPrefix(arg, "--header="); ok {
		return rest, true
	}
	return strings.CutPrefix(arg, "-header=")
}

// addHeader splits one Name=Value on its first equals and records it. The value
// is not trimmed: a credential's own leading or trailing space is its business.
func addHeader(cfg *Config, raw string) error {
	name, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("header %q must be Name=Value", raw)
	}
	if cfg.Headers == nil {
		cfg.Headers = make(map[string]string)
	}
	cfg.Headers[name] = value
	return nil
}

// isSSEEndpoint reports whether a URL selects the SSE transport: the same rule
// the CLI uses for a full URL, so `.../sse` means one thing everywhere.
func isSSEEndpoint(raw string) bool {
	return strings.HasSuffix(strings.TrimRight(strings.SplitN(raw, "?", 2)[0], "/"), "/sse")
}
