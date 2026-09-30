package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
)

func TestResolveBridgeTarget(t *testing.T) {
	const origin = "http://127.0.0.1:18765"
	cases := []struct {
		raw     string
		wantURL string
		wantSSE bool
		wantErr bool
	}{
		{"", origin + "/stream", false, false},
		{"sse", origin + "/sse", true, false},
		{"stream", origin + "/stream", false, false},
		{"/sse", origin + "/sse", true, false},
		{"/stream", origin + "/stream", false, false},
		{"http://example.com:9000/sse", "http://example.com:9000/sse", true, false},
		{"https://example.com/sse", "https://example.com/sse", true, false},
		{"http://example.com:9000/mcp", "http://example.com:9000/mcp", false, false},
		{"ftp://example.com/sse", "", false, true},
		{"example.com:9000/sse", "", false, true},
		{"/other", "", false, true},
	}
	for _, c := range cases {
		got, err := resolveBridgeTarget(origin, c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("resolveBridgeTarget(%q) accepted, want error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveBridgeTarget(%q): %v", c.raw, err)
			continue
		}
		if got.URL != c.wantURL || got.SSE != c.wantSSE {
			t.Errorf("resolveBridgeTarget(%q) = %+v, want URL %s SSE %v", c.raw, got, c.wantURL, c.wantSSE)
		}
	}
}

func TestParseSubcommand(t *testing.T) {
	cases := []struct {
		in         []string
		wantName   string
		wantAction string
		wantRest   []string
		wantErr    string // substring the error must carry
	}{
		{in: nil},
		{in: []string{"--port", "9000"}, wantRest: []string{"--port", "9000"}},
		// The bare name asks for the brief index; help reaches the detail.
		{in: []string{"daemon"}, wantName: "daemon"},
		{in: []string{"daemon", "help"}, wantName: "daemon", wantAction: actionHelp},
		{in: []string{"daemon", "--help"}, wantName: "daemon", wantAction: actionHelp},
		{in: []string{"daemon", "start"}, wantName: "daemon", wantAction: actionStart},
		{in: []string{"daemon", "status"}, wantName: "daemon", wantAction: actionStatus},
		{in: []string{"daemon", "stop", "--port", "9000"}, wantName: "daemon", wantAction: actionStop, wantRest: []string{"--port", "9000"}},
		// The stdio forms' endpoint value lands in rest like any other
		// argument; main splits it off before flag parsing.
		{in: []string{"daemon", "stdio"}, wantName: "daemon", wantAction: actionStdio},
		{in: []string{"daemon", "stdio", "sse"}, wantName: "daemon", wantAction: actionStdio, wantRest: []string{"sse"}},
		{in: []string{"daemon", "start", "extra"}, wantName: "daemon", wantAction: actionStart, wantRest: []string{"extra"}},
		// The value namespace takes everything after its name.
		{in: []string{"stdio"}, wantName: "stdio"},
		{in: []string{"stdio", "sse"}, wantName: "stdio", wantRest: []string{"sse"}},
		{in: []string{"stdio", "help"}, wantName: "stdio", wantAction: actionHelp},
		{in: []string{"stdio", "--help"}, wantName: "stdio", wantAction: actionHelp},
		{in: []string{"stdio", "--port", "9000"}, wantName: "stdio", wantRest: []string{"--port", "9000"}},
		{in: []string{"daemon", "bogus"}, wantErr: `unknown action "bogus"`},
		{in: []string{"daemon", "--stdio"}, wantErr: "the action comes right after the subcommand"},
		{in: []string{"daemon", "--daemon"}, wantErr: "the action comes right after the subcommand"},
		// A word that merely starts like a namespace is a plain argument.
		{in: []string{"daemons"}, wantRest: []string{"daemons"}},
	}
	for _, c := range cases {
		name, action, rest, err := parseSubcommand(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("parseSubcommand(%v) error = %v, want containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSubcommand(%v): %v", c.in, err)
			continue
		}
		if name != c.wantName || action != c.wantAction {
			t.Errorf("parseSubcommand(%v) = (%q, %q), want (%q, %q)", c.in, name, action, c.wantName, c.wantAction)
		}
		if strings.Join(rest, " ") != strings.Join(c.wantRest, " ") {
			t.Errorf("parseSubcommand(%v) rest = %v, want %v", c.in, rest, c.wantRest)
		}
	}
}

// TestSplitStdioEndpoint pins how the stdio forms' optional endpoint value is
// split off rest: a bare word right after `stdio` (or the daemon action) is
// the endpoint, anything flag-shaped (or absent) leaves the default.
func TestSplitStdioEndpoint(t *testing.T) {
	cases := []struct {
		in           []string
		wantEndpoint string
		wantRest     []string
	}{
		{nil, "", nil},
		{[]string{"sse"}, "sse", nil},
		{[]string{"/sse"}, "/sse", nil},
		{[]string{"--port", "9000"}, "", []string{"--port", "9000"}},
		{[]string{"sse", "--port", "9000"}, "sse", []string{"--port", "9000"}},
	}
	for _, c := range cases {
		endpoint, rest := splitStdioEndpoint(c.in)
		if endpoint != c.wantEndpoint || strings.Join(rest, " ") != strings.Join(c.wantRest, " ") {
			t.Errorf("splitStdioEndpoint(%v) = (%q, %v), want (%q, %v)", c.in, endpoint, rest, c.wantEndpoint, c.wantRest)
		}
	}
}

