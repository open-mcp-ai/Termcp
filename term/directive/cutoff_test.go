package directive

import (
	"strings"
	"testing"
)

// TestAValueThatWasCutOffIsRefused pins the three shapes where a caller's bytes used to
// stop meaning what they wrote.
//
// They are one defect: the words after a directive are read by two grammars at once, and
// wherever the shell grammar takes over without saying so, the rest of the value is
// dropped. Every case below answered 200 before this, with no error anywhere.
func TestAValueThatWasCutOffIsRefused(t *testing.T) {
	cases := []struct{ name, src, want string }{
		// The shell reads `>b` as a redirect, so the value became "a".
		{"a redirect", `write s file /x --data a>b`, "does not take a redirect"},
		// `b` is a second word: the value is "a" and `b` became a positional nobody read.
		{"a second word", `write s file /x --data a b`, "is a separate word after it"},
		{"a second word after another flag", `read s file /x --tail 5 extra`, "is a separate word after it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.src)
			if err == nil {
				t.Fatalf("%s was accepted; the rest of the value is dropped in silence", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: the refusal does not name what happened:\n%v", c.src, err)
			}
			// The refusal has to carry the fix, because the caller's next attempt is
			// otherwise another guess.
			if !strings.Contains(err.Error(), "--data") && !strings.Contains(err.Error(), "--tail") {
				t.Errorf("%s: the refusal does not name the flag:\n%v", c.src, err)
			}
		})
	}
}

// TestTheLegalSpellingsStillParse is the other half: a rule that refuses a cut-off value
// must not refuse the ways of writing a value that is not cut off.
func TestTheLegalSpellingsStillParse(t *testing.T) {
	for _, src := range []string{
		`write s file /x --data 'a;b'`,            // quoted: one word, and the `;` is inside it
		`write s file /x --data "a b" --mode hex`, // quoted value, then another flag
		`write s file /x --data=a --mode hex`,     // `=` spells the value, then another flag
		`key s:1 ctrl+c --repeat 2`,               // a bare word that IS this verb's argument
		`help read`,                               // the same, for help
		`read s file /x --tail 5`,                 // a value flag with nothing after it
		`notify --for 5s the body`,                // a payload verb: the word after the flag is its payload
		`run h:1 $1`,                              // a payload that is one reference
	} {
		if _, err := Parse(src); err != nil {
			t.Errorf("%s must parse: %v", src, err)
		}
	}
}

// TestTheEqualsSpellingIsNotACutOff: `--data=a b` writes the value with `=` and leaves
// `b`, which is the same truncation by another spelling.
func TestTheEqualsSpellingIsNotACutOff(t *testing.T) {
	if _, err := Parse(`write s file /x --data=a b`); err == nil {
		t.Error("`--data=a b` was accepted and `b` dropped")
	}
}
