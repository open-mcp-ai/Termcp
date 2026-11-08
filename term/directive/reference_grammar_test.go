package directive

import (
	"fmt"
	"strings"
	"testing"
)

// A REFERENCE FILLS A VALUE. IT NEVER BECOMES PART OF THE GRAMMAR.
//
// The parser decides a statement's shape from the words it can SEE, and a reference
// is filled in afterwards. So a word that was meant to be a kind, a flag name or a
// channel index cannot become one — it falls through to a positional argument instead,
// where the verb may not read it at all. `close <session> $1` closed the whole SESSION
// rather than channel 2 that way and answered 204 (issue #199); `read $1 $2` read the
// session's default channel and answered 200 (issue #198).
//
// These tests pin the boundary from both sides: every value slot keeps working, and
// every grammar position is refused BY NAME instead of being reinterpreted.

func TestAReferenceNeverBecomesGrammar(t *testing.T) {
	cases := []struct {
		name, cmd string
		args      []string
	}{
		// The word right after the target is where a kind goes.
		{"kind", `call $1 $2 $3`, []string{"s", "mcp", `{}`}},
		// NOT `call $1 $2`: a lone reference after a payload verb IS the frame, which is
		// data (`call h $1` is the documented spelling). The ambiguous shape is the one
		// with a word in front of it, above.
		// ... and where a channel index goes, folded into the handle.
		{"channel index on a channel verb", `read s $1`, []string{"2"}},
		{"channel index on close", `close s $1`, []string{"2"}},
		{"channel index on rm", `rm $1 $2`, []string{"host", "prod"}},
		// A flag name is matched against the op's table before the value exists.
		{"flag name", `read s:1 cdp:1 $1 3`, []string{"--at"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseWithSubst(c.cmd, Subst{Pos: c.args})
			if err == nil {
				t.Fatalf("%s parsed; a reference reached a grammar position", c.cmd)
			}
			if !strings.Contains(err.Error(), "a reference fills a VALUE") {
				t.Fatalf("%s refused for the wrong reason:\n%v", c.cmd, err)
			}
		})
	}
}

// TestAReferenceStillReachesEveryValueSlot is the other half, and it is what keeps
// the boundary from being a removal: every position that carries DATA keeps working,
// including the whole address, which is the spelling a computed index should use.
func TestAReferenceStillReachesEveryValueSlot(t *testing.T) {
	cases := []struct {
		name, cmd string
		args      []string
		check     func(*testing.T, Statement)
	}{
		{"target", `read $1`, []string{"s:1"}, func(t *testing.T, st Statement) {
			if st.Target != "s:1" {
				t.Errorf("target = %q", st.Target)
			}
		}},
		{"the whole address, index included", `close $1`, []string{"s:2"}, func(t *testing.T, st Statement) {
			if st.Target != "s:2" {
				t.Errorf("target = %q", st.Target)
			}
		}},
		{"kind key", `read s:1 mcp $1`, []string{"1"}, func(t *testing.T, st Statement) {
			if st.Kind != "mcp" || st.Key != "1" {
				t.Errorf("kind=%q key=%q", st.Kind, st.Key)
			}
		}},
		{"flag value", `read s:1 cdp:1 --at $1`, []string{"4"}, func(t *testing.T, st Statement) {
			if v, _ := st.Flag("at"); v != "4" {
				t.Errorf("at = %q", v)
			}
		}},
		{"a frame", `call $1 mcp:1 $2`, []string{"s", `{"jsonrpc":"2.0"}`}, func(t *testing.T, st Statement) {
			if st.Payload != `{"jsonrpc":"2.0"}` {
				t.Errorf("payload = %q", st.Payload)
			}
		}},
		{"a lone frame after a channel handle", `call h:1 $1`, []string{`{"jsonrpc":"2.0"}`}, func(t *testing.T, st Statement) {
			if st.Payload != `{"jsonrpc":"2.0"}` {
				t.Errorf("payload = %q", st.Payload)
			}
		}},
		{"declared data words (resize)", `resize s:1 $1 $2`, []string{"24", "80"}, func(t *testing.T, st Statement) {
			if len(st.Args) != 2 || st.Args[0] != "24" || st.Args[1] != "80" {
				t.Errorf("args = %v", st.Args)
			}
		}},
		{"declared data word (get)", `get s:1 cdp:1 protocol $1`, []string{"Dom"}, func(t *testing.T, st Statement) {
			if len(st.Args) != 2 || st.Args[1] != "Dom" {
				t.Errorf("args = %v", st.Args)
			}
		}},
		{"declared data word (key name)", `key s:1 $1`, []string{"ctrl+c"}, func(t *testing.T, st Statement) {
			if len(st.Args) != 1 || st.Args[0] != "ctrl+c" {
				t.Errorf("args = %v", st.Args)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.cmd, Subst{Pos: c.args})
			if err != nil {
				t.Fatalf("%s must parse: %v", c.cmd, err)
			}
			c.check(t, f.Statements[0])
		})
	}
}

