// Package extensions implements termcp's extension layer: named, persisted
// templates for sessions whose capabilities are trimmed to what the template's
// author decided.
//
// The layer is deliberately thin. An extension does not add a second tool
// surface or a new transport — it is an ordinary session created from a stored
// template (command, mode, target profile) plus a Restriction (which tools the
// session accepts, which remote paths the file tools may touch) plus a note the
// agent reads as its operating instructions. The reconciler keeps each enabled
// extension's session warm and reports its state; everything else is the same
// session/shell/SFTP machinery an unrestricted session offers.
package extensions

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Forward kinds name a port-forward's direction, after the OpenSSH flags. A
// forward declared on an extension is set up by the reconciler when its session
// comes up and torn down with it, so the author describes the tunnel once and
// the lifecycle follows the session.
const (
	ForwardDynamic = "dynamic" // -D: termcp listens on a local SOCKS5 port
	ForwardLocal   = "local"   // -L: termcp listens locally, target dials remote_host:remote_port
	ForwardRemote  = "remote"  // -R: the target listens, connections reach remote_host:remote_port
)

// MaxConfigSize caps a single extension file. Templates are small; a cap keeps
// a runaway write from filling the data directory.
const MaxConfigSize = 1 << 20

// DefaultIdleTimeout is how long an on-demand extension stays up after its last
// use. Ten minutes is long enough to cover a run of calls that belong to one
// task and short enough that a forgotten extension is not a permanent process on
// the target.
const DefaultIdleTimeout = 10 * time.Minute

