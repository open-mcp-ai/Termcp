package extensions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A template with holes and no values is what a package looks like on arrival, so
// it has to parse and be storable before anybody installs anything.
const holeTemplate = `description = "drive the cluster"
mode = "pipe"
command = "{{command}}"
targets = "{{targets}}"

[params.command]
type = "string"
help = "the program that serves MCP over the pipe"

[params.targets]
type = "array"
help = "one entry per host, each with its own tunnels"
`

func TestTemplateOfReadsHolesAndDeclarations(t *testing.T) {
	params, written, err := TemplateOf([]byte(holeTemplate))
	if err != nil {
		t.Fatalf("TemplateOf: %v", err)
	}
	if len(written) != 2 || written[0] != "command" || written[1] != "targets" {
		t.Errorf("written = %v, want [command targets]", written)
	}
	if params["command"].Type != "string" || params["targets"].Type != "array" {
		t.Errorf("types = %v, want string and array", params)
	}
	if params["targets"].Help == "" {
		t.Error("a hole's help line is what tells a filler what to type")
	}

	// params is a declaration, not a template field: leaving it in the rendered
	// document would make Load refuse a key it has no field for.
	rendered, err := Render([]byte(holeTemplate), map[string]any{
		"command": "kubectl-mcp",
		"targets": []any{map[string]any{"host": "pi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "params") {
		t.Errorf("the rendered document still carries the declarations:\n%s", rendered)
	}
}

// A hole that appears in prose is not a hole. The alternative is a note that
// mentions one placeholder turning the whole template into a request.
func TestTemplateOfRefusesAmbiguousHoles(t *testing.T) {
	cases := map[string]string{
		"embedded in a longer string": "command = \"run {{command}} now\"\n\n[params.command]\nhelp = \"x\"\n",
		"undeclared":                  "command = \"{{command}}\"\n",
		"unknown declaration field":   "command = \"{{command}}\"\n\n[params.command]\nrequired = true\n",
		"bad declared type":           "command = \"{{command}}\"\n\n[params.command]\ntype = \"list\"\n",
		"params is not a table":       "command = \"{{command}}\"\nparams = \"{{command}}\"\n",
	}
	for name, doc := range cases {
		if _, _, err := TemplateOf([]byte(doc)); err == nil {
			t.Errorf("%s: the template must be refused, and the error is the only place it can be explained", name)
		}
	}
}

// The store's two files are the whole contract: the template is replaced by an
// upgrade, the values survive it.
func TestStoreTemplateAndValuesAreSeparate(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveTemplate("k8s", []byte(holeTemplate)); err != nil {
		t.Fatalf("storing a package with unfilled holes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir(t, s, "k8s"), LocalFileName)); !os.IsNotExist(err) {
		t.Errorf("a template install must not write a values file it has nothing for: %v", err)
	}

	// Unfilled is not broken: the answer names the holes rather than failing.
	if _, err := s.Load("k8s"); err == nil {
		t.Fatal("Load must refuse a template whose holes are empty rather than render it as an extension with no command")
	}
	missing, err := s.Missing("k8s")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 {
		t.Errorf("missing = %v, want both holes", missing)
	}
	holes, err := s.Holes("k8s")
	if err != nil {
		t.Fatal(err)
	}
	if len(holes) != 2 || holes[0].Type != "string" || holes[1].Type != "array" {
		t.Errorf("holes = %+v, want the declared types to reach the caller", holes)
	}

	if err := s.SetValues("k8s", map[string]any{
		"command": "kubectl-mcp",
		"targets": []any{map[string]any{"host": "pi", "forward": []any{map[string]any{
			"kind": "local", "local_port": float64(8443), "remote_port": float64(443),
		}}}},
	}); err != nil {
		t.Fatalf("filling the holes: %v", err)
	}
	ext, err := s.Load("k8s")
	if err != nil {
		t.Fatalf("the filled template should build: %v", err)
	}
	if ext.Command != "kubectl-mcp" || len(ext.Targets) != 1 || ext.Targets[0].Host != "pi" {
		t.Fatalf("rendered = %+v", ext)
	}
	// A port that arrived over JSON as 8443.0 has to read back as an int, or the
	// same host filled in through the panel and through the tool would not agree.
	if ext.Targets[0].Forwards[0].LocalPort != 8443 {
		t.Errorf("local_port = %d, want 8443", ext.Targets[0].Forwards[0].LocalPort)
	}
	if strings.Contains(string(mustReadFile(t, filepath.Join(storeDir(t, s, "k8s"), LocalFileName))), ".0") {
		t.Error("the values file must not store an integer port as a float")
	}

	// Replacing the template keeps the values. That is the reason there are two files.
	if err := s.SaveTemplate("k8s", []byte(strings.Replace(holeTemplate,
		"the program that serves MCP over the pipe", "the MCP server", 1))); err != nil {
		t.Fatalf("upgrading the template: %v", err)
	}
	if again, err := s.Load("k8s"); err != nil || again.Command != "kubectl-mcp" {
		t.Errorf("a template upgrade must not discard this installation's values: %+v (%v)", again, err)
	}

	// Setting one value merges; it does not say "forget the other".
	if err := s.SetValues("k8s", map[string]any{"command": "new-cmd"}); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Load("k8s"); err != nil || again.Command != "new-cmd" || len(again.Targets) != 1 {
		t.Errorf("one value must not clear the rest: %+v (%v)", again, err)
	}

	// Clearing is explicit and puts the extension back into "needs a value".
	if err := s.SetValues("k8s", map[string]any{"command": nil}); err != nil {
		var notApplied *NotApplied
		if !errors.As(err, &notApplied) {
			t.Fatal(err)
		}
	}
	if missing, err := s.Missing("k8s"); err != nil || len(missing) != 1 || missing[0] != "command" {
		t.Errorf("after clearing, missing = %v (%v), want [command]", missing, err)
	}
}

// A value for a hole nobody asked for is refused: filed away where nothing reads
// it, it would look like a setting that works.
func TestSetValuesRefusesNamesAndTypes(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveTemplate("k8s", []byte(holeTemplate)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetValues("k8s", map[string]any{"hostname": "pi"}); err == nil {
		t.Error("a value that names no hole must be refused")
	} else if !strings.Contains(err.Error(), "command") {
		t.Errorf("the refusal must list what the template does ask for, got %q", err)
	}

	// A type the declaration rules out is refused before it is stored, so the
	// caller's typo is reported by the field that has an opinion about it.
	if err := s.SetValues("k8s", map[string]any{"targets": "pi"}); err == nil {
		t.Error("a string must not be accepted where an array is declared")
	}

	// A value that is stored but does not render is kept and reported as such —
	// it is the only copy of what someone typed.
	if err := s.SetValues("k8s", map[string]any{"command": "kubectl"}); err != nil {
		var notApplied *NotApplied
		if !errors.As(err, &notApplied) {
			t.Fatalf("an incomplete fill should report NotApplied, got %T %v", err, err)
		}
		if !strings.Contains(err.Error(), "targets") {
			t.Errorf("the reason should name the hole still open, got %q", err)
		}
	}
	if values, err := s.Values("k8s"); err != nil || values["command"] != "kubectl" {
		t.Errorf("a value that does not render yet must still be stored, got %v (%v)", values, err)
	}
}

// A document is stored the way it arrived. Re-encoding it from a struct would drop
// the holes — they are not fields — and take the values down with them.
func TestSaveTemplateStoresVerbatim(t *testing.T) {
	s := newTestStore(t)
	doc := "# the author's own comment\n" + holeTemplate
	if err := s.SaveTemplate("k8s", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, filepath.Join(storeDir(t, s, "k8s"), "config.toml"))); got != doc {
		t.Errorf("the template was rewritten on the way in:\n%s", got)
	}

	// The name belongs to the directory. An exported entry carries one, and pasting
	// it back as a single template would otherwise be stored where nobody looks.
	if err := s.SaveTemplate("k8s", []byte("name = \"other\"\n"+holeTemplate)); err == nil {
		t.Error("a document that names a different extension must be refused")
	}

	// A template that renders into something invalid is rolled back: the old
	// document still works, and the caller hears the reason.
	if err := s.SetValues("k8s", map[string]any{"command": "c", "targets": []any{map[string]any{"host": "pi"}}}); err != nil {
		t.Fatal(err)
	}
	broken := "mode = \"pipe\"\ncommand = \"{{command}}\"\n\n[params.command]\ntype = \"string\"\n"
	if err := s.SaveTemplate("k8s", []byte(broken)); err == nil {
		t.Fatal("a template that renders into an extension with no target should be refused")
	}
	if got := string(mustReadFile(t, filepath.Join(storeDir(t, s, "k8s"), "config.toml"))); got != doc {
		t.Errorf("a refused template change left the document as:\n%s\nwant the original", got)
	}
}

// The values file is this installation's: its hosts, its ports, its credentials.
// An export carries the recipe and nothing else.
func TestExportOmitsValues(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveTemplate("k8s", []byte(holeTemplate)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetValues("k8s", map[string]any{"command": "kubectl", "targets": "internal"}); err == nil {
		t.Fatal("targets declared as an array must not accept a string")
	}
	doc, err := s.ExportDocument(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "{{command}}") {
		t.Errorf("an export must carry the template, holes included:\n%s", doc)
	}
	if strings.Contains(string(doc), "kubectl") {
		t.Errorf("an export must not carry this installation's values:\n%s", doc)
	}
}

func storeDir(t *testing.T, s *Store, name string) string {
	t.Helper()
	dir, err := s.dir(name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return data
}
