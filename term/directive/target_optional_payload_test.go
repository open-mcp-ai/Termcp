package directive

import "testing"

// TestAnOptionalTargetNeverStealsThePayload pins #296.
//
// `TargetOptional` was read in exactly two branches of the target rule — "no words
// left" and "the first word is a flag" — and never in the branch that takes a word AS
// the target. So an embedder writing `deploy [<env>] $1` got `deploy $1` parsed as a
// deploy to an environment named "make release", with an empty payload: success, no
// error, and the value in the wrong field. It is the same class as a silent `$1`
// substitution — a wrong answer that looks like a right one.
//
// The rule this restores already existed one block below, where a lone reference in a
// payload verb is kept as a value (`call h $1`); the target had consumed the word
// before it could run.
//
// The target is only skipped when it is OPTIONAL. `deploy prod $1` must still be a
// deploy to `prod`, which is the regression this test is also here for.
func TestAnOptionalTargetNeverStealsThePayload(t *testing.T) {
	deployGrammar := func(optional bool) *Grammar {
		return NewGrammar([]Op{{
			Name: "deploy", Kind: KindPayload, Target: true, TargetOptional: optional,
			Usage: "deploy [<env>] $1",
		}})
	}
	cases := []struct {
		name     string
		optional bool
		cmd      string
		args     []string
		target   string
		payload  string
		wantErr  bool
	}{
		{
			name: "optional target, lone reference is the payload", optional: true,
			cmd: "deploy $1", args: []string{"make release"},
			target: "", payload: "make release",
		},
		{
			name: "optional target, named target keeps the reference as payload", optional: true,
			cmd: "deploy prod $1", args: []string{"make release"},
			target: "prod", payload: "make release",
		},
		{
			name: "optional target, a literal payload is still not a target", optional: true,
			cmd:    "deploy make release",
			target: "make", payload: "release",
		},
		{
			name: "required target keeps its reading", optional: false,
			cmd: "deploy $1", args: []string{"prod"},
			target: "prod", payload: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseGrammar(tc.cmd, Subst{Pos: tc.args}, deployGrammar(tc.optional))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted", tc.cmd)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGrammar(%q): %v", tc.cmd, err)
			}
			st := f.Statements[0]
			if st.Target != tc.target || st.Payload != tc.payload {
				t.Errorf("%q -> target=%q payload=%q, want target=%q payload=%q",
					tc.cmd, st.Target, st.Payload, tc.target, tc.payload)
			}
		})
	}
}

// TestAnOptionalTargetStillTakesTheFlagForms is the other half of the same field, and
// it must not move.
//
// `TargetOptional` exists for `watch --off <rule>` / `watch --list`: forms with no
// object to name. Those go through a different branch of the same rule, which is why
// they kept working while the positional form did not.
func TestAnOptionalTargetStillTakesTheFlagForms(t *testing.T) {
	g := NewGrammar([]Op{{
		Name: "watch", Kind: KindPayload, Target: true, TargetOptional: true,
		Flags: []string{"off", "list"}, Usage: "watch [<rule>] [--off|--list]",
	}})
	for _, cmd := range []string{"watch --list", "watch --off rule-a"} {
		f, err := ParseGrammar(cmd, Subst{}, g)
		if err != nil {
			t.Fatalf("ParseGrammar(%q): %v", cmd, err)
		}
		if st := f.Statements[0]; st.Target != "" {
			t.Errorf("%q took a target (%q); the flag forms name no object", cmd, st.Target)
		}
	}
}