// Extension is one persisted template. Fields are grouped as the reader uses
// them: identity, the session template, the capability restriction, and the
// agent-facing note.
type Extension struct {
	// Identity. Name is the TOML-encoded file's directory name, not a TOML key,
	// so it is never stored inside the file it names.
	Name string `toml:"-"`
	// CAPABILITIES COME FROM WHAT THE TEMPLATE DECLARES, never from a label:
	// command, url, paths, forwards and mode are the whole vocabulary. A template
	// says what it wants to happen rather than which category it belongs to, which
	// is why there is no classification field to fill in.
	Target string `toml:"target,omitempty"` // legacy single-target profile name, or "internal"
	// Targets defines the hosts this extension runs on. When provided, each entry
	// names a host and its host-specific forwards, allowing different targets to bind
	// non-conflicting local ports while sharing the parent extension's command,
	// mode, and capabilities.
	Targets []TargetDecl `toml:"targets,omitempty"`
	// Description is the author's one-line summary, shown wherever the extension is
	// listed. It does NOT reach the model by itself: nothing about the set of
	// installed extensions goes into the MCP instructions, because the tool surface
	// must not grow with what is installed (see docs/clients.md).
	Description string `toml:"description,omitempty"`
	// Enabled is *bool so that an explicit false survives a TOML round trip; a
	// nil value means enabled, so a file that omits the key keeps working.
	Enabled *bool `toml:"enabled,omitempty"`

	// Session template — the fields of session.Config the author controls.
	// Mode is the primary shell's mode: "pty", "pipe", or "" for no shell at
	// all (a connection that exists to carry SFTP and forwards).
	//
	// There is deliberately no on_exit here. A session's OnExit closes the
	// container when its last shell ends by itself, which is the right answer for a
	// session someone started by hand and the wrong one for an extension: the
	// container is shared by every extension on the host, so one member's channel
	// exiting must not take the connection out from under the others. The
	// reconciler owns an extension session's lifecycle — it reopens a channel whose
	// process died and tears the connection down when the group empties — so the
	// field was accepted, stored, and ignored, which is worse than absent.
	Mode    string   `toml:"mode,omitempty"`
	Command string   `toml:"command,omitempty"`
	Args    []string `toml:"args,omitempty"`
	Rows    int      `toml:"rows,omitempty"`
	Cols    int      `toml:"cols,omitempty"`

	// IdleTimeout closes the channel after this long with nothing asked of it.
	//
	// IT APPLIES TO THE EXTENSIONS THE RECONCILER STARTS ON DEMAND — every enabled
	// template that is neither resident nor declares a forward — and is ignored for
	// one that is: a listener has no caller whose absence could mean "idle", so a
	// resident template is up from startup and stays up (issues #183, #227).
	//
	// A string rather than a time.Duration so the file reads the way the flag does
	// ("10m", "90s"). Empty means DefaultIdleTimeout.
	IdleTimeout string `toml:"idle_timeout,omitempty"`

	// Resident keeps this extension up for as long as termcp runs, instead of
	// starting it on demand and reaping it after IdleTimeout.
	//
	// THIS IS A DIFFERENT REASON FROM A FORWARD, and it used to have no way to be
	// said (issue #227). A template that declares a forward is resident because a
	// listener has no "use" moment to start it; a template with no forward can still
	// have a duty nobody calls in for — an MCP server holding a WebSocket so it can
	// push events, a poller writing into a log — and for that one every legal
	// IdleTimeout eventually killed it, silently, with the only workaround being a
	// semantic lie like `idle_timeout = "87600h"`. Resident says what those templates
	// meant: it is started at boot and restarted when it exits.
	Resident bool `toml:"resident,omitempty"`

	// Capability restriction. Both are whitelists; an empty value means
	// unrestricted, so a terminal that wants the full surface simply lists nothing.
	Allow []string `toml:"allow,omitempty"`
	Paths []string `toml:"paths,omitempty"`

	// URL is an HTTP MCP endpoint (streamable HTTP, or SSE when the path ends in
	// /sse). It is the alternative to Command/Args: instead of a program to run,
	// the template names a server to speak to, and the session
	// carries the in-process relay rather than a process. The relay runs inside
	// termcp (see internal/mcpbridge), so this needs no second binary on the
	// target and no PATH lookup — the channel is the pipe, and termcp is both ends
	// of it.
	//
	// Target is still the profile the channel is opened on, so an HTTP MCP server
	// only reachable from a given host is pinned by naming that host.
	URL string `toml:"url,omitempty"`
	// Headers are extra request headers for that endpoint (e.g. a credential the
	// server wants outside the Authorization header). They belong with URL: a
	// template that names a server without saying how to authenticate to it is
	// only half a template.
	Headers map[string]string `toml:"headers,omitempty"`

	// TermcpTool names an MCP tool of this extension's server that speaks TERM/1,
	// which makes the extension a downstream this instance can route to instead of
	// a peer it can only call (#274). Empty means "this server does not speak
	// TERM/1", which is the default and today's behaviour.
	//
	// It is a NAME, not a guess. The protocol's boundary is a declaration by the
	// downstream, never an inference from arguments that happen to look like a
	// command line: a server whose `recall` tool takes a natural-language query
	// would be silently promoted to a router, and every call through it would
	// mean something else.
	//
	// Only meaningful with URL, because a stdio command's tools are reached the
	// same way and the declaration would be ambiguous about which layer answers.
	TermcpTool string `toml:"termcp_tool,omitempty"`

	// Notes are the agent's operating instructions — the recipe the reconciler
	// hands out with the session handle. For an extension this is the product
	// documentation an agent can actually act on.
	Notes string `toml:"notes,omitempty"`

	// Forwards are declared tunnels, set up when the session comes up.
	Forwards []ForwardDecl `toml:"forward,omitempty"`
}

// Fingerprint summarizes the fields this template runs from, as a comparable
// string. It is the reconciler's answer to "is this a different template", and it
// is exported because the MCP layer needs the same answer for a different
// question: whether the peer it mounted was dialled from the template the file
// describes NOW (issue #282). Two callers, one definition — a peer that decided
// for itself which fields matter would drift from the instance the reconciler
// rebuilds, which is exactly the disagreement #282 was.
func (e *Extension) Fingerprint() string { return fingerprint(e) }

// IsEnabled reports whether the reconciler should push this extension toward
// available. Absence of the key means yes.
func (e *Extension) IsEnabled() bool {
	if e.Enabled == nil {
		return true
	}
	return *e.Enabled
}

// SetEnabled stores the flag in a form that round-trips through TOML.
func (e *Extension) SetEnabled(v bool) {
	e.Enabled = &v
}

