package mcp

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
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

	// saved is the persisted half (see alias_store.go). Nil when this instance was
	// given no data directory, which leaves the table exactly as process-local as it
	// used to be.
	saved *aliasStore

	// ext reads one extension template's `alias` table. Nil when no reconciler is
	// wired, so a bare Server resolves runtime and saved names only.
	//
	// IT IS A FUNCTION, NOT A STORE, because the caller that owns the reconciler
	// decides how a template is loaded (and the alias code has no business knowing).
	ext func(ext string) (map[string]string, bool)
}

// get reads one runtime entry.
func (a *aliasTable) get(name string) (string, bool) {
	v, ok := a.m.Load(name)
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// lookup resolves a name to the command line it stands for, in the order the
// sources are allowed to win:
//
//  1. RUNTIME — `alias x …` in this process. It is the operator's scratch space, so
//     it outranks everything for as long as the process lives.
//  2. SAVED — `create alias x …`, which survives a restart.
//  3. AN EXTENSION'S OWN TABLE, addressed as `<ext>.<alias>`.
//
// THE DOTTED NAME IS THE ONLY WAY INTO A TEMPLATE'S TABLE, and that is deliberate.
// A bare name that works only when exactly one installed extension happens to
// define it is the kind of hidden state an agent cannot see and therefore cannot
// trust: the same word would mean something on one machine and nothing on the next,
// and the failure would arrive as "unknown directive" long after the install. A
// qualified name says where it comes from, so `ls ext` can list it and a typo is a
// typo.
func (a *aliasTable) lookup(name string) (string, bool, []string) {
	if expansion, ok := a.get(name); ok {
		return expansion, true, nil
	}
	if a.saved != nil {
		if expansion, ok := a.saved.get(name); ok {
			return expansion, true, nil
		}
	}
	ext, alias, dotted := strings.Cut(name, ".")
	if !dotted || ext == "" || alias == "" || a.ext == nil {
		return "", false, nil
	}
	table, ok := a.ext(ext)
	if !ok {
		return "", false, nil
	}
	expansion, ok := table[alias]
	if !ok {
		return "", false, nil
	}
	return expansion, true, []string{ext}
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
// IT RETURNS A SLICE, NOT ONE STATEMENT (#284). An alias body is a command line,
// and a command line can hold several statements — `ls alias; ls host` is two. The
// single-statement return was not a simplification, it was a ceiling on the feature:
// the only way to write a compound alias was a way that did not work.
//
// THE CYCLE RESULT IS SEPARATE FROM THE ERROR. When the chain loops, the statement
// comes back untouched so the caller reports the name it was given — but expanding
// `aa` (→ bb → aa) and dispatching the word `aa` produced "unknown directive: aa",
// which sends the caller looking for a typo in a directive that is in fact defined.
// A cycle is only reachable through the alias table, so saying so is the difference
// between a puzzle and a fix.
func (a *aliasTable) Expand(stmt directive.Statement) ([]directive.Statement, []string, *directive.Error) {
	if _, ok, _ := a.lookup(stmt.Op); !ok {
		return []directive.Statement{stmt}, nil, nil
	}
	// NO SUBSTITUTION HERE: this runs on an already-parsed statement, so the
	// caller's values have been filled in. Passing an empty table keeps the
	// expansion's own `$N` text as the shell's own positional parameters, which is
	// what it was before this path existed.
	resolved, expansionErr, ok := a.resolve(stmt.Op, strings.TrimSpace(stmt.Target+" "+stmt.Payload), directive.Subst{})
	if ok {
		return resolved, nil, nil
	}
	if expansionErr != nil {
		// THE EXPANSION'S OWN MISTAKE, REPORTED AS ITSELF. The first word resolved,
		// so the failure happened inside the alias body; naming the alias again would
		// send the caller looking for a typo in a name that exists (#284 A1).
		return nil, nil, expansionErr
	}
	return []directive.Statement{stmt}, a.cyclePath(stmt.Op), nil
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
		expansion, ok, _ := a.lookup(name)
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
		if _, ok, _ := a.lookup(next); !ok {
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
func (a *aliasTable) expandErrorCycle(err error, cmd string, sub directive.Subst) ([]directive.Statement, bool, []string, *directive.Error) {
	derr, ok := err.(*directive.Error)
	if !ok {
		return nil, false, nil, nil
	}
	// THE OFFENDING WORD IS USUALLY IN THE ERROR, AND SOMETIMES THERE IS NO WORD.
	//
	// `unknown directive: zz1` names it. The unused-args refusal does not — it says
	// "cmd never references $1", which is a fact about the LINE, and for an alias
	// invocation it is a fact about the wrong line: the reference lives in the alias
	// body, so the caller's `say` legitimately names no $1 and the expansion is the
	// only text that can (#284). The first word of the command is the name to look up
	// in that case, because a directive is the first word of a statement.
	name := derr.Op
	if name == "" {
		name = firstWord(cmd)
	}
	if name == "" {
		return nil, false, nil, nil
	}
	stmts, expansionErr, ok := a.resolve(name, derr.Rest, sub)
	if ok {
		return stmts, true, nil, nil
	}
	if expansionErr != nil {
		// THE EXPANSION'S OWN MISTAKE, and reporting it beats "unknown directive".
		// The first word resolved into an alias, so the failure happened INSIDE the
		// expansion; naming the alias again would send the caller looking for a typo
		// in a directive that is in fact defined. The inner error says what the
		// expansion actually refused (#284 A1).
		return nil, false, nil, expansionErr
	}
	return nil, false, a.cyclePath(name), nil
}

// firstWord returns the command's first word, which is where a directive name sits.
//
// It stops at whitespace and at the operators that end a statement, so `say; ls` and
// `say && ls` still look up `say`. A quoted first word is not an alias name in any
// useful sense, and the table lookup will simply miss it.
func firstWord(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	for i, r := range trimmed {
		switch r {
		case ' ', '\t', '\n', '\r', ';', '&', '|':
			return trimmed[:i]
		}
	}
	return trimmed
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
//
// THE ERROR RESULT IS THE EXPANSION'S OWN MISTAKE (#284 A1), and it is returned
// instead of being discarded: the caller is the one who can teach it, and the caller
// that asked for `zz1` was never told that the problem is inside the expansion.
func (a *aliasTable) resolve(name, rest string, sub directive.Subst) ([]directive.Statement, *directive.Error, bool) {
	seen := map[string]bool{}
	for {
		expansion, ok, _ := a.lookup(name)
		if !ok || seen[name] {
			return nil, nil, false
		}
		seen[name] = true
		f, err := directive.ParseWithSubst(strings.TrimSpace(expansion+" "+rest), sub)
		if err == nil {
			if len(f.Statements) == 0 {
				return nil, nil, false
			}
			// MULTI-STATEMENT ALIASES RUN AS THEY ARE WRITTEN (#284 A2/A3). `ls alias;
			// ls host` is two statements, and refusing it made the only way to write a
			// compound alias a way that does not work. Chaining continues only from the
			// FIRST statement: an alias whose first word is another alias is a chain, one
			// whose second word is an alias is a compound that the executor handles per
			// statement.
			if first := f.Statements[0]; len(f.Statements) == 1 {
				if _, next, _ := a.lookup(first.Op); next {
					name, rest = first.Op, strings.TrimSpace(first.Target+" "+first.Payload)
					continue
				}
			}
			return f.Statements, nil, true
		}
		derr, ok := err.(*directive.Error)
		if !ok || derr.Op == "" {
			return nil, nil, false
		}
		if _, next, _ := a.lookup(derr.Op); !next {
			// THE FAILURE IS INSIDE THE EXPANSION, NOT AT THE NEXT HOP. The word the
			// error names is a real directive (or a shape error on one), so continuing the
			// walk would report "unknown directive" about a name that exists — the same
			// wrong answer the cycle branch already had to be taught out of.
			return nil, derr, false
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
		return s.listAliases(stmt, win)
	}
	name, expansion, _ := strings.Cut(body, " ")
	name, expansion = strings.TrimSpace(name), strings.TrimSpace(expansion)
	if expansion == "--off" {
		s.aliases.drop(name)
		if s.aliases.saved != nil {
			_, _ = s.aliases.saved.drop(name)
		}
		return envelope.Slot{Code: 204, Headers: headers}
	}
	if expansion == "" {
		return badRequest(stmt, "alias needs a directive to expand to: alias jira mcp open https://...")
	}
	// AN ALIAS MAY NOT REDEFINE A DIRECTIVE: it would make every caller of that
	// directive silently run the alias instead. A directive name belongs to the
	// protocol and cannot be claimed by a caller or a template.
	if _, isDirective := directive.Lookup(name); isDirective {
		return badRequest(stmt, "cannot define alias "+quoteOrDash(name)+": it is already a directive name")
	}
	// DOT NAMES ARE RESERVED FOR EXTENSION COMMANDS (`<ext>.<alias>`), so a user
	// alias can never collide with one or pretend to come from one.
	if strings.Contains(name, ".") {
		return badRequest(stmt, "cannot define alias "+quoteOrDash(name)+": dot names are reserved for extension commands (<ext>.<alias>)")
	}
	// ponytail: a saved alias has no capability gate of its own. It changes what a
	// NAME means for every later caller, which is the same class of risk as editing
	// an installed profile — add alias.set to internal/capability when the permission
	// catalog (#252) settles, rather than minting an id here and having it renamed.
	s.aliases.set(name, expansion)
	if stmt.Op == "create" || stmt.Op == "edit" {
		// THE SAVED COPY IS THE PERSISTENT ONE, and this is the verb that promises
		// it ("Create saved configuration. It outlives every session"). The plain
		// `alias` verb stays process-local, which is its documented draft behaviour.
		if s.aliases.saved != nil {
			_ = s.aliases.saved.set(name, expansion)
		}
	}
	return envelope.Slot{Code: 204, Headers: headers}
}

// listAliases renders the three layers in source order: saved, runtime, and the
// extensions that carry an `alias` table. THE SOURCE IS THE HEADING, because a
// name's meaning is exactly the thing a caller has to be able to trust, and "who
// defined it" is most of that answer.
func (s *Server) listAliases(stmt directive.Statement, win termArgs) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	var b strings.Builder

	// 1. SAVED (persists across restarts).
	if s.aliases.saved != nil {
		if saved := s.aliases.saved.sorted(); len(saved) > 0 {
			b.WriteString("SAVED ALIASES (saved to disk):\n")
			for _, pair := range saved {
				b.WriteString("  " + pair[0] + " = " + pair[1] + "\n")
			}
		}
	}

	// 2. RUNTIME (this process only).
	runtimeEntries := s.aliases.snapshot()
	if len(runtimeEntries) > 0 {
		names := make([]string, 0, len(runtimeEntries))
		for n := range runtimeEntries {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("RUNTIME ALIASES (this process only):\n")
		for _, n := range names {
			b.WriteString("  " + n + " = " + runtimeEntries[n] + "\n")
		}
	}

	// 3. EXTENSIONS (ship with the template, addressed as <ext>.<alias>).
	if extAliases := s.extensionAliasesSorted(); len(extAliases) > 0 {
		b.WriteString("EXTENSION ALIASES (<ext>.<alias>):\n")
		for _, item := range extAliases {
			b.WriteString("  " + item[0] + " = " + item[1] + "\n")
		}
	}

	if b.Len() == 0 {
		return envelope.Slot{Code: 200, Headers: headers, Body: "(no aliases)\n"}
	}
	capped, code, cut, total := capGeneratedCounted(b.String(), win)
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: capped}
}

// extensionAliasesSorted renders every enabled extension's alias table sorted by
// qualified name.
func (s *Server) extensionAliasesSorted() [][2]string {
	if s.extensions == nil {
		return nil
	}
	names, err := s.extensions.Store().List()
	if err != nil {
		return nil
	}
	var out [][2]string
	for _, name := range names {
		ext, err := s.extensions.Store().Load(name)
		if err != nil || ext == nil || !ext.IsEnabled() || len(ext.Alias) == 0 {
			continue
		}
		for alias, exp := range ext.Alias {
			out = append(out, [2]string{ext.Name + "." + alias, exp})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
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
