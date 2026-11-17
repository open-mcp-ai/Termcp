package mcp

// completion/complete answered from the directive grammar (issue #327).

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCompletionAnswersFromTheGrammar: `cl` returns `close`, and a prefix that matches
// nothing returns an empty list rather than an error.
func TestCompletionAnswersFromTheGrammar(t *testing.T) {
	s := newTestServer(t)
	ask := func(argument, value string) completionResult {
		params := `{"ref":{"type":"ref/prompt","name":"term"},"argument":{"name":"` + argument + `","value":"` + value + `"}}`
		answer, rpcErr := s.completeDirective(context.Background(), json.RawMessage(params))
		if rpcErr != nil {
			t.Fatalf("completion returned an error: %+v", rpcErr)
		}
		return answer.(completionResult)
	}

	if got := ask("cmd", "cl"); len(got.Completion.Values) == 0 || got.Completion.Values[0] != "close" {
		t.Errorf("`cl` should complete to close, got %v", got.Completion.Values)
	}
	if got := ask("cmd", "zzz"); len(got.Completion.Values) != 0 {
		t.Errorf("an unmatched prefix should return nothing, got %v", got.Completion.Values)
	}
	if got := ask("shell_id", "a"); len(got.Completion.Values) != 0 {
		t.Errorf("only cmd is completed here, got %v for another argument", got.Completion.Values)
	}
	// A verb prefix followed by a space is an address being typed, which is not grammar
	// and not this server's to complete.
	if got := ask("cmd", "read abc"); len(got.Completion.Values) != 0 {
		t.Errorf("an address must not be completed as a verb, got %v", got.Completion.Values)
	}
}

// TestCompletionIsEmptyForANonPromptRef: a client asking about a resource it invented
// is not wrong about this server, so an empty list is the answer rather than an error.
func TestCompletionIsEmptyForANonPromptRef(t *testing.T) {
	s := newTestServer(t)
	params := `{"ref":{"type":"ref/resource","uri":"file:///x"},"argument":{"name":"cmd","value":"cl"}}`
	answer, rpcErr := s.completeDirective(context.Background(), json.RawMessage(params))
	if rpcErr != nil {
		t.Fatalf("a resource ref should not be an error: %+v", rpcErr)
	}
	if got := answer.(completionResult); len(got.Completion.Values) != 0 {
		t.Errorf("a resource ref should complete nothing, got %v", got.Completion.Values)
	}
}