// ForwardDecl is one port-forward the reconciler establishes on the extension's
// session. Field meanings follow the forward MCP tool's arguments exactly, which
// is why the listening address has two names: the side doing the listening is the
// target for -R and this host for -L/-D, and one field cannot say both.
type ForwardDecl struct {
	Kind string `toml:"kind"`
	// BindHost is what -L and -D listen on in this host; empty means loopback.
	BindHost string `toml:"bind_host,omitempty"`
	// LocalHost is what -R listens on in the TARGET host.
	LocalHost  string `toml:"local_host,omitempty"`
	LocalPort  int    `toml:"local_port,omitempty"`  // 0 = picked at runtime
	RemoteHost string `toml:"remote_host,omitempty"` // local: where the target dials
	RemotePort int    `toml:"remote_port,omitempty"`
}

// TargetDecl defines one host binding under an extension. Any host-specific
// configuration that would otherwise conflict across targets (such as local
// forward listen ports) hangs directly off this host entry.
type TargetDecl struct {
	Host     string        `toml:"host" json:"host"`
	Forwards []ForwardDecl `toml:"forward,omitempty" json:"forwards,omitempty"`
}

// EffectiveTargets returns the target bindings of the extension. If Targets is
// non-empty, it is returned; otherwise, a legacy Target string is wrapped.
func (e *Extension) EffectiveTargets() []TargetDecl {
	if len(e.Targets) > 0 {
		return e.Targets
	}
	if e.Target != "" {
		return []TargetDecl{{Host: e.Target, Forwards: e.Forwards}}
	}
	return nil
}

// TargetFor finds the matching TargetDecl by host name, or returns the first
// target if host is empty and exactly one target exists.
func (e *Extension) TargetFor(host string) (TargetDecl, bool) {
	targets := e.EffectiveTargets()
	if len(targets) == 0 {
		return TargetDecl{}, false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		if len(targets) == 1 {
			return targets[0], true
		}
		return TargetDecl{}, false
	}
	for _, t := range targets {
		if t.Host == host {
			return t, true
		}
	}
	return TargetDecl{}, false
}

// Lazy reports whether the reconciler waits for a caller before bringing this
// extension up. It does not when the template is resident, and it does not when it
// declares a forward: a tunnel is a service with no "use" moment to start it, so it
// is established at startup and kept (issue #183). `resident = true` is the same
// promise for a template whose duty is not a listener (issue #227).
func (e *Extension) Lazy() bool {
	if e.Resident {
		return false
	}
	targets := e.EffectiveTargets()
	if len(targets) == 0 {
		return len(e.Forwards) == 0
	}
	for _, t := range targets {
		if len(t.Forwards) > 0 {
			return false
		}
	}
	return true
}

// LazyForTarget reports whether this host entry has no resident forward.
func (t TargetDecl) Lazy() bool { return len(t.Forwards) == 0 }

// IdleTimeoutDuration is the configured idle window, or the default when the key
// is absent. An unparsable or non-positive value is an error rather than a
// silent fallback: a caller that wrote "10 minutes" meant something, and running
// with ten minutes instead of what it wrote is the kind of help that hides a
// typo.
func (e *Extension) IdleTimeoutDuration() (time.Duration, error) {
	raw := strings.TrimSpace(e.IdleTimeout)
	if raw == "" {
		return DefaultIdleTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("idle_timeout is not a duration (%q): write it as a Go duration, such as 10m or 90s", e.IdleTimeout)
	}
	// ZERO MEANS "NO IDLE COLLECTION", the same word the daemon's own
	// --idle-timeout uses (issue #227). An extension that carries a background duty
	// — an MCP server holding a WebSocket so it can push events — is not idle when
	// nobody calls a tool, and it had no way to say so: every legal window eventually
	// reaped it, and the only workaround was a semantic lie like "87600h".
	if d < 0 {
		return 0, fmt.Errorf("idle_timeout cannot be negative, got %q; write 0 to disable idle collection", e.IdleTimeout)
	}
	return d, nil
}

