package directive

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// What a value becomes when it arrives in cmd, measured for every declared flag.
//
// THE QUESTION IS INSERTION, NOT SUBSTITUTION. Substitution is settled: a `$1` names a
// value and the value arrives. What the caller cannot see — and what this pins — is the
// SHAPE the value has when it lands: whether a space in it makes one word or two, whether
// a `;` ends the statement, whether a value that looks like `--mode` becomes a flag. A
// value that changes the shape of the line it lands in is data that has become grammar,
// and that is the one thing this channel exists to prevent.
//
// Two insertion modes exist and they are declared, not guessed:
//
//   - ONE WORD (the default): the value is placed as a single shell word, so the parser
//     reads it as one word whatever it contains. This is every flag value, every key,
//     every positional argument and the target.
//   - VERBATIM (declared): the value reaches its destination byte for byte, because the
//     destination is not a shell — the payload of `run`/`input` (a shell line), the
//     payload of `call` (a frame), and the tail of a word-list flag (`--argv`, `--args`).
//
// The probe that found the two defects below walked the positions I could think of; this
// walks the TABLE, so a flag added later is covered by being declared rather than by
// being remembered.

// flagProbes is one command per verb that exercises that verb's value flags.
//
// EVERY VERB WITH A VALUE FLAG HAS AN ENTRY, and TestEveryVerbWithValueFlagsIsProbed
// fails when one is missing: a verb that quietly drops out of this map would take its
// flags out of the measurement without failing anything, which is the failure mode of a
// hand-kept list.
var flagProbes = map[string]string{
	// `--argv` and `--args` take the REST OF THE LINE, so they are last: a known flag
	// after one is refused rather than handed to the program.
	"connect": `connect ${1} --name ${2} --on-exit ${3} --rows ${4} --cols ${5} --size ${6} --argv ${7}`,
	"open": `open h --name ${1} --on-exit ${2} --rows ${3} --cols ${4} --size ${5} --bind-host ${6} --remote-host ${7} ` +
		`--remote-port ${8} --local-port ${9} --local-host ${10} --target ${11} --argv ${12}`,
	"get":    `get h file /x --op ${1}`,
	"close":  `close h --grace ${1} --forward-id ${2}`,
	"key":    `key h:1 ctrl+c --repeat ${1}`,
	"reader": `reader h:1 --drop ${1}`,
	"read":   `read h:1 --cursor ${1} --since ${2} --at ${3} --offset ${4} --bytes ${5} --from-line ${6} --tail ${7} --stream ${8} --grep ${9} --grep-limit ${10} --encoding ${11}`,
	"run":    `run h:1 --tail ${1} --grep ${2} --grep-limit ${3} --encoding ${4} $cmd`,
	"write":  `write h file /x --data ${1} --mode ${2} --offset ${3} --length ${4} --size ${5} --old-text ${6} --new-text ${7} --op ${8} --uid ${9} --gid ${10} --atime ${11} --mtime ${12}`,
	"notify": `notify --level ${1} --on ${2} --title ${3} --for ${4} body`,
	"create": `create host --name ${1} --host ${2} --user ${3} --port ${4} --password ${5} --private-key ${6} --key-passphrase ${7} ` +
		`--known-hosts ${8} --dial-timeout-seconds ${9} --proxy ${10} --description ${11} --default-shell ${12} --default-mode ${13} ` +
		`--jump-host ${14} --jump-user ${15} --jump-port ${16} --jump-password ${17} --jump-private-key ${18} --jump-key-passphrase ${19} ` +
		`--source ${20} --target ${21} --command ${22} --url ${23} --headers ${24} --allow ${25} --paths ${26} ` +
		`--notes ${27} --forwards ${28} --link-path ${29} --mode ${30} --op ${31} --args ${32}`,
	"edit": `edit host ${1} --name ${2} --host ${3} --user ${4} --port ${5} --password ${6} --private-key ${7} --key-passphrase ${8} ` +
		`--known-hosts ${9} --dial-timeout-seconds ${10} --proxy ${11} --description ${12} --default-shell ${13} --default-mode ${14} ` +
		`--jump-host ${15} --jump-user ${16} --jump-port ${17} --jump-password ${18} --jump-private-key ${19} --jump-key-passphrase ${20} ` +
		`--target ${21} --command ${22} --url ${23} --headers ${24} --allow ${25} --paths ${26} --notes ${27} --forwards ${28} --args ${29}`,
}

