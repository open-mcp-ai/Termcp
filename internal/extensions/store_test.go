package extensions

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if s == nil {
		t.Fatal("NewStore returned nil")
	}
	return s
}

// TestStoreSaveLoadRoundTrip pins that a template survives a write and read
// unchanged in the fields an author set — including the ones whose zero value
// is meaningful (enabled=false must not collapse to the default true).
func TestStoreSaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ext := &Extension{
		Name:    "fs-mac",
		Target:  "mac",
		Mode:    "pipe",
		Command: "npx",
		Args:    []string{"-y", "example-mcp-server"},
		Allow:   []string{"shell_input", "shell_output"},
		Notes:   "drive it",
	}
	ext.SetEnabled(false)
	if err := s.Save(ext); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load("fs-mac")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Target != "mac" || got.Mode != "pipe" || got.Command != "npx" {
		t.Errorf("template fields lost: %+v", got)
	}
	if got.Notes != "drive it" {
		t.Errorf("Notes = %q", got.Notes)
	}
	if got.IsEnabled() {
		t.Error("enabled=false must round-trip as disabled, not fall back to the default")
	}
	if len(got.Args) != 2 || got.Args[0] != "-y" || got.Args[1] != "example-mcp-server" {
		t.Errorf("Args = %v", got.Args)
	}
}

// TestStoreSaveRejectsInvalid pins that a template that cannot work is refused
// at save time, so the reconciler never loops on a configuration error.
func TestStoreSaveRejectsInvalid(t *testing.T) {
	s := newTestStore(t)
	tests := []struct {
		name string
		ext  Extension
	}{
		{"missing target", Extension{Name: "a"}},
		{"bad name", Extension{Name: "../escape", Target: "mac"}},
		{"pipe without command", Extension{Name: "a", Target: "mac", Mode: "pipe"}},
		{"bad mode", Extension{Name: "a", Target: "mac", Mode: "serial"}},
		{"bad forward kind", Extension{Name: "a", Target: "mac", Forwards: []ForwardDecl{{Kind: "sideways"}}}},
		{"local forward without remote port", Extension{Name: "a", Target: "mac", Forwards: []ForwardDecl{{Kind: ForwardLocal}}}},
		{"remote forward without local port", Extension{Name: "a", Target: "mac", Forwards: []ForwardDecl{{Kind: ForwardRemote, RemoteHost: "h", RemotePort: 22}}}},
		{"bad path pattern", Extension{Name: "a", Target: "mac", Paths: []string{"re:unclosed["}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := tt.ext
			if err := s.Save(&e); err == nil {
				t.Fatal("Save should have rejected the template")
			}
		})
	}
}

// TestStoreForwardNormalization pins the conventional defaults: a local forward
// reaches localhost on the target, a remote forward binds all interfaces. A single
// template may mix all three forward kinds.
func TestStoreForwardNormalization(t *testing.T) {
	s := newTestStore(t)
	ext := &Extension{
		Name:     "tunnel",
		Target:   "mac",
		Mode:     "",
		Forwards: []ForwardDecl{{Kind: ForwardLocal, RemotePort: 9222}, {Kind: ForwardRemote, LocalPort: 8080, RemoteHost: "h", RemotePort: 80}},
	}
	if err := s.Save(ext); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("tunnel")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Forwards[0].RemoteHost != "localhost" {
		t.Errorf("local forward default RemoteHost = %q, want localhost", got.Forwards[0].RemoteHost)
	}
	// LOOPBACK IS THE DEFAULT FOR -R TOO: the port is opened on the TARGET, and
	// publishing an authenticated tunnel to that machine's network is a decision the
	// caller makes with --local-host, not something a missing field decides.
	if got.Forwards[1].LocalHost != "127.0.0.1" {
		t.Errorf("remote forward default LocalHost = %q, want 127.0.0.1", got.Forwards[1].LocalHost)
	}

	// A stdio MCP server plus a tunnel in ONE template: this is the shape #235 says
	// the help text never mentions, and it must stay supported rather than merely
	// happening.
	mixed := Extension{Name: "mixed", Target: "mac", Mode: "pipe", Command: "example-mcp-server", Forwards: []ForwardDecl{
		{Kind: ForwardLocal, LocalPort: 8443, RemoteHost: "127.0.0.1", RemotePort: 443},
		{Kind: ForwardRemote, LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 80},
		{Kind: ForwardDynamic, LocalPort: 1080},
	}}
	if err := mixed.Validate(); err != nil {
		t.Fatalf("an MCP server with mixed forward kinds must validate: %v", err)
	}
	if mixed.Lazy() {
		t.Error("a template with forwards must be resident, not lazy")
	}
	mixed.Normalize()
	if mixed.Forwards[0].RemoteHost != "127.0.0.1" || mixed.Forwards[1].LocalHost != "127.0.0.1" {
		t.Errorf("mixed forward defaults not normalized: %+v", mixed.Forwards)
	}
}

