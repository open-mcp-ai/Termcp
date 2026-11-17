package extensions

// Issue #332: an unknown key is refused, not dropped.
//
// The failure this pins is not "the key did nothing" — it is that the template which
// came back out of the store was SHORTER than the one the author wrote, with no signal.
// Every assertion below therefore checks two things: that the call fails, and that the
// message names the key, because a refusal that says "invalid" sends the author looking
// at the wrong half of their document.

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestTargetsJSONRefusesUnknownKeys is acceptance 1 and 3 from the JSON side.
func TestTargetsJSONRefusesUnknownKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets any
		wantKey string
	}{
		{
			name:    "a process on a target",
			targets: []any{map[string]any{"host": "a", "command": "node"}},
			wantKey: `"command"`,
		},
		{
			name:    "mode on a target",
			targets: []any{map[string]any{"host": "a", "mode": "pipe"}},
			wantKey: `"mode"`,
		},
		{
			name:    "resident on a target",
			targets: []any{map[string]any{"host": "a", "resident": true}},
			wantKey: `"resident"`,
		},
		{
			name:    "a typo",
			targets: []any{map[string]any{"host": "a", "comand": "node"}},
			wantKey: `"comand"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTargetDecls(tc.targets)
			if err == nil {
				t.Fatal("an unknown key was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("the refusal must name %s, got: %v", tc.wantKey, err)
			}
			// The message has to say where the key belongs, or the author's next move
			// is a guess.
			if !strings.Contains(err.Error(), "top level") {
				t.Errorf("the refusal should point at where the key belongs, got: %v", err)
			}
		})
	}
}

// TestTargetsJSONAcceptsEveryDocumentedKey: the strictness must not reject the shapes
// the schema actually declares.
func TestTargetsJSONAcceptsEveryDocumentedKey(t *testing.T) {
	targets := []any{
		"node1",
		map[string]any{"host": "node2"},
		map[string]any{"target": "node3"},
		map[string]any{"host": "node4", "forwards": []any{
			map[string]any{"kind": "local", "local_port": 8443, "remote_port": 443},
		}},
		map[string]any{"host": "node5", "forward": []any{
			map[string]any{"kind": "remote", "local_port": 9000, "remote_host": "127.0.0.1", "remote_port": 9000},
		}},
	}
	got, err := ParseTargetDecls(targets)
	if err != nil {
		t.Fatalf("a documented target shape was refused: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 targets, got %d", len(got))
	}
	if got[3].Forwards[0].LocalPort != 8443 {
		t.Errorf("the forward under node4 lost its port: %+v", got[3].Forwards)
	}
}

// TestTOMLRefusesUnknownKeys is acceptance 2 and 3 from the file side.
//
// The key is under `[[targets]]` in the first case because that is the shape the issue
// reports; the second is the same rule at the top level, which used to be equally
// silent and is the more common typo.
func TestTOMLRefusesUnknownKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		doc     string
		wantKey string
	}{
		{
			name:    "resident under a target",
			doc:     "target = \"a\"\n\n[[targets]]\nhost = \"b\"\nresident = true\n",
			wantKey: `"targets.resident"`,
		},
		{
			name:    "a misspelled top-level key",
			doc:     "target = \"internal\"\ncomand = \"node\"\n",
			wantKey: `"comand"`,
		},
		{
			name:    "a key from a later version",
			doc:     "target = \"internal\"\nchannels = []\n",
			wantKey: `"channels"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatal("an unknown key was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("the refusal must name %s, got: %v", tc.wantKey, err)
			}
			// The distinction the issue asks for: this is an unknown key, not a wrong
			// type, and the message must not leave the reader guessing which.
			if !strings.Contains(err.Error(), "nothing reads") {
				t.Errorf("the refusal should say the key is unread, got: %v", err)
			}
		})
	}
}

// TestTOMLLeavesTypeMistakesToTheDecoder: a wrong type is a different mistake with a
// different fix, and the decoder's own message names both the key and the type. The
// two must not be conflated into one sentence that says neither.
func TestTOMLLeavesTypeMistakesToTheDecoder(t *testing.T) {
	// `enabled` is a real field, so the decoder owns this failure: the key is not
	// unknown, its value is the wrong shape.
	_, err := Parse([]byte("target = \"internal\"\nenabled = \"yes\"\n"))
	if err == nil {
		t.Fatal("a string where a number belongs was accepted")
	}
	if !strings.Contains(err.Error(), "toml") {
		t.Errorf("a decode failure should be framed as a toml problem, got: %v", err)
	}
	if strings.Contains(err.Error(), "nothing reads") {
		t.Errorf("a type mistake must not be reported as an unknown key, got: %v", err)
	}
}

// TestTOMLAcceptsAStoredTemplate is acceptance 4: everything this package writes has to
// keep loading. A strict decoder that rejected its own output would be worse than the
// silent one.
func TestTOMLAcceptsAStoredTemplate(t *testing.T) {
	ext := &Extension{
		Name:    "qt",
		Target:  "internal",
		Command: "node",
		Args:    []string{"worker.mjs"},
		Mode:    "pipe",
		Allow:   []string{"shell_output"},
		Paths:   []string{"/srv/"},
		Forwards: []ForwardDecl{
			{DirectionKind: "local", LocalPort: 8443, RemotePort: 443},
		},
	}
	// The writer this package actually uses: toml.Marshal of the same struct the
	// decoder reads, so a round trip here is the one a save-and-load performs.
	doc, err := toml.Marshal(ext)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := Parse(doc)
	if err != nil {
		t.Fatalf("a template this package wrote was refused on the way back in: %v\n%s", err, doc)
	}
	if back.Command != "node" || len(back.Args) != 1 || back.Mode != "pipe" {
		t.Errorf("the round trip lost fields: %+v", back)
	}
	if len(back.Forwards) != 1 || back.Forwards[0].LocalPort != 8443 {
		t.Errorf("the round trip lost a forward: %+v", back.Forwards)
	}
}

// TestTOMLAcceptsAMultiTargetTemplate covers the shape the issue's own example uses.
func TestTOMLAcceptsAMultiTargetTemplate(t *testing.T) {
	doc := `target = "internal"
mode = "pipe"
command = "kubectl-mcp"

[[targets]]
host = "pi"

[[targets.forward]]
kind = "local"
local_port = 8443
remote_port = 443
`
	ext, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("the documented multi-target shape was refused: %v", err)
	}
	if len(ext.Targets) != 1 || ext.Targets[0].Host != "pi" {
		t.Fatalf("targets did not decode: %+v", ext.Targets)
	}
	if len(ext.Targets[0].Forwards) != 1 || ext.Targets[0].Forwards[0].LocalPort != 8443 {
		t.Fatalf("the nested forward did not decode: %+v", ext.Targets[0].Forwards)
	}
}
