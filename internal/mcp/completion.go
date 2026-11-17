package mcp

// completion/complete, answered from the directive grammar (issue #327).
//
// THE VOCABULARY IS ALREADY IN THE BINARY, so this is a lookup rather than a second
// list of words to keep in step: `term/directive` holds every verb, and `--flag`
// summaries come from the same table the error messages do. What is completed here is
// the FIRST WORD of `cmd`, which is the verb — the one position where a completion is
// both cheap to produce and useful to a human typing a directive.
//
// WHAT IS DELIBERATELY NOT COMPLETED: addresses (a session id, a host name, a channel
// handle). Those are runtime facts, not grammar, and a list that offered every live
// session would be answering a question the caller did not ask — it wants the ones
// that exist, which is `ls`.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
)

// completionRequest is the part of completion/complete this server reads.
//
// The spec's shape is ref/prompt | ref/resource plus an argument object; only a
// PROMPT reference carrying the `cmd` argument names something this server can
// complete, and everything else answers an empty list rather than an error. A client
// that asks about a resource it invented is not wrong about this server — it is
// asking about a surface this server does not offer.
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

// completionResult is the answer shape: one values array, bounded by the client's
// own limit.
type completionResult struct {
	Completion struct {
		Values  []string `json:"values"`
		Total   int      `json:"total,omitempty"`
		HasMore bool     `json:"hasMore,omitempty"`
	} `json:"completion"`
}

// completionLimit bounds a reply. The spec lets a client ask for a page and this
// server does not page: the whole vocabulary is a few dozen words, so a truncated
// list would be more work than the list.
const completionLimit = 100

// completeDirective answers completion/complete for the `term` tool's `cmd`.
func (s *Server) completeDirective(ctx context.Context, params json.RawMessage) (any, *mcp.RPCError) {
	var req completionRequest
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &mcp.RPCError{Code: -32602, Message: "invalid completion/complete params: " + err.Error()}
		}
	}

	var out completionResult
	// ONLY THE `cmd` ARGUMENT OF THIS SERVER'S PROMPT. Anything else is a question
	// about a surface that does not exist here, and an empty list is the honest
	// answer — not an error, because the client did nothing wrong.
	if req.Argument.Name != "cmd" || (req.Ref.Type != "" && req.Ref.Type != "ref/prompt") {
		return out, nil
	}

	value := req.Argument.Value
	// THE VERB IS THE FIRST WORD, so only a prefix with no space in it is a verb
	// prefix. `read abc` is being completed on an address, which this server does
	// not offer.
	if strings.ContainsAny(value, " \t") {
		return out, nil
	}
	prefix := strings.ToLower(value)

	names := directive.Names()
	values := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			values = append(values, name)
		}
	}
	sort.Strings(values)
	if len(values) > completionLimit {
		values = values[:completionLimit]
	}
	out.Completion.Values = values
	out.Completion.Total = len(values)
	return out, nil
}
