package mcp

import (
	"strings"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// dryRunSlots assembles preview-only slots for a list of statements (#241).
//
// IT CONNECTS TO NOTHING AND SENDS NO BYTES. The statements passed here have
// already survived AST parsing, $1/$NAME substitution and shell-control checks.
// What remains is alias expansion and syntax presentation.
//
// Hard rules (from issue #241 §4):
//
//  1. Zero side effects: no dial (including `use` hosts), no stdin, no audit span.
//  2. No synthesized output: never emit Content-Range, Input-At, Exit-Code, Waited,
//     Sampled, or dummy output bodies.
//  3. Expansion stops at termcp: shell wildcards/globbing are left untouched.
//  4. Secrets are NEVER echoed in Expanded: replace each known secret value with
//     its $[NAME] placeholder.
func (s *Server) dryRunSlots(stmts []directive.Statement, win termArgs) []envelope.Slot {
	out := make([]envelope.Slot, 0, len(stmts))
	for _, stmt := range stmts {
		expanded, cycle, expansionErr := aliases.Expand(stmt)
		if expansionErr != nil {
			slot := s.parseErrorSlot(expansionErr)
			if slot.Headers == nil {
				slot.Headers = map[string]string{}
			}
			slot.Headers["Dry-Run"] = "true"
			slot.Headers["Verdict"] = "invalid"
			out = append(out, slot)
			continue
		}
		if cycle != nil {
			slot := aliasCycleSlot(stmt, cycle)
			if slot.Headers == nil {
				slot.Headers = map[string]string{}
			}
			slot.Headers["Dry-Run"] = "true"
			slot.Headers["Verdict"] = "invalid"
			out = append(out, slot)
			continue
		}

		for _, expanded := range expanded {
			headers := map[string]string{
				"Dry-Run": "true",
				"Verdict": "valid-syntax",
				// PATH IS MASKED TOO, AND THAT IS NOT REDUNDANT. The parser expands
				// the whole line, echo included (subst.go sets `Path` from the same
				// table that fills the payload), so `input s1:1 $PASS` reaches here
				// with the credential ALREADY WRITTEN INTO the echo. Masking only the
				// payload would leak through the more prominent header — measured, not
				// theorised (#241 §4.4).
				"Path": maskSecrets(expanded.Path, win.Secrets),
			}
			if expanded.Target != "" {
				headers["Target"] = maskSecrets(expanded.Target, win.Secrets)
			}
			if expanded.Payload != "" {
				headers["Expanded"] = maskSecrets(expanded.Payload, win.Secrets)
			}
			out = append(out, envelope.Slot{
				Code:    200,
				Headers: headers,
			})
		}
	}
	return out
}

// maskSecrets prevents credential leakage in dry-run's Expanded: header (#241 §4.4).
func maskSecrets(text string, secrets map[string]string) string {
	if len(secrets) == 0 {
		return text
	}
	out := text
	for k, v := range secrets {
		if v != "" {
			out = strings.ReplaceAll(out, v, "$"+k)
		}
	}
	return out
}
