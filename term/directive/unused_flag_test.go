package directive

import (
	"os"
	"strings"
	"testing"
)

// TestEveryDeclaredFlagIsReadOrRefused pins invariant A from the design review:
//
//	A parameter that is accepted must either change behaviour or be refused.
//	The third path — accepted and ignored — does not exist.
//
// The failure mode is specific and dangerous: an ignored parameter produces a
// reply that is a CONFIDENTLY CORRECT ANSWER ABOUT SOMETHING ELSE. `detect <target>`
// answered 200 with the local shell; `close <handle> --force` answered 204 while
// tearing down the whole session. Both looked authoritative.
//
// This test is deliberately textual: it reads the directive table and the handlers
// and fails when a declared flag name appears in neither. It cannot prove the flag
// CHANGES anything, but it catches the common case — declared, validated, forgotten.
func TestEveryDeclaredFlagIsReadOrRefused(t *testing.T) {
	sources, err := collectGoSources("../../internal/mcp")
	if err != nil {
		t.Fatalf("collect sources: %v", err)
	}
	all := strings.Join(sources, "\n")

	for _, op := range Ops() {
		for _, declared := range op.Flags {
			// The declared entry may carry its domain ("offset:number"); the handler
			// reads the NAME, so the annotation is stripped here rather than the
			// declaration being kept in two lists.
			flag, _, _ := strings.Cut(declared, ":")
			// Both spellings reach the handlers: `grace` as a directive flag, and
			// `grace` / `grace-seconds` as a tool argument.
			camel := toCamel(flag)
			snake := strings.ReplaceAll(flag, "-", "_")
			// Only spellings that NAME this flag count. A bare `"flag"` would match
			// any string in the package and make the test vacuous — which it was,
			// before this list was narrowed.
			considered := []string{
				"Flag(\"" + flag + "\")",
				"FlagState(\"" + flag + "\")",
				"boolFlag(stmt, \"" + flag + "\")",
				"\"" + flag + "\", \"" + snake + "\"",
				"\"" + flag + "\", \"" + camel + "\"",
				"\"" + snake + "\"",
			}
			found := false
			for _, needle := range considered {
				if strings.Contains(all, needle) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("directive %q declares --%s but no handler reads or refuses it", op.Name, flag)
			}
		}
	}
}

// collectGoSources reads every non-test .go file under dir.
func collectGoSources(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, string(b))
	}
	return out, nil
}

// toCamel turns a flag name into the argument name it usually maps to.
func toCamel(flag string) string {
	parts := strings.Split(flag, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}
