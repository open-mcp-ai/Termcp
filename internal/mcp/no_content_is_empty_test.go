package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// A 204 WITH A BODY TEACHES THE CALLER THAT THE FORMAT LIES ABOUT ITS OWN STATUS LINE.
//
// The envelope is HTTP because a model already has prior on it (see the package comment
// on internal/envelope), and the one thing every model knows about 204 is that it carries
// no content. So a 204 whose body explains itself does not merely bend a rule: it makes
// the status line untrustworthy, and that prior — the reason the format was chosen — is
// spent. Measured on `read <channel> --frame` over bytes that are not a frame, the reply
// was a `TERM/1 204 No Content` with "no complete frame yet — the bytes in front are not a
// frame: …" underneath it.
//
// THIS IS AN INVARIANT OVER THE SLOT LITERALS, not a fixed-reply test, because the body
// at the offending sites was assembled from a live buffer and a peer's bytes: no fixture
// can see every one of them, and a sixth 204 written the old way would pass a per-site
// test by not being covered. The one place the reason may live is the Hint header
// (noContentSaying), which every site now routes through.
//
// The scan is over the package's own non-test source, which is where a slot is BUILT; a
// reply rendered from a header is what the test beside this one checks on the wire.
func TestNoContentSlotNeverSetsABody(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isSlotLiteral(lit.Type) {
				return true
			}
			var body ast.Expr
			code := -1
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Code":
					if basic, ok := kv.Value.(*ast.BasicLit); ok && basic.Kind == token.INT {
						if v, err := strconv.Atoi(basic.Value); err == nil {
							code = v
						}
					}
				case "Body":
					body = kv.Value
				}
			}
			if code != 204 {
				return true
			}
			sites++
			if body == nil {
				return true
			}
			if basic, ok := body.(*ast.BasicLit); ok && basic.Kind == token.STRING && basic.Value == `""` {
				return true
			}
			t.Errorf("%s: a 204 slot sets Body; HTTP says 204 carries no content and this format is HTTP on purpose, so the reason belongs in a header (noContentSaying) — the status line must not contradict the bytes under it",
				fset.Position(lit.Lbrace))
			return true
		})
	}
	// A scan that found nothing would pass while checking nothing: the package builds
	// many 204s, and the invariant only means something if they were looked at.
	if sites == 0 {
		t.Fatal("no 204 slot literal was found; the scan is not looking where it thinks")
	}
}

// isSlotLiteral reports whether a composite literal's type is a slot. An elided type
// ([]envelope.Slot{{Code: …}}) has no expression at all and is accepted: the Code key is
// what identifies a slot, since no other literal in this package carries one.
func isSlotLiteral(typ ast.Expr) bool {
	if typ == nil {
		return true
	}
	switch t := typ.(type) {
	case *ast.Ident:
		return t.Name == "Slot"
	case *ast.SelectorExpr:
		return t.Sel.Name == "Slot"
	}
	return false
}

// AND ON THE WIRE: `read <channel> --frame` over a channel holding no frame answers a 204
// whose body is EMPTY, with the reason in the header block — because the caller still has
// to learn WHY, and this project's rule is that silence is worse than an error.
//
// The bytes here are the reported shape: a peer banner the scan will never turn into a
// message. What must survive is the quote of those bytes; what must not is a body under a
// 204.
func TestAFrameMissAnswersNoContentWithAnEmptyBody(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, `sh -c 'printf "just a banner, no frame\n"; sleep 5'`)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 1})
	if !statusLineIs(got, "204") {
		t.Fatalf("a buffer holding no frame must answer 204:\n%s", got)
	}
	head, body, ok := strings.Cut(got, "\n\n")
	if !ok {
		t.Fatalf("no blank line between the headers and the body:\n%q", got)
	}
	if body != "" {
		t.Fatalf("a 204 carried a body %q; HTTP says 204 has no content, and the status line must not contradict the bytes under it", body)
	}
	hint := headerValueOr(head, "Hint", "")
	for _, want := range []string{"no complete frame yet", "not a frame", "just a banner"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the 204 stopped saying why no frame was taken: Hint = %q, want it to name %q\n%s", hint, want, got)
		}
	}
}