// TestDaemonIdleHint pins the per-action default the hint advertises: the
// stdio action's instance counts down, a plain start's does not.
func TestDaemonIdleHint(t *testing.T) {
	cases := []struct {
		stdio   bool
		arg     string
		timeout time.Duration
		want    string
	}{
		{true, "", 0, "exits after " + daemon.DefaultIdleTimeout.String() + " with no connections (--idle-timeout adjusts this, 0 disables)"},
		{false, "", 0, "no idle auto-exit: runs until stopped (--idle-timeout adds a countdown)"},
		{true, "10m", 10 * time.Minute, "exits after 10m0s with no connections"},
		{false, "0", 0, "idle auto-exit disabled (--idle-timeout 0)"},
	}
	for _, c := range cases {
		if got := daemonIdleHint(c.stdio, c.arg, c.timeout); got != c.want {
			t.Errorf("daemonIdleHint(%v, %q, %v) = %q, want %q", c.stdio, c.arg, c.timeout, got, c.want)
		}
	}
}

// TestDaemonHelpLevels pins the split: the bare subcommand is the brief
// action index pointing at `--help`, and the detailed help owns the
// parameters section.
func TestDaemonHelpLevels(t *testing.T) {
	var brief, full bytes.Buffer
	runDaemonBrief(&brief)
	runDaemonHelp(&full)

	for _, want := range []string{"termcp daemon status", "termcp daemon start", "termcp daemon stop", "termcp daemon stdio", "termcp daemon --help"} {
		if !strings.Contains(brief.String(), want) {
			t.Errorf("brief help misses %q", want)
		}
	}
	if strings.Contains(brief.String(), "Parameters that influence") {
		t.Error("brief help carries the parameters section; it belongs to --help")
	}
	if !strings.Contains(full.String(), "Parameters that influence") {
		t.Error("detailed help misses the parameters section")
	}
}

func TestFindAction(t *testing.T) {
	// help is implicit for every subcommand, so it belongs to none of them.
	for word, want := range map[string]string{"start": "daemon", "stop": "daemon", "status": "daemon", "stdio": "daemon", "help": "", "bogus": ""} {
		got, ok := findAction(word)
		if want == "" {
			if ok {
				t.Errorf("findAction(%q) = %q, want no subcommand", word, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("findAction(%q) = %q, %v; want %q", word, got, ok, want)
		}
	}
}

func TestListWords(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"start"}, "start"},
		{[]string{"start", "stop"}, "start or stop"},
		{[]string{"start", "stop", "status"}, "start, stop or status"},
		{[]string{"start", "stop", "status", "stdio"}, "start, stop, status or stdio"},
	}
	for _, c := range cases {
		if got := listWords(c.in); got != c.want {
			t.Errorf("listWords(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBridgeKeepAlive(t *testing.T) {
	cases := []struct {
		ensure  bool
		arg     string
		timeout time.Duration
		want    time.Duration
	}{
		// No explicit timeout: whatever instance answers will run with the
		// default countdown, whether this invocation started it or not.
		{false, "", 0, daemon.DefaultIdleTimeout / 3},
		{true, "", 0, daemon.DefaultIdleTimeout / 3},
		// An explicit timeout only applies when this invocation passes it on.
		{true, "6s", 6 * time.Second, 2 * time.Second},
		{true, "9s", 9 * time.Second, 3 * time.Second},
		{false, "6s", 6 * time.Second, daemon.DefaultIdleTimeout / 3},
		// A disabled countdown needs no pings.
		{true, "0", 0, 0},
		// Pings stay frequent enough to matter for very short countdowns.
		{true, "1ms", time.Millisecond, 250 * time.Millisecond},
	}
	for _, c := range cases {
		got := bridgeKeepAlive(c.ensure, c.arg, c.timeout)
		if got != c.want {
			t.Errorf("bridgeKeepAlive(%v, %q, %v) = %v, want %v", c.ensure, c.arg, c.timeout, got, c.want)
		}
	}
}

func TestTargetOnLocalInstance(t *testing.T) {
	cfg := config.Default()
	cases := []struct {
		target string
		want   bool
	}{
		{daemon.BaseURL(cfg.Host, cfg.Port) + "/stream", true},
		{daemon.BaseURL(cfg.Host, cfg.Port) + "/sse", true},
		{"http://localhost:18765/sse", true},
		{"http://127.0.0.1:18766/sse", false},
		{"http://example.com:18765/sse", false},
		{"http://192.168.1.5:18765/sse", false},
	}
	for _, c := range cases {
		if got := targetOnLocalInstance(cfg, c.target); got != c.want {
			t.Errorf("targetOnLocalInstance(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

// The bridge must ping faster than the instance's *own* countdown, whichever it
// is. Assuming the 30s default is what let a pre-existing instance started with
// --idle-timeout 6s die under a bridge that pinged every 10s: the bridge is
// supposed to count as a connection, and it only does if it speaks before the
// countdown expires.
func TestKeepAliveForActualCountdown(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want time.Duration
	}{
		{0, 0},                               // never auto-exits: nothing to hold off
		{6 * time.Second, 2 * time.Second},   // a custom short countdown
		{30 * time.Second, 10 * time.Second}, // the default
		{10 * time.Minute, 3*time.Minute + 20*time.Second}, // a long countdown
		{300 * time.Millisecond, 250 * time.Millisecond},   // the floor
		{time.Millisecond, 250 * time.Millisecond},         // still the floor
	}
	for _, c := range cases {
		got := keepAliveFor(c.idle)
		if got != c.want {
			t.Errorf("keepAliveFor(%v) = %v, want %v", c.idle, got, c.want)
		}
		// Whatever the answer, it must be strictly shorter than the countdown it
		// is meant to keep alive -- otherwise the bridge loses the race it exists
		// to win. The 250ms floor is the one exception, and only for a countdown
		// shorter than the floor itself.
		if c.idle > 0 && got > 0 && got >= c.idle && c.idle > 250*time.Millisecond {
			t.Errorf("keepAliveFor(%v) = %v, which is not faster than the countdown", c.idle, got)
		}
	}
}
