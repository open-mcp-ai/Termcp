package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestEveryDirectiveHasADispatcher pins that the parser's vocabulary and the
// runtime's dispatcher agree on the set of directives.
//
// The directive table lives in internal/directive and cannot name internal/mcp
// handlers without a cyclic import, so `runDirective` dispatches via a switch. That
// split creates a failure mode: someone adds a directive to the table and forgets the
// switch, and the new directive parses cleanly and fails at dispatch with
// "unknown directive: <name>".
//
// This test exercises every entry in directive.Ops through runDirective, expecting
// anything EXCEPT "unknown directive".
func TestEveryDirectiveHasADispatcher(t *testing.T) {
	s := newTestServer(t)
	for _, op := range directive.Ops() {
		t.Run(op.Name, func(t *testing.T) {
			stmt := directive.Statement{
				Op:   op.Name,
				Path: op.Name,
			}
			slots := s.runDirective(context.Background(), stmt, termArgs{})
			if len(slots) == 0 {
				t.Fatalf("%s produced zero slots", op.Name)
			}
			body := slots[0].Body
			if strings.Contains(body, "unknown directive: "+op.Name) {
				t.Errorf("directive %q is in directive.Ops but missing from runDirective's switch", op.Name)
			}
		})
	}
}
