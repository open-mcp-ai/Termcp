package term_test

import (
	"context"
	"testing"

	"github.com/open-mcp-ai/termcp/term"
)

// recorder is a capability layer that answers every verb the same way and keeps
// what it was asked, so a test can assert on the plan that reached it.
type recorder struct {
	ops   []string
	code  int
	slots int
}

func (r *recorder) Handle(ctx context.Context, st term.Statement) []term.Slot {
	r.ops = append(r.ops, st.Op)
	if r.slots == 0 {
		return []term.Slot{{Code: r.code, Headers: map[string]string{"Path": st.Path}, Body: "ok"}}
	}
	out := make([]term.Slot, 0, r.slots)
	for i := 0; i < r.slots; i++ {
		out = append(out, term.Slot{Code: r.code, Headers: map[string]string{"Path": st.Path}, Body: "ok"})
	}
	return out
}

// vocabulary is what a program embedding TERM/1 writes: its own verbs, declared
// here, with nothing borrowed from termcp. That is the whole shape — a grammar is
// an argument to New, never a default.
func vocabulary() *term.Grammar {
	return term.NewGrammar([]term.Op{
		{Name: "deploy", Kind: term.KindPayload, Target: true, Usage: "deploy <env> $1"},
		{Name: "status", Kind: term.KindFlags, Usage: "status"},
	})
}

// mustParse parses against the caller's own verbs or fails the test.
func mustParse(t *testing.T, tl *term.Term, src string) *term.File {
	t.Helper()
	f, err := tl.Parse(src, term.Subst{})
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return f
}

// TestAVerbTheLanguageNeverHeardOfParses is the point of NewGrammar: the parser
// accepts a verb this build has never heard of, and cuts its words by the
// caller's declaration.
func TestAVerbTheLanguageNeverHeardOfParses(t *testing.T) {
	tl := term.New(vocabulary())

	file, err := tl.Parse("deploy prod $1", term.Subst{Pos: []string{"make release"}})
	if err != nil {
		t.Fatalf("a declared verb was refused: %v", err)
	}
	if len(file.Statements) != 1 {
		t.Fatalf("parsed %d statements, want 1", len(file.Statements))
	}
	st := file.Statements[0]
	if st.Op != "deploy" || st.Target != "prod" {
		t.Errorf("parsed op=%q target=%q, want deploy/prod", st.Op, st.Target)
	}
	if st.Payload != "make release" {
		t.Errorf("payload = %q, want the substituted command line", st.Payload)
	}
}

// TestTheVocabularyIsClosedToWordsItDidNotDeclare is the other half: the table an
// embedding program passes in is the only table, so termcp's verbs are not there
// unless the caller put them there.
func TestTheVocabularyIsClosedToWordsItDidNotDeclare(t *testing.T) {
	tl := term.New(vocabulary())
	if _, err := tl.Parse("ls session", term.Subst{}); err == nil {
		t.Fatal("a verb the caller never declared parsed; the vocabulary is not closed")
	}
}

// TestGrammarIsNotSharedBetweenCallers pins that one caller's table cannot leak
// into another Term's parse — the reason the vocabulary is a value, not a package
// variable.
func TestGrammarIsNotSharedBetweenCallers(t *testing.T) {
	mine := term.New(vocabulary())
	if _, err := mine.Parse("deploy prod $1", term.Subst{Pos: []string{"x"}}); err != nil {
		t.Fatalf("the caller's own grammar refused its own verb: %v", err)
	}

	theirs := term.New(term.NewGrammar(nil))
	if _, err := theirs.Parse("deploy prod $1", term.Subst{Pos: []string{"x"}}); err == nil {
		t.Fatal("a custom grammar leaked into an unrelated Term")
	}
}

// TestOpsIsACopy pins that Ops() cannot reach into the table the parser indexes.
// Before this it returned the backing slice itself, so one caller's edit could
// rewrite the verbs of every other.
func TestOpsIsACopy(t *testing.T) {
	g := vocabulary()
	ops := g.Ops()
	before := len(ops)
	ops[0].Name = "hijacked"
	if _, ok := g.Lookup("hijacked"); ok {
		t.Fatal("Ops() returned the indexed table; a caller's edit reached the parser")
	}
	if got := len(g.Ops()); got != before {
		t.Fatalf("Ops() length changed from %d to %d", before, got)
	}
}

// TestThePlanIsWholeBeforeAnythingRuns is where review lives now: between Parse
// and Run, at the call site. The plan carries every statement — including the
// branches && and || would skip — so a gate can decide once for the line.
func TestThePlanIsWholeBeforeAnythingRuns(t *testing.T) {
	h := &recorder{code: 500} // every statement fails, so && branches cannot run
	tl := term.New(vocabulary(), term.WithHandler(h))

	file := mustParse(t, tl, "status; status && status; status")
	if len(file.Statements) != 4 {
		t.Fatalf("the plan has %d statements, want all 4 before anything runs", len(file.Statements))
	}
	if file.Statements[2].Cond != "&&" {
		t.Fatalf("the gated statement lost its condition: %q", file.Statements[2].Cond)
	}
	if len(h.ops) != 0 {
		t.Fatalf("the handler ran %v while the caller was still looking at the plan", h.ops)
	}

	// A gate is the caller's code: refuse here and Run is never reached.
	refuse := true
	if !refuse {
		tl.Run(context.Background(), file)
	}
	if len(h.ops) != 0 {
		t.Fatalf("a refused line reached the handler: %v", h.ops)
	}

	reply := tl.Run(context.Background(), file)
	if got := len(h.ops); got != 3 {
		t.Errorf("handler ran %d statements (%v), want 3: the && branch must be skipped", got, h.ops)
	}
	if len(reply.Slots) != 4 {
		t.Errorf("reply has %d slots, want one per statement", len(reply.Slots))
	}
}

// TestAnUnparseableLineNeverReachesTheHandler pins the first half of the order:
// syntax is decided before anything can run.
func TestRunIsParseReviewExecuteInThatOrder(t *testing.T) {
	h := &recorder{code: 200}
	tl := term.New(vocabulary(), term.WithHandler(h))

	if _, err := tl.Parse("nosuchverb x", term.Subst{}); err == nil {
		t.Fatal("an unparseable line was accepted")
	}
	if len(h.ops) != 0 {
		t.Error("the handler ran a line that did not parse")
	}
}

// TestATermWithNoHandlerRefusesToRun pins the honest default: a Term can be built
// to check whether a line is legal, and it must say so rather than answer as if it
// had run.
func TestATermWithNoHandlerRefusesToRun(t *testing.T) {
	tl := term.New(vocabulary())
	reply := tl.Run(context.Background(), mustParse(t, tl, "status"))
	if len(reply.Slots) != 1 || reply.Slots[0].Code != 403 {
		t.Fatalf("a handler-less Term answered %+v, want one 403", reply.Slots)
	}
}
