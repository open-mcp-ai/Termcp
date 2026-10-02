package mcp

import (
	"context"
	"testing"
)

// Batch form of the session lifecycle tools: session_id carrying a
// comma-separated list. Each entry is executed independently and the result is
// a per-session outcomes array; one bad entry never aborts the rest. A single
// id keeps the original code path and error shapes (covered by
// TestHandleTerminateSession_SessionNotFound).

// startPipeSession starts a short-lived internal pipe session (`echo`) and
// returns its id — cheap sessions whose exact liveness does not matter because
// both batch tools (terminate, delete) accept running and DEAD entries.
func startPipeSession(t *testing.T, s *Server) string {
	t.Helper()
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    "echo",
		"args":       []any{"batch"},
		"mode":       "pipe",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return parseResult(t, res)["session_id"].(string)
}

func batchResults(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	raw, ok := result["results"].([]any)
	if !ok {
		t.Fatalf("result has no results array: %v", result)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func TestHandleDeleteSessionBatch(t *testing.T) {
	s := newTestServer(t)
	ids := []string{startPipeSession(t, s), startPipeSession(t, s), startPipeSession(t, s)}

	// Whitespace around the commas is tolerated.
	result, err := s.handleDeleteSession(context.Background(), makeRequest(map[string]any{
		"session_id": ids[0] + ", " + ids[1] + "," + ids[2],
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("batch delete reported a tool error: %v", result.Content)
	}
	results := batchResults(t, parseResult(t, result))
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, id := range ids {
		if results[i]["session_id"] != id || results[i]["ok"] != true {
			t.Errorf("results[%d] = %v, want {%s ok:true}", i, results[i], id)
		}
		if s.sessMgr.Get(id) != nil {
			t.Errorf("session %s still registered after batch delete", id)
		}
	}
}

func TestHandleDeleteSessionBatchPartialFailure(t *testing.T) {
	s := newTestServer(t)
	id := startPipeSession(t, s)

	result, err := s.handleDeleteSession(context.Background(), makeRequest(map[string]any{
		"session_id": "ghost," + id,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("a partially failing batch must still be a tool success: %v", result.Content)
	}
	results := batchResults(t, parseResult(t, result))
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0]["ok"] != false || results[0]["code"] != CodeSessionNotFound {
		t.Errorf("results[0] = %v, want ghost session_not_found", results[0])
	}
	if results[1]["ok"] != true {
		t.Errorf("results[1] = %v, want the real session ok", results[1])
	}
	// The first entry's failure must not have stopped the second.
	if s.sessMgr.Get(id) != nil {
		t.Errorf("session %s survived the batch", id)
	}
}

func TestHandleTerminateSessionBatch(t *testing.T) {
	s := newTestServer(t)
	ids := []string{startPipeSession(t, s), startPipeSession(t, s)}

	result, err := s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
		"session_id": ids[0] + "," + ids[1],
		"force":      true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("batch terminate reported a tool error: %v", result.Content)
	}
	results := batchResults(t, parseResult(t, result))
	if len(results) != 2 || results[0]["ok"] != true || results[1]["ok"] != true {
		t.Fatalf("results = %v, want both ok", results)
	}
	// Terminate only closes: the entries stay in the registry (DEAD), unlike
	// session_delete — that is the whole point of having both tools.
	for _, id := range ids {
		if s.sessMgr.Get(id) == nil {
			t.Errorf("terminate removed session %s from the registry", id)
		}
	}
}
