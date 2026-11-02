package mcp

import (
	"context"
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// alias, and the command space.
//
// An alias is a name for a directive: it is expanded ONCE, at parse time, and the
// reply's Path shows the expansion, so a caller reading the reply sees the command
// that actually ran rather than the name it typed. Runtime aliases are a draft —
// they live in this process and are gone at restart. Persisting one means writing
// it into an extension's [alias] table, which is the distribution unit: install the
// extension, get the command.

// runtimeAliases is this process's alias table.
//
// The lock is NOT decoration: a Term call is allowed to run concurrently with
// another (the MCP server hands each request to its own goroutine, and HTTP does
// the same), so an `alias jira …` in flight races every statement being parsed
// against a plain map. That is a data race the Go runtime panics on under -race and
// corrupts under load — the worst kind, because it shows up as an alias resolving
// to a mangled expansion rather than as a crash.
//
// An alias table is a key-value store with no multi-key invariant: every operation
// touches exactly one entry. That is the shape sync.Map is for, so there is no
// lock to get wrong.
type aliasTable struct {
	m sync.Map // name string -> expansion string
}

// aliases is the process-wide table.
var aliases = &aliasTable{}

// get reads one entry.
func (a *aliasTable) get(name string) (string, bool) {
	v, ok := a.m.Load(name)
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// set writes one entry.
func (a *aliasTable) set(name, expansion string) {
	a.m.Store(name, expansion)
}

// drop removes one entry.
func (a *aliasTable) drop(name string) {
	a.m.Delete(name)
}

// snapshot copies the table for a listing. Range does not promise a consistent
// snapshot, which is fine here: the answer is a rendering for a human, and a
// listing that misses an alias added a microsecond ago is not an error.
func (a *aliasTable) snapshot() map[string]string {
	out := make(map[string]string)
	a.m.Range(func(k, v any) bool {
		name, _ := k.(string)
		expansion, _ := v.(string)
		out[name] = expansion
		return true
	})
	return out
}

// Expand resolves a directive's first word through the alias table. The
// expansion may itself name an alias, so resolution repeats until it reaches a
// real directive (with a cycle guard).
//
// THE SECOND RESULT IS A CYCLE, NOT A FAILURE. When the chain loops, the statement
// comes back untouched so the caller reports the name it was given — but expanding
// `aa` (→ bb → aa) and dispatching the word `aa` produced "unknown directive: aa",
// which sends the caller looking for a typo in a directive that is in fact defined.
// A cycle is only reachable through the alias table, so saying so is the difference
// between a puzzle and a fix.
func (a *aliasTable) Expand(stmt directive.Statement) (directive.Statement, []string) {
	if _, ok := a.get(stmt.Op); !ok {
		return stmt, nil
	}
	// NO SUBSTITUTION HERE: this runs on an already-parsed statement, so the
	// caller's values have been filled in. Passing an empty table keeps the
	// expansion's own `$N` text as the shell's own positional parameters, which is
	// what it was before this path existed.
	if resolved, ok := a.resolve(stmt.Op, strings.TrimSpace(stmt.Target+" "+stmt.Payload), directive.Subst{}); ok {
		return resolved, nil
	}
	return stmt, a.cyclePath(stmt.Op)
}

// cyclePath replays the expansion chain from name and returns the names it
// revisits, or nil when the chain simply does not end at a directive.
func (a *aliasTable) cyclePath(name string) []string {
	seen := map[string]bool{}
	var path []string
	for {
		if seen[name] {
			return append(path, name)
		}
		seen[name] = true
		path = append(path, name)
		expansion, ok := a.get(name)
		if !ok {
			return nil
		}
		// The next hop can come from a SUCCESSFUL parse or from the parse ERROR:
		// an expansion naming another alias is not a directive, so ParseWithSubst
		// rejects it and carries the offending word in Error.Op. resolve() reads the
		// chain the same way; ignoring the error path here is what made this walk stop
		// at the first hop and report no cycle.
		f, err := directive.ParseWithSubst(strings.TrimSpace(expansion), directive.Subst{})
		next := ""
		if err == nil && len(f.Statements) == 1 {
			next = f.Statements[0].Op
		} else if derr, ok := err.(*directive.Error); ok && derr.Op != "" {
			next = derr.Op
		}
		if _, ok := a.get(next); !ok {
			return nil
		}
		name = next
	}
}

// expandErrorCycle resolves the directive a parse error named, reporting a cycle
// separately from "this word never resolved".
//
// THE PARSE ERROR IS WHERE AN ALIAS IS FIRST SEEN. An invented directive is unknown
// to the parser by definition, so it fails there and is resolved from the error —
// which means Expand() below is never reached for the common case, and a cycle that
// only Expand() knew how to report still came back as "unknown directive: aa". Both
// doors need the same answer.
func (a *aliasTable) expandErrorCycle(err error) (directive.Statement, bool, []string) {
	derr, ok := err.(*directive.Error)
	if !ok || derr.Op == "" {
		return directive.Statement{}, false, nil
	}
	if stmt, ok := a.resolve(derr.Op, derr.Rest, directive.Subst{}); ok {
		return stmt, true, nil
	}
	return directive.Statement{}, false, a.cyclePath(derr.Op)
}

// resolve follows a name through the table until it lands on a directive the
// parser knows, or fails (unknown name, unparseable expansion, or a cycle).
//
// IT RE-PARSES WITH THE CALLER'S SUBSTITUTION TABLE, and that is not a detail. The
// first parse happened against `ech $1`, where `ech` was an unknown directive — so
// the `$1` was still raw text. If this pass parsed the expansion WITHOUT the table,
// the value would never be filled in; if it parsed the ALREADY-SUBSTITUTED rest, the
// value would arrive quoted as data and a positional payload would stop being syntax.
// Passing the table through is what makes an alias expand to the same statement the
// caller would have written out in full.
func (a *aliasTable) resolve(name, rest string, sub directive.Subst) (directive.Statement, bool) {
	seen := map[string]bool{}
	for {
		expansion, ok := a.get(name)
		if !ok || seen[name] {
			return directive.Statement{}, false
		}
		seen[name] = true
		f, err := directive.ParseWithSubst(strings.TrimSpace(expansion+" "+rest), sub)
		if err == nil && len(f.Statements) == 1 {
			stmt := f.Statements[0]
			if _, next := a.get(stmt.Op); !next {
				return stmt, true
			}
			name, rest = stmt.Op, strings.TrimSpace(stmt.Target+" "+stmt.Payload)
			continue
		}
		derr, ok := err.(*directive.Error)
		if !ok || derr.Op == "" {
			return directive.Statement{}, false
		}
		name, rest = derr.Op, derr.Rest
	}
}

// directiveAlias manages the runtime table.
func (s *Server) directiveAlias(_ context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	// The payload is "<name> <expansion...>" verbatim, so the expansion is never
	// re-quoted: what the caller writes is what the alias runs.
	body := strings.TrimSpace(stmt.Payload)
	if body == "" {
		entries := aliases.snapshot()
		if len(entries) == 0 {
			return envelope.Slot{Code: 200, Headers: headers, Body: "(no aliases)\n"}
		}
		var b strings.Builder
		for name, expansion := range entries {
			b.WriteString(name + " = " + expansion + "\n")
		}
		capped, code, cut, total := capGeneratedCounted(b.String(), win)
		noteTruncated(headers, code, cut, total, win)
		return envelope.Slot{Code: code, Headers: headers, Body: capped}
	}
	name, expansion, _ := strings.Cut(body, " ")
	name, expansion = strings.TrimSpace(name), strings.TrimSpace(expansion)
	if expansion == "--off" {
		aliases.drop(name)
		return envelope.Slot{Code: 204, Headers: headers}
	}
	if expansion == "" {
		return badRequest(stmt, "alias needs a directive to expand to: alias jira mcp open https://...")
	}
	aliases.set(name, expansion)
	return envelope.Slot{Code: 204, Headers: headers}
}

// aliasCycleSlot reports an alias chain that never reaches a directive.
func aliasCycleSlot(stmt directive.Statement, cycle []string) envelope.Slot {
	return envelope.Slot{
		Code:    400,
		Headers: map[string]string{"Error-Code": "alias_cycle", "Path": stmt.Path},
		Body: "alias cycle: " + strings.Join(cycle, " → ") +
			"\nthe chain never reaches a directive; break it with `alias <name> --off`",
	}
}
