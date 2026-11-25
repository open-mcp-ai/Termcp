package extensions

// Template v2: a list of channels (issue #331).
//
// WHAT THE OLD SHAPE COULD NOT SAY. A template had one command/url/mode triple, so
// one extension meant one process or one endpoint. Every other capability was a
// LIST hanging off that single channel — forwards, exposes, the file window — which
// is why "one endpoint plus a worker" or "an HTTP MCP and a stdio MCP from the same
// card" needed two cards, with two lifecycles, two retry ladders and two places to
// edit when the host changes.
//
// The list is the fix: `[[channel]]` names each process or endpoint, and everything
// else stays where it was.
//
//	[[channel]]
//	name = "api"
//	kind = "http"
//	url  = "http://127.0.0.1:18890/mcp"
//
//	[[channel]]
//	name = "worker"
//	kind = "stdio"
//	command = "node"
//	args = ["worker.mjs"]
//
// TWO SPELLINGS DO NOT COEXIST. Top-level command/url/mode remain the single-channel
// form — every saved template keeps its meaning, and `ext get` prints the same shape
// — but a file carrying both is refused by name rather than merged, because "which
// one is the channel" would otherwise have two answers.

import (
	"fmt"
	"strings"
)

// Channel kinds. Empty means "infer from what the channel carries", which is what
// makes the short form readable: a url is http, a command with mode=pty is a
// terminal, any other command is stdio.
const (
	ChannelHTTP  = "http"
	ChannelStdio = "stdio"
	ChannelPTY   = "pty"
)