// Normalize trims whitespace and fills the defaults an author would otherwise
// have to restate. It is idempotent, so calling it on a loaded extension is
// safe and keeps hand-edited files tidy after one save.
func (e *Extension) Normalize() {
	e.Name = strings.TrimSpace(e.Name)
	e.IdleTimeout = strings.TrimSpace(e.IdleTimeout)
	e.Target = strings.TrimSpace(e.Target)
	e.Mode = strings.TrimSpace(e.Mode)
	e.Command = strings.TrimSpace(e.Command)
	e.URL = strings.TrimSpace(e.URL)
	e.TermcpTool = strings.TrimSpace(e.TermcpTool)
	// An empty headers map is the same as none, and a header name is an HTTP
	// token: a blank one is dropped rather than sent as a request line nobody can
	// parse.
	for name, value := range e.Headers {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			delete(e.Headers, name)
			continue
		}
		if trimmed != name {
			delete(e.Headers, name)
			e.Headers[trimmed] = value
		}
	}
	if len(e.Headers) == 0 {
		e.Headers = nil
	}
	for i := range e.Args {
		e.Args[i] = strings.TrimSpace(e.Args[i])
	}
	for i := range e.Forwards {
		e.Forwards[i].Normalize()
	}
	for i := range e.Targets {
		e.Targets[i].Host = strings.TrimSpace(e.Targets[i].Host)
		for j := range e.Targets[i].Forwards {
			e.Targets[i].Forwards[j].Normalize()
		}
	}
}

// Normalize fills a forward's conventional defaults.
func (f *ForwardDecl) Normalize() {
	f.Kind = strings.TrimSpace(strings.ToLower(f.Kind))
	f.BindHost = strings.TrimSpace(f.BindHost)
	f.LocalHost = strings.TrimSpace(f.LocalHost)
	f.RemoteHost = strings.TrimSpace(f.RemoteHost)
	// -L reaches a host as seen from the target, where localhost is the usual
	// intent; -R names its own target explicitly, so it gets no default.
	if f.Kind == ForwardLocal && f.RemoteHost == "" {
		f.RemoteHost = "localhost"
	}
	if f.Kind == ForwardRemote && f.LocalHost == "" {
		// LOOPBACK, LIKE EVERY OTHER DEFAULT HERE. The port is opened on the TARGET,
		// and 0.0.0.0 there publishes an authenticated tunnel to that machine's whole
		// network — which is a decision, not a default. `ssh -R` made the same move for
		// the same reason; a caller that means to publish says so with --local-host.
		f.LocalHost = "127.0.0.1"
	}
	// A template that wrote local_host on a -L/-D declaration meant the address
	// this host listens on -- the reading the field now has under its own name.
	// Those declarations were accepted and ignored before, so honouring the
	// intent here costs nothing and fixes them rather than keeping them broken.
	if (f.Kind == ForwardLocal || f.Kind == ForwardDynamic) && f.BindHost == "" {
		f.BindHost = f.LocalHost
	}
}

