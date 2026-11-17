package extensions

// Issue #330: a forward may name the host it is built through.
//
// What is testable here is the DECISION LAYER — which hosts exist, what an ask
// resolves to, what is resident, and which of these combinations is refused. The
// tunnel itself is the forward manager's, and it is exercised by that package's own
// tests; asserting it here would need two live SSH servers to prove something the
// plan already says.

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/forward"
)

// viaTemplate is the shape the issue asks for: one extension that dials an MCP
// endpoint from this machine AND listens on the far host.
func viaTemplate() *Extension {
	return &Extension{
		Name:   "qt",
		Target: "internal",
		URL:    "http://127.0.0.1:18890/mcp",
		Mode:   "pipe",
		Forwards: []ForwardDecl{{
			DirectionKind: ForwardRemote,
			Via:           "tencentcloud",
			LocalPort:     18890,
			LocalHost:     "127.0.0.1",
			RemoteHost:    "127.0.0.1",
			RemotePort:    18890,
		}},
	}
}

// TestViaHostBecomesATarget is change 2 of the issue: nothing else would ever dial
// that host, so it has to appear in the list the reconciler walks.
func TestViaHostBecomesATarget(t *testing.T) {
	ext := viaTemplate()
	targets := ext.EffectiveTargets()
	if len(targets) != 2 {
		t.Fatalf("expected the declared host and the via host, got %d: %+v", len(targets), targets)
	}
	if targets[0].Host != "internal" {
		t.Errorf("the declared host should come first, got %q", targets[0].Host)
	}
	if targets[1].Host != "tencentcloud" {
		t.Fatalf("the via host is not a target: %+v", targets)
	}
	if len(targets[1].Forwards) != 0 {
		t.Errorf("a derived target carries no forwards of its own, got %+v", targets[1].Forwards)
	}
}

// TestViaHostIsNotAnAddressableTarget: `Acquire` resolves against the DECLARED list,
// so `/ext/{name}/stream` and `open ext` cannot become ambiguous because of a tunnel.
func TestViaHostIsNotAnAddressableTarget(t *testing.T) {
	ext := viaTemplate()
	if _, ok := ext.TargetFor("tencentcloud"); ok {
		t.Fatal("a via-derived host was offered as an addressable target")
	}
	if _, ok := ext.TargetFor("internal"); !ok {
		t.Fatal("the declared target stopped resolving")
	}
	// And an empty ask still picks the single declared host.
	if got, ok := ext.TargetFor(""); !ok || got.Host != "internal" {
		t.Fatalf("an empty target should resolve to the declared one, got %+v ok=%v", got, ok)
	}
}

// TestViaDerivedTargetIsNotLazy: its whole reason to exist is a tunnel somebody
// else is waiting for, and there is no caller whose ask could start it.
func TestViaDerivedTargetIsNotLazy(t *testing.T) {
	ext := viaTemplate()
	view := forTarget(ext, "tencentcloud")
	if !view.IsViaDerived() {
		t.Fatal("the derived view did not record that it is derived")
	}
	if view.Lazy() {
		t.Fatal("a host that exists for a tunnel was left lazy: nothing would dial it")
	}

	// The declared host is resident too, because it declares the forward. What
	// matters here is that residency is decided per view, not per template.
	if internalView := forTarget(ext, "internal"); internalView.Lazy() {
		t.Error("the host carrying a forward should not be lazy")
	}
}

// TestViaIsNotTheBridgeCarrier is change 3: the loopback rule for a url template
// exists because the relay runs here, and a `via` host is not here.
func TestViaIsNotTheBridgeCarrier(t *testing.T) {
	ext := viaTemplate()
	if ext.isViaDerivedTarget() {
		t.Fatal("the declared target was mistaken for a derived one")
	}

	// A view of the derived host: its own target is now the via host.
	view := forTarget(ext, "tencentcloud")
	view.Target = "tencentcloud"
	if !view.isViaDerivedTarget() {
		t.Fatal("the derived view does not recognize its own target")
	}
}

// TestViaOnADeclaredTargetIsRefused is the rule the issue asks for: one tunnel, one
// host. Two declarations of the same tunnel would let the runtime pick silently.
func TestViaOnADeclaredTargetIsRefused(t *testing.T) {
	ext := &Extension{
		Name: "multi",
		Targets: []TargetDecl{
			{Host: "a"},
			{Host: "b"},
		},
		Forwards: []ForwardDecl{{
			DirectionKind: ForwardLocal,
			Via:           "b",
			LocalPort:     8443,
			RemotePort:    443,
		}},
	}
	err := ext.Validate()
	if err == nil {
		t.Fatal("a top-level via naming a declared target was accepted")
	}
	if !strings.Contains(err.Error(), `"b"`) || !strings.Contains(err.Error(), "targets #2") {
		t.Errorf("the refusal must name the host and the entry it duplicates, got: %v", err)
	}
}

// TestViaToAnUndeclaredHostIsAllowed is the other half of that rule: a host that is
// NOT a target is exactly what `via` is for.
func TestViaToAnUndeclaredHostIsAllowed(t *testing.T) {
	ext := &Extension{
		Name: "multi",
		Targets: []TargetDecl{
			{Host: "a"},
		},
		Forwards: []ForwardDecl{{
			DirectionKind: ForwardLocal,
			Via:           "elsewhere",
			LocalPort:     8443,
			RemotePort:    443,
		}},
	}
	if err := ext.Validate(); err != nil {
		t.Fatalf("a via to a host that is not a target was refused: %v", err)
	}
	if got := len(ext.EffectiveTargets()); got != 2 {
		t.Fatalf("expected the declared host plus the via host, got %d", got)
	}
}

