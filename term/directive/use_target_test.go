package directive

import (
	"testing"
)

// TestUseHostHostTargetParsesPinsIssue237 pins the spellings a short-circuit target
// may take, and — the part that is easy to get wrong — that the payload region still
// starts at the payload.
//
// TWO WALKS READ A STATEMENT: parseStatement decides the grammar, and analyzeSource
// decides which reference is a value. They must consume the SAME words. `use <host>`
// is one target built from two words, and when only the first walk folded it, the
// second left `internal` in place and read `$1` as shell text glued to a literal —
// `run use internal $1` was refused with "a variable cannot sit inside a shell line".
func TestUseHostHostTargetParsesPinsIssue237(t *testing.T) {
	cases := []struct {
		src    string
		target string
		kind   string
		key    string
		args   []string
	}{
		{src: "run use internal $1", target: "use internal", args: []string{"echo hi"}},
		{src: "run use:internal $1", target: "use:internal", args: []string{"echo hi"}},
		{src: "run `use internal` $1", target: "`use internal`", args: []string{"echo hi"}},
		{src: "run internal $1", target: "internal", args: []string{"echo hi"}},
		{src: "read use internal file /etc/hosts", target: "use internal", kind: "file", key: "/etc/hosts"},
		{src: "write prod file /tmp/x --data hi", target: "prod", kind: "file", key: "/tmp/x"},
		{src: "get use internal file /tmp/x", target: "use internal", kind: "file", key: "/tmp/x"},
		{src: "rm use internal file /tmp/x", target: "use internal", kind: "file", key: "/tmp/x"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			// The substitution is the half that failed: a payload that is one
			// reference must arrive as the caller's value, unchanged.
			f, err := ParseWithSubst(tc.src, Subst{Pos: tc.args})
			if err != nil {
				t.Fatalf("%q: %v", tc.src, err)
			}
			if len(f.Statements) != 1 {
				t.Fatalf("%q produced %d statements", tc.src, len(f.Statements))
			}
			st := f.Statements[0]
			if st.Target != tc.target {
				t.Errorf("target = %q, want %q", st.Target, tc.target)
			}
			if tc.kind != "" && st.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", st.Kind, tc.kind)
			}
			if tc.key != "" && st.Key != tc.key {
				t.Errorf("key = %q, want %q", st.Key, tc.key)
			}
			if st.Op == "run" && st.Payload != "echo hi" {
				t.Errorf("payload = %q, want the value as written", st.Payload)
			}
		})
	}

	// `use` with no host is refused by name, not silently read as a host called "use".
	if _, err := Parse("run use $1"); err == nil {
		t.Fatalf("`run use $1` should have been refused")
	}
}