// TestEveryFlagOpRefusesAReferenceWhereAKindGoes sweeps the Op TABLE rather than a
// list of verbs, so a directive added later inherits the boundary instead of having
// to remember it. A flag verb has no payload, so a lone reference after its target
// cannot be anything but a word that failed to become a kind or an index.
func TestEveryFlagOpRefusesAReferenceWhereAKindGoes(t *testing.T) {
	for _, op := range Ops() {
		if op.Kind != KindFlags || len(op.Kinds) == 0 {
			continue
		}
		src := op.Name + " s:1 $1"
		if op.Target == false {
			src = op.Name + " $1" // no target to spend: the reference is the first word
		}
		if _, err := ParseWithSubst(src, Subst{Pos: []string{"2"}}); err == nil {
			t.Errorf("%s accepted a reference where a kind goes", src)
		}
	}
}

// TestTheDataWhitelistIsExplicit pins WHICH directives let a reference become a
// positional word. That list is the whole DSL boundary: growing it is a decision about
// what a caller's data may decide, so it should not be possible to grow it by accident.
func TestTheDataWhitelistIsExplicit(t *testing.T) {
	want := map[string]int{"get": 1, "key": 1, "resize": 2}
	for _, op := range Ops() {
		if got := op.DataArgs; got != want[op.Name] {
			t.Errorf("%s DataArgs = %d, want %d (the whitelist is a decision, not a default)",
				op.Name, got, want[op.Name])
		}
	}
}