// TestViaMustBeAProfileName: a typo should fail where the author can read it, not at
// dial time as "no such profile" on a host they never meant to write.
func TestViaMustBeAProfileName(t *testing.T) {
	f := ForwardDecl{DirectionKind: ForwardLocal, Via: "has space", LocalPort: 8443, RemotePort: 443}
	if err := f.Validate(); err == nil {
		t.Fatal("an invalid profile name was accepted as via")
	}
}

// TestViaRoundTrips: the field has to survive a save, or the tunnel silently reverts
// to the extension's own host on the next load — the failure this field exists to
// remove, one layer down.
func TestViaRoundTrips(t *testing.T) {
	ext := viaTemplate()
	doc, err := tomlMarshal(t, ext)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(doc), `via = "tencentcloud"`) {
		t.Fatalf("the field did not reach the document: %s", doc)
	}
	back, err := Parse(doc)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	if len(back.Forwards) != 1 || back.Forwards[0].Via != "tencentcloud" {
		t.Fatalf("the field did not survive the round trip: %+v", back.Forwards)
	}
}

// TestViaDoesNotChangeASingleHostTemplate is acceptance 4: every template that does
// not use the field behaves exactly as before.
func TestViaDoesNotChangeASingleHostTemplate(t *testing.T) {
	ext := &Extension{
		Name:    "plain",
		Target:  "pi",
		Command: "node",
		Mode:    "pipe",
		Forwards: []ForwardDecl{{
			DirectionKind: ForwardLocal,
			LocalPort:     8443,
			RemotePort:    443,
		}},
	}
	if err := ext.Validate(); err != nil {
		t.Fatalf("a plain template was refused: %v", err)
	}
	targets := ext.EffectiveTargets()
	if len(targets) != 1 || targets[0].Host != "pi" {
		t.Fatalf("a template with no via gained a target: %+v", targets)
	}
	if declaredView := forTarget(ext, "pi"); declaredView.IsViaDerived() {
		t.Error("a declared target was marked derived")
	}
}

// tomlMarshal keeps the round-trip test reading like the store's own write path.
func tomlMarshal(t *testing.T, ext *Extension) ([]byte, error) {
	t.Helper()
	return toml.Marshal(ext)
}

// TestForwardViaIsShownOnItsOwnHost is acceptance 6's data half: the tunnel's status
// has to say which host it is on, or an operator reading `ext get` sees a listener
// with no location.
func TestForwardViaIsShownOnItsOwnHost(t *testing.T) {
	// The status type carries the direction and addresses; the host is the state it
	// was recorded under. What is asserted here is that the plan puts the forward on
	// the derived host's view, which is what makes the state keyed correctly.
	ext := viaTemplate()
	view := forTarget(ext, "tencentcloud")
	if view.Target != "tencentcloud" {
		t.Fatalf("the derived view's target is %q", view.Target)
	}
	if len(view.Forwards) != 0 {
		t.Errorf("the derived view should carry no forwards of its own: %+v", view.Forwards)
	}
	// The tunnel lives in the DECLARING host's forward list, and its Via is what says
	// where it is built.
	declared := forTarget(ext, "internal")
	if len(declared.Forwards) != 1 || declared.Forwards[0].Via != "tencentcloud" {
		t.Fatalf("the declaring view lost the forward or its via: %+v", declared.Forwards)
	}
}

var _ = forward.ForwardInfo{}

// TestDerivedTargetCarriesNoChannel is change 4: the process runs on the DECLARED
// host. Copying the template down here would start a second copy on the machine that
// is only meant to listen.
func TestDerivedTargetCarriesNoChannel(t *testing.T) {
	ext := &Extension{
		Name:    "both",
		Target:  "internal",
		Command: "node",
		Args:    []string{"worker.mjs"},
		Mode:    "pipe",
		Forwards: []ForwardDecl{{
			DirectionKind: ForwardRemote,
			Via:           "vps",
			LocalPort:     18890,
			RemoteHost:    "127.0.0.1",
			RemotePort:    18890,
		}},
	}
	view := forTarget(ext, "vps")
	if view.Command != "" || len(view.Args) != 0 || view.Mode != "" {
		t.Fatalf("the derived view inherited a channel: command=%q args=%v mode=%q", view.Command, view.Args, view.Mode)
	}
	if !view.viaDerived {
		t.Fatal("the derived view lost its flag")
	}
	// The declaring host keeps everything.
	declared := forTarget(ext, "internal")
	if declared.Command != "node" || declared.Mode != "pipe" {
		t.Fatalf("the declaring view lost its channel: %+v", declared)
	}
	// AND THE WHITELIST SURVIVES. The derived host's connection is still a session an
	// agent can drive by name; clearing the restriction would turn it into the one
	// place an extension's capabilities do not apply.
	withWhitelist := *ext
	withWhitelist.Allow = []string{"shell_output"}
	withWhitelist.Forwards = ext.Forwards
	if got := forTarget(&withWhitelist, "vps"); len(got.Allow) != 1 {
		t.Fatalf("the derived view dropped the whitelist: %+v", got.Allow)
	}
}
