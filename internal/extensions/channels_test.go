package extensions

// Issue #331: a template may declare more than one channel.
//
// The load-bearing promise here is that ONE STILL MEANS ONE. Every template written
// before the list existed has to behave identically — the same channel, the same
// handle, the same `ext get` fields — because the list is an addition, not a
// migration. Half of these tests are about that and only that.

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestLegacyTemplateIsOneDerivedChannel: command/url/mode read as a single unnamed
// channel, which is what lands them in the same state a single-channel template
// always used.
func TestLegacyTemplateIsOneDerivedChannel(t *testing.T) {
	ext := &Extension{Name: "old", Target: "internal", Command: "node", Args: []string{"w.mjs"}, Mode: "pipe"}
	chans := ext.EffectiveChannels()
	if len(chans) != 1 {
		t.Fatalf("expected one derived channel, got %d", len(chans))
	}
	c := chans[0]
	if c.Command != "node" || len(c.Args) != 1 || c.resolvedKind() != "stdio" {
		t.Fatalf("the derived channel lost its fields: %+v", c)
	}
	if c.Name != "" {
		t.Errorf("a derived channel must be nameless so it files under the old key, got %q", c.Name)
	}
	def, ok := ext.DefaultChannel()
	if !ok || def.Name != "" {
		t.Fatalf("the derived channel should be the default, got %+v ok=%v", def, ok)
	}

	// A url template derives an http channel.
	urlExt := &Extension{Name: "u", Target: "internal", URL: "http://127.0.0.1:9/mcp", Mode: "pipe"}
	uc := urlExt.EffectiveChannels()
	if len(uc) != 1 || uc[0].resolvedKind() != "http" {
		t.Fatalf("a url template should derive one http channel: %+v", uc)
	}

	// A template with none of the three declares no channel at all.
	empty := &Extension{Name: "f", Target: "internal", Paths: []string{"/srv/"}}
	if got := empty.EffectiveChannels(); len(got) != 0 {
		t.Fatalf("a forwards-only template gained a channel: %+v", got)
	}
	if _, ok := empty.DefaultChannel(); ok {
		t.Error("a template with no channel reported a default one")
	}
}

// TestDeclaredChannelsResolveByKind: the list is the new shape, and each entry says
// what it is with one word.
func TestDeclaredChannelsResolveByKind(t *testing.T) {
	ext := &Extension{
		Name:   "multi",
		Target: "internal",
		Channels: []ChannelDecl{
			{Name: "mcp-http", Kind: "http", URL: "http://127.0.0.1:18890/mcp"},
			{Name: "worker", Kind: "stdio", Command: "node", Args: []string{"worker.mjs"}},
		},
	}
	chans := ext.EffectiveChannels()
	if len(chans) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(chans))
	}
	if chans[0].resolvedKind() != "http" || chans[1].resolvedKind() != "stdio" {
		t.Fatalf("kinds were not kept: %+v", chans)
	}
	// No default marked: the first one is it.
	def, ok := ext.DefaultChannel()
	if !ok || def.Name != "mcp-http" {
		t.Fatalf("an unmarked list should default to the first, got %+v", def)
	}
	// The http channel is what the MCP route can carry.
	if h, ok := ext.HTTPChannel(); !ok || h.Name != "mcp-http" {
		t.Fatalf("HTTPChannel picked %+v ok=%v", h, ok)
	}
}

// TestMarkedDefaultWins: one answer to "which channel does an unqualified ask mean".
func TestMarkedDefaultWins(t *testing.T) {
	ext := &Extension{
		Name:   "multi",
		Target: "internal",
		Channels: []ChannelDecl{
			{Name: "a", Kind: "stdio", Command: "one"},
			{Name: "b", Kind: "stdio", Command: "two", Default: true},
		},
	}
	if def, ok := ext.DefaultChannel(); !ok || def.Name != "b" {
		t.Fatalf("the marked default was not chosen: %+v", def)
	}
	if got, ok := ext.ChannelFor("a"); !ok || got.Name != "a" {
		t.Fatalf("ChannelFor did not find a declared name: %+v", got)
	}
	if _, ok := ext.ChannelFor("nope"); ok {
		t.Fatal("ChannelFor invented a channel")
	}
	if def, ok := ext.ChannelFor(""); !ok || def.Name != "b" {
		t.Fatalf("an empty name should mean the default: %+v", def)
	}
}

// TestChannelListAndLegacyFieldsAreAlternatives: two answers to "what runs" is the
// same defect the target/[[targets]] pair already refuses.
func TestChannelListAndLegacyFieldsAreAlternatives(t *testing.T) {
	ext := &Extension{
		Name:     "both",
		Target:   "internal",
		Command:  "node",
		Mode:     "pipe",
		Channels: []ChannelDecl{{Name: "a", Kind: "stdio", Command: "node"}},
	}
	err := ext.Validate()
	if err == nil {
		t.Fatal("a template with both a command and a channel list was accepted")
	}
	if !strings.Contains(err.Error(), "[[channel]]") {
		t.Errorf("the refusal should name the list, got: %v", err)
	}
}

// TestChannelNamesAreRequiredAndUnique: two nameless channels are indistinguishable,
// and the name is what `--channel` and a failure report point at.
func TestChannelNamesAreRequiredAndUnique(t *testing.T) {
	base := func(chans ...ChannelDecl) *Extension {
		return &Extension{Name: "x", Target: "internal", Channels: chans}
	}
	if err := base(ChannelDecl{Kind: "stdio", Command: "node"}).Validate(); err == nil {
		t.Error("a channel with no name was accepted")
	}
	if err := base(
		ChannelDecl{Name: "a", Kind: "stdio", Command: "node"},
		ChannelDecl{Name: "a", Kind: "stdio", Command: "node"},
	).Validate(); err == nil {
		t.Error("a repeated channel name was accepted")
	}
	if err := base(
		ChannelDecl{Name: "a", Kind: "stdio", Command: "node", Default: true},
		ChannelDecl{Name: "b", Kind: "stdio", Command: "node", Default: true},
	).Validate(); err == nil {
		t.Error("two channels were both marked default")
	}
}

