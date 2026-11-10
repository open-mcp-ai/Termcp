package guard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// lockTypes are the stdlib primitives this repository does not use in shipping
// code. sync.Cond is on the list for a second reason: it cannot be cancelled, so
// every wait built on it needs a helper goroutine to deliver a deadline (the
// comment at buffer.reader.go records the measurement: 24 waiters cost 48
// goroutines before the change and 24 after).
var lockTypes = map[string]bool{
	"Mutex":   true,
	"RWMutex": true,
	"Cond":    true,
	"Locker":  true,
}

// TestNoLockPrimitivesInShippingCode is a GATE ON THE SOURCE, not a style
// preference. Every mutex that used to be here was replaced by one of four shapes
// whose correctness is checkable where it is written: an immutable snapshot behind
// an atomic pointer, a single-owner goroutine, a 1-slot channel token, or the
// filesystem as the arbiter (see AGENTS.md, "Concurrency and deadlock prevention").
// A new lock is not a bug by itself — the patterns above work for the shapes this
// codebase happens to have — but it is a decision that has to be argued.
//
// IF A LOCK IS GENUINELY NEEDED, add the file to allowedLocks with the reason: the
// invariant it protects, why a snapshot/owner/token cannot express it, and which
// section is short enough to hold no I/O and no callback. The gate then still
// reviews every other file.
var allowedLocks = map[string]string{
	// Empty on purpose. Today: 0.
}

func TestNoLockPrimitivesInShippingCode(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".pi-tmp", "dist", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		scanned++
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for _, pos := range lockUsages(t, path) {
			if reason, ok := allowedLocks[rel]; ok && reason != "" {
				continue
			}
			hits = append(hits, rel+":"+pos)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// THE SCAN MUST NOT BE VACUOUS. A wrong root or a filter that matches nothing
	// would make this test pass forever while checking nothing; the floor is well
	// under the real count (219 at the time of writing) so ordinary churn is fine.
	if scanned < 100 {
		t.Fatalf("only %d shipping source files were scanned; the scan is not looking where it thinks", scanned)
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		t.Fatalf("shipping code uses a lock primitive (%d):\n\t%s\n\n"+
			"Pick the shape that fits instead: a snapshot behind an atomic pointer, a single-owner\n"+
			"goroutine, a 1-slot channel token, or a filesystem claim. If none of them can express the\n"+
			"invariant, add the file to allowedLocks with the reason (see the comment there).",
			len(hits), strings.Join(hits, "\n\t"))
	}
}

// lockUsages reports the sync selectors in one file, as "LINE TYPE".
//
// Parsing rather than grepping is the whole point: comments and strings are not
// AST nodes, so this file's own prose about sync.Mutex and sync.Cond — and the
// comments that explain each lock that was removed — cannot trip the gate. A grep
// would have to be taught to tell them apart, and it would get that wrong.
func lockUsages(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !lockTypes[sel.Sel.Name] {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "sync" {
			return true
		}
		out = append(out, fmt.Sprintf("%d sync.%s", fset.Position(sel.Pos()).Line, sel.Sel.Name))
		return true
	})
	return out
}

// THE DETECTOR ITSELF IS TESTED, because a gate that cannot see the thing it
// guards passes for the wrong reason. Each shape below is one people actually
// write: a field, an embedded lock, and a var.
func TestTheLockDetectorSeesEveryShape(t *testing.T) {
	src := `package p

import "sync"

type a struct {
	mu sync.Mutex
}

type b struct {
	sync.RWMutex
}

var c sync.Cond
var d sync.Locker
var e sync.Map
var f sync.Once
`
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := lockUsages(t, path)
	if len(got) != 4 {
		t.Fatalf("detector found %d lock primitives, want 4 (Mutex, RWMutex, Cond, Locker); got %v.\n"+
			"sync.Map and sync.Once are not locks and must not be flagged.", len(got), got)
	}
}

// AND IT DOES NOT SEE PROSE. The comments in this repository name the primitives
// constantly — every removed lock left a paragraph behind saying what it was
// replaced by — so a gate that flagged comments would make the explanation of the
// rule impossible to write.
func TestTheLockDetectorIgnoresCommentsAndStrings(t *testing.T) {
	src := "package p\n\n" +
		"// a sync.Mutex used to guard this; it is a snapshot now.\n" +
		"// sync.Cond cannot be cancelled, which is why it is gone.\n" +
		"var note = \"sync.RWMutex was removed here\"\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "prose.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lockUsages(t, path); len(got) != 0 {
		t.Fatalf("the detector flagged prose: %v", got)
	}
}