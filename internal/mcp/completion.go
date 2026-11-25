package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
)

// COMPLETION: completion/complete answers argument suggestions for this server's
// prompts and tool arguments.
//
// WHY IT IS WORTH THE CODE. A client that supports completion offers the caller a
// picker while it types, and the picker is built from the words the server actually
// has. Without it, a model writing a `cmd` line guesses at verb names and spellings —
// this repository's own issue #356 is a measurement of what that guessing costs (6 of
// 6 `call … tools` attempts failed because the word was in the wrong layer). The
// vocabulary already exists in `term/directive`; this is a lookup, not a second table.
//
// IT NEVER FAILS. The specification is explicit: an unknown reference or an argument
// this server does not complete answers with an EMPTY list, not an error. A client
// that asks about something else gets nothing back and shows no suggestions, which is
// exactly right — an error there would surface in a person's editor as a broken editor.
//
// WHAT IT DOES NOT PRETEND TO KNOW. It completes VERBS, because verbs are a closed
// vocabulary this build owns. It does not complete a session id, a path or a port: those
// are values of a running instance, they change between calls, and a stale suggestion
// is worse than none — the caller would paste an address that no longer resolves,
// which is the one mistake #356 counted fourteen times.

// completionRequest is the params of completion/complete.
type completionRequest struct {
	Ref struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"ref"`
	Argument struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"argument"`
}

// completionResult is the answer: a bounded list of candidate values.
type completionResult struct {
	Completion completionValues `json:"completion"`
}

type completionValues struct {
	Values  []string `json:"values"`
	Total   int      `json:"total,omitempty"`
	HasMore bool     `json:"hasMore,omitempty"`
}

// maxCompletions bounds one answer. A picker with a thousand rows is not a picker; the
// prefix is what narrows it, and a caller that typed one letter gets the first slice.
const maxCompletions = 100

// handleCompletion answers completion/complete.
func (s *Server) handleCompletion(_ context.Context, params json.RawMessage) (any, *mcp.RPCError) {
	var req completionRequest
	if len(params) > 0 {
		// A malformed params object is answered EMPTY rather than with an error, for the
		// same reason as everything else in this file: a picker that reports a protocol
		// failure in the middle of someone's typing teaches them the editor is broken.
		if err := json.Unmarshal(params, &req); err != nil {
			return completionResult{}, nil
		}
	}
	values := s.completeValues(req)
	return completionResult{Completion: completionValues{
		Values:  values,
		Total:   len(values),
		HasMore: false,
	}}, nil
}

// completeValues is the lookup, kept separate so the request envelope and the
// vocabulary cannot get tangled.
func (s *Server) completeValues(req completionRequest) []string {
	// THE ARGUMENT NAME IS WHAT DECIDES, not the reference: `cmd` is the only argument
	// whose value is a directive line, and it is the same argument whether the client
	// asks about the tool or about a prompt.
	if req.Argument.Name != "cmd" {
		return nil
	}
	prefix := strings.TrimSpace(req.Argument.Value)
	// ONLY THE FIRST WORD IS COMPLETED. `read a1f3:1 --ta` is a flag position with a
	// session in it, and completing that needs the state of a running session — see the
	// note on stale values above. Dotting the line out of scope is honest; guessing is
	// what this refuses.
	if strings.ContainsAny(prefix, " \t;|&") {
		return nil
	}
	out := make([]string, 0, maxCompletions)
	for _, name := range directive.DefaultGrammar.Names() {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, name)
		if len(out) >= maxCompletions {
			break
		}
	}
	sort.Strings(out)
	return out
}
