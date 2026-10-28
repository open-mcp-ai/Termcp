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
	"github.com/google/uuid"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Extension kinds are presets, not code paths: choosing one fills in a set of
// defaults (see preset.go), and every field stays editable afterwards. An
// extension always remains one shape — a trimmed session — whatever it is
// called; the kind only says which common shape it started from.
const (
	KindMCP    = "mcp"    // an MCP server driven over a pipe shell (stdio command, or an HTTP endpoint bridged in-process)
	KindSkill  = "skill"  // a read-only window onto remote files via SFTP
	KindPrompt = "prompt" // an interactive terminal with instructions attached
	KindCustom = "custom" // everything by hand
)

// validKinds is the set Validate accepts. An empty kind is saved as custom.
var validKinds = map[string]bool{
	KindMCP:    true,
	KindSkill:  true,
	KindPrompt: true,
	KindCustom: true,
}

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
	Name           string `toml:"-"`
	Kind           string `toml:"kind"`
	Target         string `toml:"target"` // an ssh_config profile name, or "internal"
	TargetIdentity string `toml:"target_identity,omitempty"`
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
	// template that declares no forward — and is ignored for one that does: a
	// listener has no caller whose absence could mean "idle", so a template with a
	// tunnel is up from startup and stays up (issue #183).
	//
	// A string rather than a time.Duration so the file reads the way the flag does
	// ("10m", "90s"). Empty means DefaultIdleTimeout.
	IdleTimeout string `toml:"idle_timeout,omitempty"`

	// Capability restriction. Both are whitelists; an empty value means
	// unrestricted, so a prompt-kind extension that wants the full terminal
	// surface simply lists nothing.
	Allow []string `toml:"allow,omitempty"`
	Paths []string `toml:"paths,omitempty"`

	// URL is an HTTP MCP endpoint (streamable HTTP, or SSE when the path ends in
	// /sse). It is the alternative to Command/Args for kind=mcp: instead of a
	// program to run, the template names a server to speak to, and the session
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

	// Notes are the agent's operating instructions — the recipe the reconciler
	// hands out with the session handle. For an extension this is the product
	// documentation an agent can actually act on.
	Notes string `toml:"notes,omitempty"`

	// Forwards are declared tunnels, set up when the session comes up.
	Forwards []ForwardDecl `toml:"forward,omitempty"`
}

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

// Lazy reports whether the reconciler waits for a caller before bringing this
// extension up. It does, unless the template declares a forward: a tunnel is a
// service with no "use" moment to start it, so it is established at startup and
// kept (issue #183).
func (e *Extension) Lazy() bool { return len(e.Forwards) == 0 }

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
	if d <= 0 {
		return 0, fmt.Errorf("idle_timeout must be positive, got %q", e.IdleTimeout)
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
	e.Kind = strings.TrimSpace(e.Kind)
	e.Mode = strings.TrimSpace(e.Mode)
	e.Command = strings.TrimSpace(e.Command)
	if e.Kind == "" {
		e.Kind = KindCustom
	}
	e.URL = strings.TrimSpace(e.URL)
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
	if e.Target == "" {
		return fmt.Errorf("target is required (an ssh_config profile name or \"internal\")")
	}
	if e.TargetIdentity != "" {
		id, err := uuid.Parse(e.TargetIdentity)
		if err != nil || id == uuid.Nil || id.String() != e.TargetIdentity {
			return fmt.Errorf("target_identity must be a canonical UUID")
		}
	}
	if !validKinds[e.Kind] {
		return fmt.Errorf("unknown extension kind %q (mcp, skill, prompt or custom)", e.Kind)
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
	// URL and Command are two answers to one question — what runs on the channel
	// — so a template carrying both would have to pick one silently.
	if e.URL != "" && e.Command != "" {
		return fmt.Errorf("url and command are alternatives: set one, not both")
	}
	if e.URL != "" {
		if err := validEndpointURL(e.URL); err != nil {
			return err
		}
		// The relay runs inside termcp, so the endpoint is reached from this host
		// and the channel has to be the in-memory one. On a remote target the name
		// would be looked up on a machine that has never heard of it, and the
		// failure would read as a missing command rather than as a template asking
		// the wrong host to do the work.
		if !sshconfig.IsInternalName(e.Target) {
			return fmt.Errorf("url requires target = \"internal\": the relay runs inside termcp, so the endpoint is reached from this host (not from %q)", e.Target)
		}
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
	// A skill with no paths would be a read-only window onto nothing. Any other
	// kind may leave paths empty to stay unrestricted.
	if e.Kind == KindSkill && len(e.Paths) == 0 {
		return fmt.Errorf("a skill extension requires at least one path (the directories it may read)")
	}
	for i := range e.Forwards {
		if err := e.Forwards[i].Validate(); err != nil {
			return fmt.Errorf("forward #%d: %w", i+1, err)
		}
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
