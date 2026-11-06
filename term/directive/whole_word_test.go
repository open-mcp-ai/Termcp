package directive

import (
	"strings"
	"testing"
)

// TestAReferenceIsAWholeWord pins the rule cmd rests on: a reference is the whole word
// it sits in, and a word built from a reference and a literal is refused.
//
// Each case below used to be ACCEPTED and silently assembled — `/tmp/$1` became a path,
// `a$1` became a value — which is the shell's concatenation rule arriving in a language
// that declares itself not to have one. The reply was 200 either way, so the caller had
// no way to learn which reading it got.
func TestAReferenceIsAWholeWord(t *testing.T) {
	cases := []struct{ name, src string }{
		{"a glued prefix in a path", `write h file /tmp/$1`},
		{"a glued prefix in a value", `write h file /x --data a$1`},
		{"a glued suffix in a value", `write h file /x --data $1.bak`},
		{"a reference inside arithmetic", `write h file /x --data $(( $1 + 1 ))`},
		{"a reference glued in a positional", `create profile --host $1.example`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseWithSubst(c.src, Subst{Pos: []string{"x"}})
			if err == nil {
				t.Fatalf("%s was accepted; the value was glued to a literal in silence", c.src)
			}
			// The refusal quotes what the CALLER wrote. Echoing the internal marker
			// (`__hole_<seed>_0__`) tells them their input contains something it does
			// not, which turns a teaching error into a puzzle.
			if strings.Contains(err.Error(), "__hole_") {
				t.Errorf("the refusal leaks the substitution marker:\n%v", err)
			}
			if !strings.Contains(err.Error(), "mixes a value with shell text") {
				t.Errorf("the refusal does not name the shape:\n%v", err)
			}
			// And it has to carry the way out, or the next attempt is another guess.
			if !strings.Contains(err.Error(), "args:") {
				t.Errorf("the refusal does not show the spelling that works:\n%v", err)
			}
		})
	}
}

// TestAWholeWordReferenceStillReachesEveryPosition is the other half: the rule refuses
// concatenation, not references. A whole-word reference has to work everywhere a value
// can go — and the capability it replaces is intact, because the whole thing travels as
// the value.
func TestAWholeWordReferenceStillReachesEveryPosition(t *testing.T) {
	cases := []struct {
		name, src, sub string
		check          func(t *testing.T, st Statement)
	}{
		{"a path", `write h file $1`, "/tmp/x", func(t *testing.T, st Statement) {
			if st.Key != "/tmp/x" {
				t.Errorf("path = %q", st.Key)
			}
		}},
		{"a flag value", `write h file /x --data $1`, "a b", func(t *testing.T, st Statement) {
			if v, _ := st.Flag("data"); v != "a b" {
				t.Errorf("data = %q, want the value with its space intact", v)
			}
		}},
		{"a payload", `run h:1 $1`, "ls -la | wc -l", func(t *testing.T, st Statement) {
			if st.Payload != "ls -la | wc -l" {
				t.Errorf("payload = %q", st.Payload)
			}
		}},
		{"a positional", `key h:1 $1`, "ctrl+c", func(t *testing.T, st Statement) {
			if len(st.Args) != 1 || st.Args[0] != "ctrl+c" {
				t.Errorf("args = %v", st.Args)
			}
		}},
		{"a word in a word list", `open h --argv sh -c $1`, "make test", func(t *testing.T, st Statement) {
			if v, _ := st.Flag("argv"); !strings.Contains(v, "make test") {
				t.Errorf("argv = %q", v)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.src, Subst{Pos: []string{c.sub}})
			if err != nil {
				t.Fatalf("%s must parse: %v", c.src, err)
			}
			c.check(t, f.Statements[0])
		})
	}
}

// TestAFrameIsNotAWord pins the one declared exception: a directive whose payload
// reaches a protocol peer as a FRAME never passes through a shell, so a reference
// inside a template is the peer's business. The exception is read from the declaration
// (PayloadSpan: spanVerbatim), not from a list of verbs.
func TestAFrameIsNotAWord(t *testing.T) {
	f, err := ParseWithSubst(`call s cdp '{"id":$1}'`, Subst{Pos: []string{"7"}})
	if err != nil {
		t.Fatalf("a frame template is not a word: %v", err)
	}
	if got := f.Statements[0].Payload; !strings.Contains(got, "7") {
		t.Errorf("payload = %q, want the value filled in", got)
	}
}