// Validate checks the template so a broken extension is refused at save time
// rather than failing every time the reconciler tries to bring it up. It
// assumes Normalize has run.
func (e *Extension) Validate() error {
	if err := sshconfig.ValidateName(e.Name); err != nil {
		return fmt.Errorf("invalid extension name: %w", err)
	}
	if e.Target == "" && len(e.Targets) == 0 {
		return fmt.Errorf("target is required (an ssh_config profile name or \"internal\"), or a [[targets]] list naming the hosts this extension runs on")
	}
	// ONE SPELLING, NOT TWO. A file carrying both `target` and `[[targets]]` would
	// leave "which hosts does this run on" with two answers, and the runtime would
	// have to pick one silently.
	if e.Target != "" && len(e.Targets) > 0 {
		return fmt.Errorf("target and [[targets]] are alternatives: target names one host, [[targets]] names several. Move %q into the list, or drop the list", e.Target)
	}
	if len(e.Targets) > 0 {
		seen := make(map[string]bool, len(e.Targets))
		for i, t := range e.Targets {
			if t.Host == "" {
				return fmt.Errorf("targets #%d has no host: every entry needs host = \"<ssh_config profile>\"", i+1)
			}
			if err := sshconfig.ValidateName(t.Host); err != nil {
				return fmt.Errorf("targets #%d host: %w", i+1, err)
			}
			// A HOST APPEARS ONCE. Two entries for one machine are two channels on one
			// connection with no way to tell them apart, and the second one's forwards
			// would collide with the first's.
			if seen[t.Host] {
				return fmt.Errorf("targets #%d repeats host %q: each host appears once", i+1, t.Host)
			}
			seen[t.Host] = true
			for j := range t.Forwards {
				if err := t.Forwards[j].Validate(); err != nil {
					return fmt.Errorf("targets #%d (%s) forward #%d: %w", i+1, t.Host, j+1, err)
				}
			}
		}
		// The legacy top-level list belongs to the legacy top-level target; with a
		// [[targets]] list there is no single host for it to hang off.
		if len(e.Forwards) > 0 {
			return fmt.Errorf("forwards belong to a host: move [[forward]] under the [[targets]] entry it listens for, since two targets cannot both bind the same local port")
		}
	}
	switch e.Mode {
	case "", "pty", "pipe":
	default:
		return fmt.Errorf("mode must be pty, pipe or empty (no shell), got %q", e.Mode)
	}
	// A pipe has no login shell to fall back to, so a commandless pipe would
	// start a channel that can do nothing — refuse it here rather than at dial.
	// An HTTP endpoint is the one exception: there the relay IS the command, and
	// the template names a server instead of a program.
	if e.Mode == "pipe" && e.Command == "" && e.URL == "" {
		return fmt.Errorf("mode=pipe requires a command (a pipe channel has no fallback shell)")
	}
	// URL AND COMMAND STILL EXCLUDE EACH OTHER, and the reason is mechanical rather
	// than conceptual: a template gets ONE channel, and both fields claim it. With
	// `url` what runs on the channel is termcp's bridge (resolve() replaces cmd/args
	// with bridgeCommand), so a `command` here would be saved and silently never run.
	//
	// "Start the server, then connect to it" IS a real need — an HTTP MCP server that
	// termcp itself brings up — but it needs TWO channels (one daemon, one bridge),
	// which the template cannot express yet. Refusing is what keeps that gap visible;
	// accepting would ship a field that does nothing.
	// A DECLARATION NEEDS A PEER TO DECLARE ABOUT. termcp_tool names a tool of a
	// mounted MCP server; with no url there is no such server, and the field would
	// be a promise nothing can keep. Refusing at save time is the same choice the
	// url/command pair makes above: a field that silently does nothing is worse
	// than one that says why it cannot be used.
	if e.TermcpTool != "" && e.URL == "" {
		return fmt.Errorf("termcp_tool names a tool of the extension's MCP server, so it needs url: without one there is no server to name a tool of. " +
			"Set url to the server's endpoint, or drop termcp_tool")
	}
	if e.URL != "" && e.Command != "" {
		return fmt.Errorf("url and command are alternatives on one channel: with url, the channel runs termcp's bridge, so a command here would never execute. " +
			"Split it in two for now — one extension runs the server, another takes its url — or use a stdio command that speaks MCP itself")
	}
	if e.URL != "" {
		if err := validEndpointURL(e.URL); err != nil {
			return err
		}
		// THE TARGET OF A url EXTENSION IS THE MACHINE THE CONNECTION LEAVES FROM (#233).
		//
		// When target = "internal", requests leave from this process's own network, so
		// `http://127.0.0.1:8080` reaches this host's loopback.
		//
		// When target names a remote host (e.g. `target = "prod-box"`), requests leave
		// from THAT host's network: the in-process bridge dials inside that host's SSH
		// connection. A URL naming 127.0.0.1 then reaches the remote host's loopback,
		// and private network addresses (10.x, 192.168.x) are resolved from its side.
		// No port forward is needed on this machine, no listening port is bound, and
		// two extensions reaching the same remote service cannot collide.
		//
		// The relay speaks HTTP; there is no channel for a pty to own.
		if e.Mode != "" && e.Mode != "pipe" {
			return fmt.Errorf("url requires mode=pipe (the bridge speaks a line protocol, not a terminal)")
		}
	}
	for _, b := range [2]int{e.Rows, e.Cols} {
		if b < 0 || b > 1000 {
			return fmt.Errorf("rows and cols must be 0 (default) or 1-1000")
		}
	}
	// AN EXTENSION MUST EXPLICITLY DECLARE AT LEAST ONE CAPABILITY.
	// An author must declare what the extension actually does (a command, an mcp url,
	// exposed file paths, port forwards, or an interactive pty shell). Declaring none
	// of these is an empty container.
	hasCapability := e.Command != "" || e.URL != "" || len(e.Paths) > 0 || len(e.Forwards) > 0 || e.Mode == "pty"
	for _, t := range e.Targets {
		if len(t.Forwards) > 0 {
			hasCapability = true
		}
	}
	if !hasCapability {
		return fmt.Errorf("extension declares no capability: explicitly specify at least one of command, url, paths, forward, or mode=pty")
	}
	for i := range e.Forwards {
		if err := e.Forwards[i].Validate(); err != nil {
			return fmt.Errorf("forward #%d: %w", i+1, err)
		}
	}
	if e.Resident && strings.TrimSpace(e.IdleTimeout) != "" {
		return fmt.Errorf("resident = true and idle_timeout are alternatives: a resident extension is up from startup and stays up, so an idle collection window has nothing to apply to (remove idle_timeout or drop resident = true)")
	}
	if _, err := e.IdleTimeoutDuration(); err != nil {
		return err
	}
	// Compile the restriction now so a bad pattern is reported to whoever saved
	// the extension, not to an agent that reaches for a file later.
	if _, err := session.NewRestriction(e.Allow, e.Paths); err != nil {
		return err
	}
	return nil
}