// TestStoreLoadMissing pins the sentinel: a name with no file is ErrNotFound,
// and errors.Is works so callers can branch without string matching.
func TestStoreLoadMissing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Load("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(missing) = %v, want ErrNotFound", err)
	}
}

// TestStoreListAndDelete pins the directory contract the reconciler relies on:
// List names exactly what is on disk, and Delete leaves nothing behind.
func TestStoreListAndDelete(t *testing.T) {
	s := newTestStore(t)
	names, err := s.List()
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("empty store listed %v", names)
	}

	// A template must declare at least one capability (see Validate): this test is
	// about directory ordering, so each one carries the smallest real declaration.
	for _, n := range []string{"b-ext", "a-ext"} {
		if err := s.Save(&Extension{Name: n, Target: "mac", Mode: "pty"}); err != nil {
			t.Fatalf("Save(%s): %v", n, err)
		}
	}
	names, err = s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 || names[0] != "a-ext" || names[1] != "b-ext" {
		t.Errorf("List = %v, want sorted [a-ext b-ext]", names)
	}

	if err := s.Delete("a-ext"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.root(), "a-ext")); !os.IsNotExist(err) {
		t.Errorf("Delete left the directory behind: %v", err)
	}
	// Deleting a name that is already gone is not an error.
	if err := s.Delete("a-ext"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
}

// TestStoreRejectsPathEscape pins that a name cannot walk out of the extensions
// directory, since the name becomes a filesystem path.
func TestStoreRejectsPathEscape(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"../escape", "sub/dir", ".."} {
		if err := s.Save(&Extension{Name: name, Target: "mac"}); err == nil {
			t.Errorf("Save(%q) should have been rejected", name)
		}
	}
}

// TestApplyDefaultsPreservesAuthorChoices pins the merge rule: unset fields
// take the preset, and set fields are left alone even when the value is one the
// preset would have supplied.
func TestApplyDefaultsPreservesAuthorChoices(t *testing.T) {
	e := &Extension{Target: "mac", Command: "node", Notes: ""}
	ApplyDefaults(e)
	if e.Mode != "pipe" {
		t.Errorf("unset Mode should take the preset, got %q", e.Mode)
	}
	if e.Command != "node" {
		t.Errorf("author's Command should win, got %q", e.Command)
	}
	// An author's command must not arrive with a preset's arguments attached. This
	// used to assert the opposite — `Command: "node"` and then `Args[0] == "-y"` —
	// which is the same bug the preset caused at runtime, written down as the
	// expected behaviour, which is why it survived.
	if len(e.Args) != 0 {
		t.Errorf("an author's Command must not inherit preset args, got %v", e.Args)
	}
	if e.Notes != mcpNotes {
		t.Error("unset Notes should take the preset recipe")
	}

	// A deliberately cleared Notes must survive ApplyDefaults, so an author can
	// ship an unannotated terminal. ApplyDefaults runs at create time only, so
	// this asserts it does not refill what it was handed.
	e2 := &Extension{Target: "mac", Notes: "use it well"}
	ApplyDefaults(e2)
	if e2.Notes != "use it well" {
		t.Errorf("author's Notes should win, got %q", e2.Notes)
	}
}
