package term

import (
	"context"
	"sync"
)

// Execution: the rules that live BETWEEN directives.
//
// Parsing turns a line into statements and rendering turns slots into an
// envelope; both are already here. What was missing is the part in between, and
// it is the part with the most protocol in it — `&&` and `||` gating, `&` making
// a group run together, one slot per statement so slot i is statement i, and 208
// for the branch that was not taken.
//
// That logic used to be unreachable: it lived on this project's server type, so a
// program that wanted TERM/1 had to re-implement it, and every re-implementation
// got the slot alignment or the short-circuit subtly wrong. It is not capability
// — nothing here knows what a session is — so it belongs to the protocol.
//
// # The three layers
//
//	entry        HTTP / MCP transport / your own loop        (yours, or termcp's)
//	term.Execute    sequencing, one slot per statement, 208     (this file)
//	Handler      what each verb DOES                         (yours, or termcp's)
//
// A handler is the whole of the capability layer's obligation: one statement in,
// its slots out. Alias expansion, dry runs and "is this byte safe for a terminal"
// are decisions about capabilities, so they stay on the handler's side.

// Handler is the capability layer: one statement, its answer.
//
// It returns a SLICE because a verb may address several objects at once — `run
// a:1,a:2 $1` is one statement and two slots — and collapsing that here would
// lose the alignment the protocol promises.
//
// Implementations must be safe for concurrent use: Execute runs the members of an
// `&` group in parallel.
type Handler interface {
	Handle(ctx context.Context, st Statement) []Slot
}

// Options are the evaluation's own knobs. They are deliberately few: anything
// that changes what a verb does belongs to the Handler, not here.
type Options struct {
	// LearnMode attaches the directive's name to the first slot it produces, so a
	// caller that asked to be taught gets the word it should look up. Off by
	// default: a reply that carries Usage headers on every call is noise.
	LearnMode bool
}

// executeGrammar runs every statement, in order, and returns one reply. It is what
// Term.Run calls; the vocabulary only changes which usage line LearnMode attaches,
// so an embedder that adds verbs gets its own text instead of silence.
//
// The guarantee is the protocol's: one slot per statement, in the same order. A
// statement skipped by `&&` or `||` still produces its slot, it just says 208 —
// which is what lets a caller map slot i back to the statement it wrote without
// counting. A statement that produced several slots (a batch target) breaks that
// arithmetic, which is why Handler returns a slice.
func executeGrammar(ctx context.Context, file *File, h Handler, opts Options, g *Grammar) Reply {
	if file == nil {
		return Reply{}
	}
	out := Reply{Slots: make([]Slot, 0, len(file.Statements))}
	previousOK := true
	seen := map[string]bool{}

	for _, group := range groups(file.Statements) {
		if len(group) == 1 {
			slots := runSequential(ctx, h, group[0], previousOK, opts, seen, g)
			out.Slots = append(out.Slots, slots...)
			previousOK = allOK(slots)
			continue
		}
		// A PARALLEL GROUP IS UNGATED, on purpose. Its members were written to run
		// at the same time, so "did the previous one succeed" has no answer yet:
		// gating on it would either serialize them (changing what `&` means) or
		// pick an arbitrary winner. `&&` between statements that run together is
		// meaningless, and the honest reading is to run both.
		slots := runParallel(ctx, h, group, opts, g)
		out.Slots = append(out.Slots, slots...)
		// The group's outcome is the AND of its members: a following `&&` should
		// see a failure inside the group, not only the last statement's.
		previousOK = allOK(slots)
	}
	return out
}

// runSequential runs one statement under the `&&`/`||` gate.
func runSequential(ctx context.Context, h Handler, st Statement, previousOK bool, opts Options, seen map[string]bool, g *Grammar) []Slot {
	if st.Cond == "&&" && !previousOK {
		return []Slot{skipped(st, "&&", "the previous statement failed")}
	}
	if st.Cond == "||" && previousOK {
		return []Slot{skipped(st, "||", "the previous statement succeeded")}
	}
	slots := h.Handle(ctx, st)
	if opts.LearnMode && len(slots) > 0 && !seen[st.Op] {
		// THE HEADER CARRIES THE USAGE LINE, not the verb: a caller that has just
		// learned the name still needs its shape. It is read from the registry
		// rather than from the statement so the two cannot disagree.
		if op, ok := g.Lookup(st.Op); ok && op.Usage != "" {
			if slots[0].Headers == nil {
				slots[0].Headers = map[string]string{}
			}
			slots[0].Headers["Usage"] = op.Usage
			// ONCE PER CALL, not once per statement: the point is to introduce a
			// verb, and repeating it on the second use is noise.
			seen[st.Op] = true
		}
	}
	return slots
}

// runParallel runs a group, keeping the slots in statement order.
//
// The slots are collected per position rather than appended as they finish:
// completion order is not statement order, and a reply whose slots are shuffled
// breaks the one guarantee Execute makes.
func runParallel(ctx context.Context, h Handler, group []Statement, opts Options, g *Grammar) []Slot {
	results := make([][]Slot, len(group))
	var wg sync.WaitGroup
	for i, st := range group {
		wg.Add(1)
		go func(i int, st Statement) {
			defer wg.Done()
			results[i] = h.Handle(ctx, st)
		}(i, st)
	}
	wg.Wait()
	var out []Slot
	for i, slots := range results {
		if opts.LearnMode && len(slots) > 0 {
			// A parallel member cannot dedup against its neighbours without a lock
			// around state the sequential path also writes, and repeating a usage
			// line is the honest outcome: they really did run twice.
			if op, ok := g.Lookup(group[i].Op); ok && op.Usage != "" {
				if slots[0].Headers == nil {
					slots[0].Headers = map[string]string{}
				}
				slots[0].Headers["Usage"] = op.Usage
			}
		}
		out = append(out, slots...)
	}
	return out
}

// groups splits statements where `&` ended one. A group of one is an ordinary
// statement; a group of several runs together.
func groups(stmts []Statement) [][]Statement {
	var out [][]Statement
	var cur []Statement
	for _, st := range stmts {
		cur = append(cur, st)
		if !st.Background {
			out = append(out, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// allOK reports whether every slot is 2xx.
//
// Every, not the first: a statement that produced several slots is only a success
// if all of them are, and a chain gating on one would call a partial failure a
// success.
func allOK(slots []Slot) bool {
	for _, sl := range slots {
		if sl.Code >= 300 {
			return false
		}
	}
	return true
}

// skipped is the slot for a branch the condition refused.
//
// 208 rather than 204: 204 says "done, nothing to say", which is also what a
// successful close answers, so a caller could not tell a statement it skipped from
// one that ran. 208 is outside the range a directive normally returns without
// being an error — the chain worked as written, one branch simply was not taken.
func skipped(st Statement, op, why string) Slot {
	return Slot{
		Code: 208,
		Headers: map[string]string{
			"Path":       st.Path,
			"Error-Code": "short_circuited",
		},
		Body: "not run: `" + op + "` and " + why,
	}
}