// TestChannelKindDecidesWhatItNeeds: a stdio channel without a command and an http
// one without a url are both half-written, and both are refused at save time rather
// than at start time.
func TestChannelKindDecidesWhatItNeeds(t *testing.T) {
	cases := []struct {
		name string
		c    ChannelDecl
		want string
	}{
		{"stdio without a command", ChannelDecl{Name: "a", Kind: "stdio"}, "has no command"},
		{"http without a url", ChannelDecl{Name: "a", Kind: "http"}, "has no url"},
		{"http with a command", ChannelDecl{Name: "a", Kind: "http", URL: "http://x/mcp", Command: "node"}, "relay IS its command"},
		{"stdio with a url", ChannelDecl{Name: "a", Kind: "stdio", Command: "node", URL: "http://x"}, "or an endpoint"},
		{"unknown kind", ChannelDecl{Name: "a", Kind: "nonsense", Command: "node"}, "use stdio, pty or http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext := &Extension{Name: "x", Target: "internal", Channels: []ChannelDecl{tc.c}}
			err := ext.Validate()
			if err == nil {
				t.Fatal("an incomplete channel was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should say %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestChannelsRoundTrip: the list has to survive a save, or a two-channel template
// comes back as an empty one and the reconciler stops running anything.
func TestChannelsRoundTrip(t *testing.T) {
	ext := &Extension{
		Name:   "multi",
		Target: "internal",
		Channels: []ChannelDecl{
			{Name: "mcp-http", Kind: "http", URL: "http://127.0.0.1:18890/mcp", Default: true,
				Headers: map[string]string{"Authorization": "Bearer x"}},
			{Name: "worker", Kind: "stdio", Command: "node", Args: []string{"worker.mjs"}},
		},
	}
	doc, err := toml.Marshal(ext)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(doc), "[[channel]]") || !strings.Contains(string(doc), `name = "worker"`) {
		t.Fatalf("the list did not reach the document: %s", doc)
	}
	back, err := Parse(doc)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	if len(back.Channels) != 2 {
		t.Fatalf("the list did not survive the round trip: %+v", back.Channels)
	}
	if back.Channels[0].Headers["Authorization"] != "Bearer x" {
		t.Errorf("a channel's headers were lost: %+v", back.Channels[0].Headers)
	}
	if !back.Channels[0].Default {
		t.Error("the default mark was lost")
	}
}

// TestTwoChannelTemplateIsUsable is acceptance 1: a two-channel template validates.
func TestTwoChannelTemplateIsUsable(t *testing.T) {
	doc := `target = "internal"

[[channel]]
name = "mcp-http"
kind = "http"
url = "http://127.0.0.1:18890/mcp"
default = true

[[channel]]
name = "worker"
kind = "stdio"
command = "node"
args = ["worker.mjs"]
`
	ext, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ext.Name = "two"
	ext.Normalize()
	if err := ext.Validate(); err != nil {
		t.Fatalf("the issue's own two-channel shape was refused: %v", err)
	}
	if def, ok := ext.DefaultChannel(); !ok || def.Name != "mcp-http" {
		t.Fatalf("the marked default was not chosen: %+v", def)
	}
}

// TestFingerprintCoversTheChannelList: an edit that adds a worker has to rebuild,
// exactly as an edit to the command does.
func TestFingerprintCoversTheChannelList(t *testing.T) {
	one := &Extension{Name: "x", Target: "internal",
		Channels: []ChannelDecl{{Name: "a", Kind: "stdio", Command: "node"}}}
	two := &Extension{Name: "x", Target: "internal",
		Channels: []ChannelDecl{
			{Name: "a", Kind: "stdio", Command: "node"},
			{Name: "b", Kind: "stdio", Command: "node"},
		}}
	if fingerprint(one) == fingerprint(two) {
		t.Fatal("adding a channel produced the same fingerprint")
	}
	renamed := &Extension{Name: "x", Target: "internal",
		Channels: []ChannelDecl{{Name: "c", Kind: "stdio", Command: "node"}}}
	if fingerprint(one) == fingerprint(renamed) {
		t.Fatal("renaming a channel produced the same fingerprint")
	}
	// And the same list twice is the same fingerprint: the reconciler rebuilds on a
	// difference, so a rendering that varied per call would rebuild on every walk.
	if fingerprint(one) != fingerprint(one) {
		t.Fatal("the fingerprint is not stable across calls")
	}
}

// TestDerivedAndDeclaredAgreeOnOneChannel: a one-channel [[channel]] list and the
// legacy spelling describe the same instance, which is what makes the compatibility
// claim true rather than plausible.
func TestDerivedAndDeclaredAgreeOnOneChannel(t *testing.T) {
	legacy := &Extension{Name: "x", Target: "internal", Command: "node", Mode: "pipe"}
	declared := &Extension{Name: "x", Target: "internal",
		Channels: []ChannelDecl{{Name: "main", Kind: "stdio", Command: "node", Default: true}}}
	l := legacy.EffectiveChannels()
	d := declared.EffectiveChannels()
	if len(l) != 1 || len(d) != 1 {
		t.Fatalf("expected one channel each, got %d and %d", len(l), len(d))
	}
	if l[0].resolvedKind() != d[0].resolvedKind() || l[0].Command != d[0].Command {
		t.Fatalf("the two spellings disagree: %+v vs %+v", l[0], d[0])
	}
}