// TestEveryVerbWithValueFlagsIsProbed keeps the map above honest.
func TestEveryVerbWithValueFlagsIsProbed(t *testing.T) {
	for _, op := range Ops() {
		hasValue := false
		for _, f := range op.FlagNames() {
			if !isValueless(op, f) {
				hasValue = true
				break
			}
		}
		if !hasValue {
			continue
		}
		if _, ok := flagProbes[op.Name]; !ok {
			t.Errorf("%s declares value flags but has no probe command, so nothing measures how its values arrive", op.Name)
		}
	}
}

// TestAStringValueArrivesAsOneWord feeds every value flag a string full of the
// characters that mean something to a shell, an argv parser or a flag reader, and
// requires the value back unchanged.
//
// A value that cannot arrive intact is a value the caller has to encode, and an
// encoding the caller must remember is one the model will get wrong — which is why the
// assertion is equality rather than "it parsed".
func TestAStringValueArrivesAsOneWord(t *testing.T) {
	compared := 0
	defer func() {
		// A TEST THAT MEASURES NOTHING PASSES FOR THE SAME REASON A CHECK THAT NEVER
		// RUNS DOES. This file has already been vacuous twice: once because it looked
		// for `--flag=$N` when the probes are space-separated, and once because the
		// filler for the other references did not satisfy their domains, so every case
		// failed on a neighbour. Both versions were green. The count is the tripwire.
		var want int
		for verb, cmd := range flagProbes {
			op, _ := Lookup(verb)
			for _, name := range op.FlagNames() {
				if isValueless(op, name) || flagRef(cmd, name) == "" {
					continue
				}
				if _, domain, _ := flagDomain(op, name); domain != "" {
					continue
				}
				want++
			}
		}
		if compared != want || want < 40 {
			t.Fatalf("compared %d flags, want %d (and at least 40): the probe is not reaching the table", compared, want)
		}
	}()
	// space, pipe, semicolon, ampersand, both quotes, a dollar, a backtick, a redirect,
	// a newline: each of these ends a word, a statement or a value somewhere.
	hostile := "a b|c;d&e'f\"g$h`i>j\nk"
	for verb, cmd := range flagProbes {
		op, ok := Lookup(verb)
		if !ok {
			t.Fatalf("%s is not declared", verb)
		}
		for _, name := range op.FlagNames() {
			if isValueless(op, name) {
				continue
			}
			// Only the flags this command sets, and only the ones that take free text:
			// a declared domain is measured by the test below.
			ref := flagRef(cmd, name)
			if ref == "" {
				continue
			}
			if _, domain, declared := flagDomain(op, name); declared && domain != "" {
				continue
			}
			f, err := ParseWithSubst(cmd, substFor(cmd, verb, ref, hostile))
			if err != nil {
				t.Errorf("%s --%s: a hostile value was refused: %v", verb, name, err)
				continue
			}
			got, ok := f.Statements[0].Flag(name)
			if !ok {
				continue
			}
			compared++
			if got != hostile {
				t.Errorf("%s --%s: value arrived as %q, want %q", verb, name, got, hostile)
			}
		}
	}
}

// TestADeclaredDomainAcceptsAReference is the defect the probe found: every flag with a
// declared domain was UNREACHABLE through a reference.
//
// The check ran before the holes were filled, so the value it tested was the
// substitution's stand-in token and the refusal read `--repeat takes a number, got
// "__hole_9f3c_0__"`. A caller whose `$1` held `3` was told their 3 was not a number,
// for every numeric, enum and geometry flag in the table — and the same check on a
// literal worked, so the tests that covered it passed.
func TestADeclaredDomainAcceptsAReference(t *testing.T) {
	for verb, cmd := range flagProbes {
		op, _ := Lookup(verb)
		for _, name := range op.FlagNames() {
			if isValueless(op, name) {
				continue
			}
			ref := flagRef(cmd, name)
			if ref == "" {
				continue
			}
			_, domain, _ := flagDomain(op, name)
			if domain == "" {
				continue
			}
			good, bad := domainSamples(domain)
			t.Run(verb+"_"+name, func(t *testing.T) {
				if _, err := ParseWithSubst(cmd, substFor(cmd, verb, ref, good)); err != nil {
					t.Errorf("--%s: %q through a reference was refused: %v", name, good, err)
				}
				// And the check still has to fire on a value that arrived that way: a
				// check that runs after the value exists is the only one that can.
				_, err := ParseWithSubst(cmd, substFor(cmd, verb, ref, bad))
				if err == nil {
					t.Fatalf("--%s: %q through a reference was accepted", name, bad)
				}
				if strings.Contains(err.Error(), "__hole_") {
					t.Errorf("--%s: the refusal quotes the substitution marker:\n%v", name, err)
				}
				if !strings.Contains(err.Error(), bad) {
					t.Errorf("--%s: the refusal does not show what was received:\n%v", name, err)
				}
			})
		}
	}
}

