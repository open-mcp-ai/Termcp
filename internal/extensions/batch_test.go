package extensions

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// TestDocumentRoundTrip pins the property the panel depends on: what export
// writes is what import reads, names and all, so a set of extensions can leave one
// Termcp and arrive at another unchanged.
func TestDocumentRoundTrip(t *testing.T) {
	src := newTestStore(t)
	first := Extension{}
	first.Name = "docs"
	first.Target = "internal"
	first.Paths = []string{"~/skills/docs/"}
	first.Description = "the docs tree"
	if err := src.Save(&first); err != nil {
		t.Fatal(err)
	}
	second := Extension{Mode: "pipe"}
	second.Name = "tickets"
	second.Target = "internal"
	second.Command = "npx"
	second.Args = []string{"-y", "tickets-mcp"}
	second.Forwards = []ForwardDecl{{Kind: ForwardDynamic, LocalPort: 0}}
	if err := src.Save(&second); err != nil {
		t.Fatal(err)
	}

	data, err := src.ExportDocument(nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := newTestStore(t)
	result, err := dst.ImportDocument(data)
	if err != nil {
		t.Fatalf("importing an export should work: %v\n%s", err, data)
	}
	if result.Imported != 2 || len(result.Renamed) != 0 {
		t.Fatalf("importing into an empty store should take both names unchanged, got %+v", result)
	}
	for _, name := range []string{"docs", "tickets"} {
		got, err := dst.Load(name)
		if err != nil {
			t.Fatalf("%s did not survive the round trip: %v", name, err)
		}
		want, err := src.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Target != want.Target || got.Command != want.Command ||
			got.Description != want.Description || got.Notes != want.Notes {
			t.Errorf("%s came back as %+v, want %+v", name, got, want)
		}
		if strings.Join(got.Paths, ",") != strings.Join(want.Paths, ",") {
			t.Errorf("%s paths = %v, want %v", name, got.Paths, want.Paths)
		}
		if len(got.Forwards) != len(want.Forwards) {
			t.Errorf("%s forwards = %+v, want %+v", name, got.Forwards, want.Forwards)
		}
	}
}

// TestExportDocumentSelection pins the export of exactly the selected templates,
// including the stale-selection case: a name that no longer resolves is skipped so
// the file still downloads what is left.
func TestExportDocumentSelection(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"alpha", "beta"} {
		ext := Extension{}
		ext.Name = name
		ext.Target = "internal"
		ext.Paths = []string{"/srv/"}
		if err := s.Save(&ext); err != nil {
			t.Fatal(err)
		}
	}
	data, err := s.ExportDocument([]string{"beta", "deleted-since"})
	if err != nil {
		t.Fatalf("a selection with a missing name should still export: %v", err)
	}
	var doc struct {
		Extensions []map[string]any `toml:"extensions"`
	}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Extensions) != 1 || doc.Extensions[0]["name"] != "beta" {
		t.Fatalf("export should hold exactly the selection that still exists, got %v", doc.Extensions)
	}
}

// TestImportDocumentRenamesCollisions pins the non-destructive rule: an import
// never overwrites a stored template, and it says which entries it had to rename
// — including two entries of the same document fighting over one name.
func TestImportDocumentRenamesCollisions(t *testing.T) {
	s := newTestStore(t)
	stored := Extension{}
	stored.Name = "docs"
	stored.Target = "internal"
	stored.Paths = []string{"/srv/kept/"}
	if err := s.Save(&stored); err != nil {
		t.Fatal(err)
	}

	doc := `
[[extensions]]
name = "docs"
target = "internal"
paths = ["/srv/imported/"]

[[extensions]]
name = "docs"
target = "internal"
paths = ["/srv/second/"]

[[extensions]]
name = "fresh"
target = "internal"
paths = ["/srv/fresh/"]
`
	result, err := s.ImportDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 3 {
		t.Fatalf("imported = %d, want 3", result.Imported)
	}
	if len(result.Renamed) != 2 {
		t.Fatalf("both entries colliding on \"docs\" should be renamed, got %+v", result.Renamed)
	}
	kept, err := s.Load("docs")
	if err != nil {
		t.Fatal(err)
	}
	if !allowsPath(t, kept, "/srv/kept/") {
		t.Error("the stored template must not have been overwritten by the import")
	}
	for _, rename := range result.Renamed {
		stored, err := s.Load(rename.To)
		if err != nil {
			t.Fatalf("imported entry %q should exist: %v", rename.To, err)
		}
		if rename.From != "docs" {
			t.Errorf("rename from = %q, want docs", rename.From)
		}
		if stored.Name != rename.To {
			t.Errorf("template stored as %q but reported as %q", stored.Name, rename.To)
		}
	}
}

// TestImportDocumentRejectsBrokenDocuments pins that an import is all-or-nothing:
// a document that is not the expected shape, or that holds an invalid template,
// leaves the store exactly as it was rather than importing half of it.
func TestImportDocumentRejectsBrokenDocuments(t *testing.T) {
	s := newTestStore(t)
	good := Extension{}
	good.Name = "keep-me"
	good.Target = "internal"
	good.Paths = []string{"/srv/"}
	if err := s.Save(&good); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"unknown table", "[[hosts]]\nname = \"x\"\n"},
		{"no entries", "[extensions]\n"},
		{"missing name", "[[extensions]]\nkind = \"skill\"\ntarget = \"internal\"\npaths = [\"/srv/\"]\n"},
		{"invalid name", "[[extensions]]\nname = \"bad name\"\nkind = \"skill\"\ntarget = \"internal\"\npaths = [\"/srv/\"]\n"},
		{"invalid template", "[[extensions]]\nname = \"no-shell\"\nkind = \"mcp\"\ntarget = \"\"\n"},
		{"skill without paths", "[[extensions]]\nname = \"empty-window\"\nkind = \"skill\"\ntarget = \"internal\"\n"},
	} {
		if _, err := s.ImportDocument([]byte(tc.doc)); err == nil {
			t.Errorf("%s: import should have been refused", tc.name)
		}
		list, err := s.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0] != "keep-me" {
			t.Fatalf("%s: a refused import must leave the store untouched, got %v", tc.name, list)
		}
	}
}

// allowsPath reports whether a stored template's own window grants a path, read
// through the same constructor a session uses.
func allowsPath(t *testing.T, e *Extension, p string) bool {
	t.Helper()
	r, err := session.NewRestriction(e.Allow, e.Paths)
	if err != nil {
		t.Fatalf("restriction: %v", err)
	}
	return r.AllowsPath(p)
}