// Channel is one process or endpoint a template brings up.
//
// EVERY CHANNEL HAS ITS OWN HOST, and that is the structural half of this change. An
// http channel's connection is the loopback one (what runs on it is termcp's own
// relay), while the machine its requests leave from is `target`; a stdio or pty
// channel's connection is where its process runs. So a template with an http channel
// and a stdio channel on a remote host has TWO connections, one per host — the old
// "one channel, one session" assumption could not express that at all.
type Channel struct {
	// Name addresses the channel: `open ext <name> --channel worker`. Empty in the
	// single-channel form, where there is nothing to disambiguate.
	Name string `toml:"name,omitempty" json:"name"`
	// Kind is http, stdio or pty. Empty is inferred (see inferKind).
	Kind string `toml:"kind,omitempty" json:"kind,omitempty"`
	// Target is the host this channel's process runs on, or — for an http channel —
	// the machine its requests leave from. Empty inherits the extension's target,
	// which is what keeps the single-channel form unchanged.
	Target string `toml:"target,omitempty" json:"target,omitempty"`

	Command string            `toml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `toml:"args,omitempty" json:"args,omitempty"`
	Mode    string            `toml:"mode,omitempty" json:"mode,omitempty"`
	URL     string            `toml:"url,omitempty" json:"url,omitempty"`
	Headers map[string]string `toml:"headers,omitempty" json:"headers,omitempty"`
	Rows    int               `toml:"rows,omitempty" json:"rows,omitempty"`
	Cols    int               `toml:"cols,omitempty" json:"cols,omitempty"`

	// Default marks the channel that answers the questions the surface only asks
	// once: which handle `open ext` hands out with no --channel, which channel
	// `Acquire` picks, and which one a template's forwards hang off. Exactly one may
	// say so; with none, the FIRST declared channel is the default, because order is
	// identity everywhere else in this file format too.
	Default bool `toml:"default,omitempty" json:"default,omitempty"`
}

// Host is the ssh_config profile whose connection carries this channel.
//
// IT IS NOT ALWAYS THE CHANNEL'S TARGET. An http channel is carried by the loopback
// connection: the thing running on it is termcp's own relay, and the target only
// says where that relay's requests are dialled from (#233). Everything else runs
// where it says it runs.
func (c Channel) Host(fallback string) string {
	if c.Kind == ChannelHTTP {
		return "internal"
	}
	if c.Target != "" {
		return c.Target
	}
	return fallback
}

// DialHost is the profile a url channel's requests leave from: the channel's own
// target, or the extension's.
func (c Channel) DialHost(fallback string) string {
	if c.Target != "" {
		return c.Target
	}
	return fallback
}

// Label is what a shell opened for this channel is called in the session listing.
//
// THE DEFAULT CHANNEL KEEPS THE EXTENSION'S OWN NAME, so `ls <session>` reads the way
// it did and a handle taken before this change still names the same process. The
// others are qualified with the channel's name, which is the only thing that tells
// two processes of one template apart.
func (c Channel) Label(extName string, isDefault bool) string {
	if isDefault || c.Name == "" {
		return extName
	}
	return extName + "." + c.Name
}

// inferKind fills Kind from what the channel carries.
func (c *Channel) inferKind() {
	if c.Kind != "" {
		return
	}
	switch {
	case strings.TrimSpace(c.URL) != "":
		c.Kind = ChannelHTTP
	case strings.EqualFold(strings.TrimSpace(c.Mode), "pty"):
		c.Kind = ChannelPTY
	case strings.TrimSpace(c.Command) != "":
		c.Kind = ChannelStdio
	}
}

// Normalize trims a channel and fills its defaults.
func (c *Channel) Normalize(fallbackTarget string, index int) {
	c.Name = strings.TrimSpace(c.Name)
	c.Kind = strings.ToLower(strings.TrimSpace(c.Kind))
	c.Target = strings.TrimSpace(c.Target)
	c.Command = strings.TrimSpace(c.Command)
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	c.URL = strings.TrimSpace(c.URL)
	for i := range c.Args {
		c.Args[i] = strings.TrimSpace(c.Args[i])
	}
	c.inferKind()
	// MODE FOLLOWS KIND WHEN IT IS NOT WRITTEN (issue #331). In v1 `mode` had to be
	// spelled out because it was the only thing that said "there is a shell here"; in
	// v2 the kind already says it, so a stdio channel with no mode is a pipe and a pty
	// channel is a terminal. Writing the mode anyway stays legal, which is what keeps a
	// migrated file byte-identical.
	if c.Mode == "" {
		switch c.Kind {
		case ChannelStdio, ChannelHTTP:
			c.Mode = "pipe"
		case ChannelPTY:
			c.Mode = "pty"
		}
	}
	if c.Name == "" {
		// A NAME IS HOW A CHANNEL IS ADDRESSED, so one is always present: the kind is
		// the word an author would have written anyway, and a numeric suffix keeps a
		// second channel of the same kind from colliding with it.
		c.Name = c.Kind
		if c.Name == "" {
			c.Name = fmt.Sprintf("channel%d", index+1)
		}
	}
}

// Validate refuses a channel that cannot be brought up, at save time, naming the
// channel rather than the line.
func (c *Channel) Validate() error {
	switch c.Kind {
	case ChannelHTTP:
		if c.URL == "" {
			return fmt.Errorf("channel %q is kind = \"http\" but names no url", c.Name)
		}
		if c.Command != "" {
			return fmt.Errorf("channel %q is kind = \"http\" and also names a command: an endpoint is dialled, not run. "+
				"An http channel and a process are two channels, so write the process as a [[channel]] of its own", c.Name)
		}
	case ChannelStdio, ChannelPTY:
		if c.Command == "" {
			return fmt.Errorf("channel %q is kind = %q but names no command", c.Name, c.Kind)
		}
		if c.URL != "" {
			return fmt.Errorf("channel %q is kind = %q and also names a url: a process and an endpoint are two channels", c.Name, c.Kind)
		}
	case "":
		return fmt.Errorf("channel %q carries nothing: it needs a command, a url, or a kind", c.Name)
	default:
		return fmt.Errorf("channel %q has kind = %q; the kinds are http, stdio and pty", c.Name, c.Kind)
	}
	switch c.Mode {
	case "", "pty", "pipe":
	default:
		return fmt.Errorf("channel %q: mode must be pty, pipe or empty, got %q", c.Name, c.Mode)
	}
	if c.Kind == ChannelHTTP && c.Mode != "" && c.Mode != "pipe" {
		// AN ENDPOINT'S CHANNEL IS A PIPE: termcp's relay runs on it, and a pty there
		// would be a terminal wrapped around a JSON-RPC stream — the shape that made
		// v1 require mode = "pipe" beside url.
		return fmt.Errorf("channel %q is an endpoint, so its mode is pipe (termcp's relay runs on the channel); got %q", c.Name, c.Mode)
	}
	if c.Kind == ChannelPTY && c.Mode == "pipe" {
		return fmt.Errorf("channel %q is kind = \"pty\" but mode = \"pipe\": kind already says what the channel is", c.Name)
	}
	if c.Kind == ChannelHTTP {
		if strings.HasPrefix(c.URL, "http://") || strings.HasPrefix(c.URL, "https://") {
			if err := validEndpointURL(c.URL); err != nil {
				return fmt.Errorf("channel %q: %w", c.Name, err)
			}
		}
	}
	if c.Rows < 0 || c.Cols < 0 {
		return fmt.Errorf("channel %q: rows and cols cannot be negative", c.Name)
	}
	return nil
}

// EffectiveChannels is the list of channels this template brings up, normalized.
//
// THREE SHAPES, ONE ANSWER. A template with `[[channel]]` entries gets those; a
// template with the old top-level command/url/mode gets one channel built from them
// (so everything downstream can stop caring which spelling the file used); a
// template with neither — a file window with `paths`, a card that is only forwards —
// gets none, which is how "a connection with no process on it" stays expressible.
func (e *Extension) EffectiveChannels() []Channel {
	if len(e.Channels) > 0 {
		return e.Channels
	}
	if e.Command == "" && e.URL == "" && e.Mode == "" {
		return nil
	}
	ch := Channel{
		Command: e.Command,
		Args:    e.Args,
		Mode:    e.Mode,
		URL:     e.URL,
		Headers: e.Headers,
		Rows:    e.Rows,
		Cols:    e.Cols,
		Default: true,
	}
	ch.inferKind()
	return []Channel{ch}
}

// ChannelNamed finds one channel by name. An empty name means the default channel,
// which is what every caller that predates this feature is asking for.
func (e *Extension) ChannelNamed(name string) (Channel, bool) {
	chs := e.EffectiveChannels()
	if len(chs) == 0 {
		return Channel{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		for _, ch := range chs {
			if ch.Default {
				return ch, true
			}
		}
		return chs[0], true
	}
	for _, ch := range chs {
		if ch.Name == name {
			return ch, true
		}
	}
	return Channel{}, false
}

// ChannelNames lists the channel names in declared order, for a hint that has to
// say what the legal answers are.
func (e *Extension) ChannelNames() []string {
	chs := e.EffectiveChannels()
	out := make([]string, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.Name)
	}
	return out
}

// validateChannels refuses a channel list that cannot be brought up as declared:
// a repeated name, two defaults, or one channel that carries nothing.
//
// THE NAMES ARE THE ADDRESSING, so a repeated one is not cosmetic — `--channel
// worker` would have two answers, and a handle would be a coin flip.
func (e *Extension) validateChannels() error {
	seen := make(map[string]bool, len(e.Channels))
	defaults := 0
	for i := range e.Channels {
		ch := &e.Channels[i]
		ch.Name = strings.TrimSpace(ch.Name)
		if ch.Name == "" {
			ch.Name = ch.Kind
		}
		if seen[ch.Name] {
			return fmt.Errorf("channel name %q appears twice: a name addresses one channel, so it has to be unique", ch.Name)
		}
		seen[ch.Name] = true
		if ch.Default {
			defaults++
		}
		if err := ch.Validate(); err != nil {
			return fmt.Errorf("channel #%d: %w", i+1, err)
		}
	}
	if defaults > 1 {
		return fmt.Errorf("%d channels are marked default = true: one extension has one default channel — the one a bare "+
			"`open ext` and /ext/{name}/stream answer with. Drop the flag from the rest", defaults)
	}
	return nil
}

// pickChannel resolves a name against a list: empty means the default.
func pickChannel(chs []Channel, name string) (Channel, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		for _, ch := range chs {
			if ch.Default {
				return ch, true
			}
		}
		return chs[0], true
	}
	for _, ch := range chs {
		if ch.Name == name {
			return ch, true
		}
	}
	return Channel{}, false
}