// TestNoRefusalQuotesTheSubstitutionMarker is the rule behind the two defects above.
//
// The marker is this parser's bookkeeping. Quoting it back tells the caller their input
// contains something it does not, and the message stops being a lesson and becomes a
// puzzle — the caller has no way to search their own cmd for `__hole_9f3c_0__`.
func TestNoRefusalQuotesTheSubstitutionMarker(t *testing.T) {
	cases := []struct {
		name string
		src  string
		sub  Subst
	}{
		{"a value followed by a stray word", `write h file /x --data $1 b`, Subst{Pos: []string{"v"}}},
		{"a value flag followed by a stray word", `read h:1 --tail $1 extra`, Subst{Pos: []string{"5"}}},
		{"a payload that is not one reference", `run h:1 $1 $2`, Subst{Pos: []string{"ls", "pwd"}}},
		{"a glued value", `write h file /x --data a$1`, Subst{Pos: []string{"v"}}},
		{"a number that is not one", `key h:1 ctrl+c --repeat $1`, Subst{Pos: []string{"abc"}}},
		{"an enum that is not one", `read h:1 --stream $1`, Subst{Pos: []string{"nope"}}},
		{"a geometry that is not one", `open h --size $1`, Subst{Pos: []string{"nope"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseWithSubst(c.src, c.sub)
			if err == nil {
				t.Fatalf("%s was accepted", c.src)
			}
			if strings.Contains(err.Error(), "__hole_") {
				t.Errorf("the refusal quotes the substitution marker:\n%v", err)
			}
		})
	}
}

// flagRef returns the `$N` a probe command uses for one flag, or "" when it does not set
// it. The command is the source of truth for which reference belongs to which flag, so
// the two cannot drift apart.
func flagRef(cmd, flag string) string {
	fields := strings.Fields(cmd)
	for i, field := range fields {
		if ref, ok := strings.CutPrefix(field, "--"+flag+"="); ok {
			return ref
		}
		// The space-separated spelling too: `--name $1` is two fields, and a reader that
		// only knew the `=` form silently skipped every flag in this file.
		if field == "--"+flag && i+1 < len(fields) && strings.HasPrefix(fields[i+1], "$") {
			return fields[i+1]
		}
	}
	return ""
}

// substFor builds a Subst with the probe's own value at the given reference and a value
// its domain accepts everywhere else, so one flag can be varied while the rest of the
// command stays parseable.
//
// A FIXED FILLER DOES NOT WORK, and the first version of this file proved it: filling
// every other reference with "x" made `--rows x` and `--stream x` fail, so the test
// reported the flag under measurement as refused when the refusal was about its
// neighbour. The filler has to satisfy the DECLARATION, which is the same table the
// parser reads.
func substFor(cmd, opName, ref, value string) Subst {
	op, _ := Lookup(opName)
	// Map each `$N` in the command to the flag it fills, so the filler can be chosen by
	// that flag's declared domain.
	byRef := map[string]string{}
	for _, name := range op.FlagNames() {
		if r := flagRef(cmd, name); r != "" {
			byRef[r] = name
		}
	}
	// The bound is read off the command, not fixed: `create host` names 33 references
	// and a fixed 12 made every value past the twelfth "an arg nothing references".
	args := make([]string, 0, 40)
	for n := 1; n <= maxRef(cmd); n++ {
		r := fmt.Sprintf("${%d}", n)
		if !strings.Contains(cmd, r) {
			continue
		}
		if r == ref {
			args = append(args, value)
			continue
		}
		filler := "x"
		if name, ok := byRef[r]; ok {
			if _, domain, _ := flagDomain(op, name); domain != "" {
				filler, _ = domainSamples(domain)
			}
		}
		args = append(args, filler)
	}
	return Subst{Pos: args}
}

// maxRef is the highest `$N` a command names.
func maxRef(cmd string) int {
	max := 0
	for _, m := range regexp.MustCompile(`\$\{(\d+)\}`).FindAllStringSubmatch(cmd, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

// domainSamples returns one value the domain accepts and one it refuses.
func domainSamples(domain string) (good, bad string) {
	switch {
	case strings.HasPrefix(domain, "enum="):
		choices := strings.Split(strings.TrimPrefix(domain, "enum="), ",")
		return choices[0], "not-one-of-those"
	case domain == "geometry":
		return "40x120", "not-a-geometry"
	default: // number
		return "3", "not-a-number"
	}
}

// isValueless reports whether a flag is declared as taking no value, so a probe can skip
// it: `--sticky` and `--json` are switches, and "what does its value become" has no
// answer for them.
func isValueless(op Op, name string) bool {
	for _, v := range op.Valueless {
		if v == name {
			return true
		}
	}
	return false
}
