package directive

import (
	"strings"
	"testing"
)

// TestEscapingFollowsTheDestination pins the rule that decides whether a value is
// escaped: WHO READS IT, not whether the caller wrote quotes.
//
// The reported failure was a path. `write <session> file "$1"` — the ordinary instinct
// to quote a path — answered 202 and created a file whose NAME carried the escape
// sequences (`C:\\Users\\...\\asked.bin`), because the double quotes in the source were
// read as "escape this for a shell" when no shell was ever going to read it. The
// unquoted spelling had been right all along, which is the shape of the bug: two
// spellings of one intent, one of them silently corrupt.
//
// A shell payload is the case the escaping exists for and it keeps it: the quotes reach
// the shell, so the value has to survive that unquoting.
func TestEscapingFollowsTheDestination(t *testing.T) {
	const value = `/tmp/a"b$c.txt`

	cases := []struct {
		name string
		cmd  string
		// read pulls the field under test out of the statement.
		read func(Statement) string
		want string
		why  string
	}{
		{
			name: "an unquoted path is the value",
			cmd:  "write h file $1",
			read: func(st Statement) string { return st.Key },
			want: value,
			why:  "a path is bytes, and nothing will unquote it",
		},
		{
			name: "a quoted path is THE SAME value",
			cmd:  `write h file "$1"`,
			read: func(st Statement) string { return st.Key },
			want: value,
			why:  "quoting is the caller's habit; it must not add backslashes to a path",
		},
		{
			name: "a quoted flag value is the value",
			cmd:  `write h file /tmp/x --data "$1"`,
			read: func(st Statement) string { v, _ := st.Flag("data"); return v },
			want: value,
			why:  "a flag value arrives as the bytes between the flags",
		},
		{
			name: "an unquoted shell payload is the statement itself",
			cmd:  "run h:1 $1",
			read: func(st Statement) string { return st.Payload },
			want: `/tmp/a"b$c.txt`,
			why:  "the value IS the shell line, so it is inserted as written",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: []string{value}})
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", tc.cmd, err)
			}
			if got := tc.read(f.Statements[0]); got != tc.want {
				t.Errorf("%s\ngot  %q\nwant %q", tc.why, got, tc.want)
			}
		})
	}
}

// TestAnEscapedPathIsNeverProducedForANonShellDestination is the same claim as a
// property, because the failure mode was a silent one: the command SUCCEEDED.
//
// A path that carries a backslash the caller did not write is not a syntax error and
// not a refusal — it is a different file, created in the wrong place. Nothing
// downstream can catch it, so it has to be caught here.
func TestAnEscapedPathIsNeverProducedForANonShellDestination(t *testing.T) {
	const value = `D:\a b\c.txt`
	for _, cmd := range []string{"write h file $1", `write h file "$1"`, "get h file $1", "rm h file $1"} {
		f, err := ParseWithSubst(cmd, Subst{Pos: []string{value}})
		if err != nil {
			t.Fatalf("ParseWithSubst(%q): %v", cmd, err)
		}
		if got := f.Statements[0].Key; got != value {
			t.Errorf("%q: key = %q, want %q — a destination is the caller's bytes", cmd, got, value)
		}
		if strings.Contains(f.Statements[0].Key, `\`) != strings.Contains(value, `\`) {
			t.Errorf("%q: the backslashes in %q are not the caller's", cmd, value)
		}
	}
}
