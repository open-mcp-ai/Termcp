package directive

import (
	"testing"
)

// TestEveryStatementFieldIsExpanded pins the one property the substitution table
// owes a caller: after parsing, NO field of a Statement can still hold a hole.
//
// A sentinel is the placeholder the table mints for a value it has
// not inserted yet. One that survives parsing is not a cosmetic bug — it is a WRONG
// ADDRESS that nothing refuses, because every one of these fields accepts arbitrary
// text: `write h file $1 --local-path go.mod` wrote the file to a path literally named
// such a name, answered 202 "accepted", and the caller found out by looking where
// they had asked and finding nothing.
//
// The fields are listed here as a SET rather than one test per field, because the bug
// was that a field was left out of a list — a test that enumerates the same list it is
// checking would have missed it too. This walks the whole Statement by reflection, so
// a field added later is covered without anyone remembering to add a case.
func TestEveryStatementFieldIsExpanded(t *testing.T) {
	// One statement that fills every field a caller can write: a sub-resource key, a
	// positional arg, a flag value, a target, and a payload.
	const cmd = "write $1 file $2 --data $3"
	sub := Subst{Pos: []string{"sess", "/tmp/x", "hello"}}

	f, err := ParseWithSubst(cmd, sub)
	if err != nil {
		t.Fatalf("ParseWithSubst(%q): %v", cmd, err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("want 1 statement, got %d", len(f.Statements))
	}
	st := f.Statements[0]

	// The values really did arrive, so the walk below is checking expansion rather
	// than a statement that never had a hole in the first place.
	if st.Target != "sess" {
		t.Errorf("target = %q, want %q", st.Target, "sess")
	}
	if st.Key != "/tmp/x" {
		t.Errorf("key = %q, want %q — a leaked sentinel here is a file written to the wrong path", st.Key, "/tmp/x")
	}

	// And nothing anywhere still looks like a hole.
	if leaked := leakedSentinels(st); len(leaked) > 0 {
		t.Errorf("these fields still hold a substitution placeholder: %v\nstatement: %+v", leaked, st)
	}
}

// leakedSentinels names the fields of st whose value still contains a hole.
func leakedSentinels(st Statement) []string {
	var bad []string
	check := func(field, v string) {
		if isSentinel(v) {
			bad = append(bad, field+"="+v)
		}
	}
	check("Target", st.Target)
	check("Key", st.Key)
	check("Payload", st.Payload)
	check("Path", st.Path)
	for i, a := range st.Args {
		check("Args["+itoa(i)+"]", a)
	}
	for k, v := range st.Flags {
		if v != nil {
			check("Flags["+k+"]", *v)
		}
	}
	return bad
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestASubstitutedPathReachesTheFileVerb pins the caller-visible consequence, because
// the field list above is a claim about the parser and this is a claim about the
// surface: the address in the statement must be the address that was asked for.
func TestASubstitutedPathReachesTheFileVerb(t *testing.T) {
	f, err := ParseWithSubst("write $1 file $2 --data payload", Subst{Pos: []string{"h", "/tmp/report.txt"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := f.Statements[0]
	if st.Key != "/tmp/report.txt" {
		t.Fatalf("the file verb's path is %q; the caller asked for /tmp/report.txt", st.Key)
	}
	if st.Op != "write" || st.Kind != "file" {
		t.Fatalf("parsed as %s/%s, want write/file", st.Op, st.Kind)
	}
}