// TestEveryPositionOnEveryDirectiveIsClassified is the sweep that keeps both halves of
// the rule honest, over the whole Op table rather than a hand-written list:
//
//	能用变量的地方不要漏掉   a DATA position accepts a reference, and the value lands
//	                         in a field that reaches the operation
//	不能用变量的地方不能放过 a GRAMMAR position refuses one by name, and never
//	                         absorbs it into a value slot
//
// A directive added later is classified by construction: it is probed at its target,
// its second word, its channel index, its kind, its kind's key and every declared flag
// — both as the flag's value and as the flag's name.
//
// Two outcomes are allowed and are not failures:
//
//	NOT APPLICABLE  the literal form does not parse either, so the template does not
//	                describe this directive and there is nothing to classify
//	ABSORBED        a lone reference after a payload verb IS that verb's payload
//	                (`call h $1` is the documented spelling for a frame)
func TestEveryPositionOnEveryDirectiveIsClassified(t *testing.T) {
	type probe struct {
		tmpl, val, where string
		data             bool
	}
	probesFor := func(op Op) []probe {
		var out []probe
		tgt := "s:1"
		if op.Target && op.TargetKind != TargetChain {
			tgt = "s"
		}
		head := op.Name
		if op.Target {
			head += " " + tgt
		}
		tail := ""
		if op.Kind == KindPayload {
			tail = " msg"
		}
		if op.Target {
			out = append(out, probe{op.Name + " %", "zztarget", "target", true})
		}
		if len(op.Kinds) > 0 {
			k := op.Kinds[0]
			out = append(out, probe{head + " %", k, "kind", false})
			out = append(out, probe{head + " " + k + " %", "zzkey", "kind-key", true})
		}
		if op.Target && op.TargetKind == TargetChain {
			out = append(out, probe{op.Name + " s %", "7", "channel-index", false})
		}
		valueless := map[string]bool{}
		for _, v := range op.Valueless {
			valueless[v] = true
		}
		for _, spec := range op.Flags {
			name := strings.SplitN(spec, ":", 2)[0]
			if valueless[name] {
				continue
			}
			sample := "zzval"
			if i := strings.IndexByte(spec, ':'); i >= 0 {
				switch d := spec[i+1:]; {
				case d == "number":
					sample = "7"
				case strings.HasPrefix(d, "enum="):
					sample = strings.Split(d[len("enum="):], ",")[0]
				case strings.Contains(d, "x") && !strings.Contains(d, "="):
					sample = "7x70"
				}
			}
			out = append(out, probe{head + " --" + name + " %" + tail, sample, "flag-value:" + name, true})
			out = append(out, probe{head + " % " + sample + tail, "--" + name, "flag-name:" + name, false})
		}
		return out
	}

	// whereIs reports every field of the filled statement that carries v.
	whereIs := func(st Statement, v string) []string {
		var hits []string
		if strings.Contains(st.Target, v) {
			hits = append(hits, "target")
		}
		if strings.Contains(st.Key, v) {
			hits = append(hits, "key")
		}
		if strings.Contains(st.Payload, v) {
			hits = append(hits, "payload")
		}
		if strings.Contains(st.Kind, v) {
			hits = append(hits, "KIND")
		}
		for n, fv := range st.Flags {
			if fv != nil && strings.Contains(*fv, v) {
				hits = append(hits, "flag:"+n)
			}
		}
		for i, a := range st.Args {
			if strings.Contains(a, v) {
				hits = append(hits, "arg["+fmt.Sprint(i)+"]")
			}
		}
		return hits
	}

	for _, op := range Ops() {
		for _, p := range probesFor(op) {
			name := op.Name + "/" + p.where
			// The literal spelling has to work for the probe to describe this
			// directive at all.
			if _, lerr := ParseWithSubst(strings.Replace(p.tmpl, "%", p.val, 1), Subst{}); lerr != nil {
				continue
			}
			refSrc := strings.Replace(p.tmpl, "%", "$1", 1)
			f, err := ParseWithSubst(refSrc, Subst{Pos: []string{p.val}})
			if err != nil {
				// A refusal is right for a grammar position; for a data position it is
				// the failure this sweep exists to catch.
				if !p.data {
					continue
				}
				t.Errorf("%s: a DATA position refused a reference: %v", name, err)
				continue
			}
			hits := whereIs(f.Statements[0], p.val)
			if len(hits) == 0 {
				t.Errorf("%s: the reference parsed but its value is nowhere in the statement", name)
				continue
			}
			if p.data {
				continue
			}
			// A grammar position must not accept a reference. The one exception is a
			// lone reference after a payload verb, which is that verb's payload.
			if op.Kind == KindPayload && len(hits) == 1 && hits[0] == "payload" {
				continue
			}
			t.Errorf("%s: a GRAMMAR position absorbed a reference into %s", name, strings.Join(hits, ","))
		}
	}
}

// TestAnUnreadPositionalWordIsRefused is the BACKSTOP under the reference boundary,
// and it is about literals: a verb that reads its object from the target and its detail
// from a kind has nothing left to do with a word, so a word there is a mistake.
//
// Ignoring it is not neutral. `close <session> 99` never reaches this path (the index
// folds into the address), but any word that fails to fold does, and a command whose
// subject was meant to be narrower than it is should not answer 204.
func TestAnUnreadPositionalWordIsRefused(t *testing.T) {
	refused := []string{
		`close s:1 88`, // the target already names a channel, so 88 cannot fold
		`ls s:1 88`,
		`read s:1 88`,
		`rm s 88`, // rm acts on the session; a channel handle is refused earlier
	}
	for _, cmd := range refused {
		if _, err := Parse(cmd); err == nil {
			t.Errorf("%s parsed; the word would be read by nobody", cmd)
		} else if !strings.Contains(err.Error(), "takes no positional words") &&
			!strings.Contains(err.Error(), "needs a SESSION") {
			t.Errorf("%s refused for the wrong reason: %v", cmd, err)
		}
	}

	// The folded spellings still work: the index belongs in the address, and writing
	// it as a second word IS how an address is written.
	for _, cmd := range []string{`close s 99`, `read s 2`, `ls s 1`} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%s must parse: %v", cmd, err)
		}
		if !strings.Contains(f.Statements[0].Target, ":") {
			t.Errorf("%s should fold into a channel handle, got target %q", cmd, f.Statements[0].Target)
		}
	}
}