// channelNameList renders the legal names for an error or a hint.
func channelNameList(chs []Channel) []string {
	out := make([]string, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.Name)
	}
	return out
}

// chMode is the mode a channel runs with, and "" when it has no channel at all
// (a template that is only a file window or a tunnel set).
func chMode(ch Channel) string {
	if ch.Kind == "" {
		return ""
	}
	return ch.Mode
}

// ChannelStatus is one channel as the reconciler actually runs it.
type ChannelStatus struct {
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`
	Target  string `json:"target,omitempty"`
	Default bool   `json:"default,omitempty"`
	// SessionID is the connection that carries this channel — its own channel's host,
	// which for an http channel is the loopback one and for everything else is where
	// the process runs.
	SessionID string `json:"session_id,omitempty"`
	// ShellID is the handle that drives it; empty when it is declared and not running.
	ShellID string `json:"shell_id,omitempty"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// channelStatuses renders a template's channels against the state that is running.
//
// IT REPORTS EVERY DECLARED CHANNEL, RUNNING OR NOT. A channel that failed is the
// one a reader needs to see, and dropping it would leave the file and the status
// disagreeing about what this extension is.
func (r *Reconciler) channelStatuses(ext *Extension, st *extState) []ChannelStatus {
	chs := ext.EffectiveChannels()
	if len(chs) <= 1 {
		return nil
	}
	runs := map[string]channelRun{}
	for _, run := range st.sideRunsSnapshot() {
		runs[run.name] = run
	}
	out := make([]ChannelStatus, 0, len(chs))
	for _, ch := range chs {
		row := ChannelStatus{
			Name:    ch.Name,
			Kind:    ch.Kind,
			Target:  ch.Host(strings.TrimSpace(ext.Target)),
			Default: ch.Default,
		}
		if run, ok := runs[ch.Name]; ok {
			row.SessionID, row.ShellID = run.sessionID, run.shellID
			if r.liveSession(run.sessionID) != nil && run.shellID != "" {
				row.Status = StatusAvailable
			} else {
				row.Status = StatusUnavailable
				row.Reason = "the channel's shell is gone"
			}
		} else {
			defaultCh, _ := pickChannel(chs, "")
			if defaultCh.Name == ch.Name {
				// The default channel is the instance machinery, so its handle is the
				// extension's own.
				row.SessionID, row.ShellID = st.sessionOf(), st.shellOf()
				if row.ShellID != "" {
					row.Status = StatusAvailable
				}
			}
			if row.Status == "" {
				row.Status = StatusUnavailable
				row.Reason = "not opened yet"
			}
		}
		out = append(out, row)
	}
	return out
}

// NameOfShell reports which extension owns a channel handle, or "" for one that belongs
// to nobody (a caller's own instance, an ordinary session).
//
// IT ASKS THE RUNNING STATE, NOT THE TEMPLATE. The endpoint that fans notifications out
// needs the extension's NAME to find its transport, and the only thing that knows a
// handle belongs to an extension is the reconciler that opened it.
func (r *Reconciler) NameOfShell(shellID string) string {
	if strings.TrimSpace(shellID) == "" {
		return ""
	}
	r.mu.Lock()
	states := make(map[string]*extState, len(r.states))
	for name, st := range r.states {
		states[name] = st
	}
	r.mu.Unlock()
	for _, st := range states {
		if st.ownsShell(shellID) {
			// THE STATE'S OWN NAME, NOT THE MAP KEY: the key is name+target so one
			// extension can hold several hosts, and handing that back would name an
			// extension nothing else has heard of.
			return st.name()
		}
	}
	return ""
}
