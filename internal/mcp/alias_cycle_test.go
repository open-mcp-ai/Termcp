package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// A cycle has to be reported AS a cycle. Both names are defined, so "unknown
// directive: aa" sends the caller hunting for a typo in a directive that exists —
// and the only way to make a cycle is through the alias table.
func TestAliasCycleIsReportedAsACycle(t *testing.T) {
	a := newAliasTableForTest(map[string]string{"aa": "bb", "bb": "aa"})
	path := a.cyclePath("aa")
	if len(path) == 0 {
		t.Fatal("a two-name cycle was not detected")
	}
	joined := strings.Join(path, " ")
	if !strings.Contains(joined, "aa") || !strings.Contains(joined, "bb") {
		t.Fatalf("cycle path = %v, want both names", path)
	}
}

func TestAliasSelfReferenceIsACycle(t *testing.T) {
	a := newAliasTableForTest(map[string]string{"self": "self"})
	if path := a.cyclePath("self"); len(path) == 0 {
		t.Fatal("a self-referencing alias was not reported as a cycle")
	}
}

// A chain that resolves is NOT a cycle, and cyclePath must not claim otherwise.
func TestAliasChainWithoutCycleHasNoCyclePath(t *testing.T) {
	a := newAliasTableForTest(map[string]string{"mx": "ls --all"})
	if path := a.cyclePath("mx"); len(path) != 0 {
		t.Fatalf("cyclePath = %v, want none", path)
	}
}

// THE PARSE-ERROR DOOR IS THE ONE THE CALLER USUALLY ARRIVES AT: an invented
// directive fails parsing, and the alias is resolved from that error. Reporting a
// cycle only from Expand() left this path still saying "unknown directive".
func TestAliasCycleIsReportedThroughTheParseError(t *testing.T) {
	a := newAliasTableForTest(map[string]string{"aa": "bb", "bb": "aa"})
	_, err := directive.Parse("aa")
	if err == nil {
		t.Fatal("expected the parser to reject an invented directive")
	}
	stmt, resolved, cycle := a.expandErrorCycle(err)
	if resolved {
		t.Fatalf("a cyclic chain resolved to %q", stmt.Op)
	}
	if len(cycle) == 0 {
		t.Fatal("expandErrorCycle reported no cycle")
	}
}

// An unrelated parse error must not be turned into a cycle report.
func TestUnresolvableNameIsNotACycle(t *testing.T) {
	a := newAliasTableForTest(map[string]string{})
	_, err := directive.Parse("nosuchdirective")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if _, resolved, cycle := a.expandErrorCycle(err); resolved || len(cycle) != 0 {
		t.Fatalf("resolved=%v cycle=%v, want neither for a name that is not an alias", resolved, cycle)
	}
}