// Validate checks a declared forward against the same rules the forward tool
// enforces, so a tunnel that cannot be created is caught at save time.
func (f *ForwardDecl) Validate() error {
	switch f.Kind {
	case ForwardDynamic:
		// Only a listen port, and even that is optional.
	case ForwardLocal:
		if !validPort(f.RemotePort, 1) {
			return fmt.Errorf("local forward requires remote_port (1-65535)")
		}
	case ForwardRemote:
		if !validPort(f.LocalPort, 1) {
			return fmt.Errorf("remote forward requires local_port (1-65535)")
		}
		if f.RemoteHost == "" || !validPort(f.RemotePort, 1) {
			return fmt.Errorf("remote forward requires remote_host and remote_port")
		}
	default:
		return fmt.Errorf("forward kind must be dynamic, local or remote, got %q", f.Kind)
	}
	// The listen port is the one field every kind carries, and every kind lets it
	// be left at 0 ("pick one"), so its range is checked once, after the kinds.
	if !validPort(f.LocalPort, 0) {
		return fmt.Errorf("local_port out of range (0-65535)")
	}
	// A SOCKS5 proxy this server runs performs no authentication, so a template
	// asking for one on a public interface would publish an open proxy. Refuse it
	// at save time, where the author can read the reason, rather than at every
	// reconcile -- and point at the forward that does publish one service.
	if f.Kind == ForwardDynamic && !forward.IsLoopbackHost(f.BindHost) {
		return fmt.Errorf("a dynamic (SOCKS5) forward binds loopback only: it performs no authentication, so %q would be an open proxy (use kind = \"local\" to publish one service)", forward.ResolveBindHost(f.BindHost))
	}
	return nil
}

// validPort reports whether p is a usable port number, with 0 allowed only when
// lo is 0 — the difference between a port a tunnel cannot do without and one it
// may leave for the runtime to pick.
func validPort(p, lo int) bool {
	return p >= lo && p <= 65535
}

// validEndpointURL checks an HTTP MCP endpoint the bridge will be pointed at. It
// is a shape check, not a reachability one: what it refuses is a value that
// cannot be a URL at all, so the author learns at save time instead of from a
// failure three walks later.
func validEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q names no host", raw)
	}
	return nil
}

// Parse decodes TOML bytes into an Extension without validating. Callers that
// round-trip a file through an editor use this; Save validates separately.
func Parse(data []byte) (*Extension, error) {
	if len(data) > MaxConfigSize {
		return nil, fmt.Errorf("extension config too large (max %d bytes)", MaxConfigSize)
	}
	var e Extension
	if err := toml.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("extension toml: %w", err)
	}
	return &e, nil
}
