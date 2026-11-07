package mcp

import (
	"context"
	"sync"
	"testing"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

func startTestSession(t *testing.T, s *Server) (sessionID, shellID string) {
	t.Helper()
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testShellIdleArgs(),
		"mode":       "pipe",
		"ssh_config": "internal",
	}))
	if err != nil || res.IsError {
		t.Fatalf("start session failed: %v %+v", err, res)
	}
	m := parseResult(t, res)
	return m["session_id"].(string), m["shell_id"].(string)
}

func callReq(name string, args map[string]any) mcpgo.CallToolRequest {
	return mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: name, Arguments: args},
	}
}

type notifyCall struct {
	Level       string
	Title       string
	Message     string
	SessionID   string
	DurationSec int
}

// notifyCapture is a fake UI notifier that records deliveries and reports a
// fixed "delivered" count.
type notifyCapture struct {
	mu    sync.Mutex
	calls []notifyCall
}

func (c *notifyCapture) notifier() func(level, title, message, sessionID string, durationSec int) int {
	return func(level, title, message, sessionID string, durationSec int) int {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, notifyCall{level, title, message, sessionID, durationSec})
		return 2
	}
}

func (c *notifyCapture) recorded() []notifyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]notifyCall, 0, len(c.calls))
	for _, call := range c.calls {
		out = append(out, call)
	}
	return out
}

func TestNotifyUser_DeliversToUINotifier(t *testing.T) {
	s := newTestServer(t)
	cap := &notifyCapture{}
	s.SetUINotifier(cap.notifier())

	res, err := s.handleNotifyUser(context.Background(), makeRequest(map[string]any{
		"message":          "build finished",
		"title":            "CI",
		"level":            "success",
		"duration_seconds": 5,
	}))
	if err != nil || res.IsError {
		t.Fatalf("notify_user failed: %v %+v", err, res)
	}
	m := parseResult(t, res)
	if m["ok"] != true {
		t.Fatalf("ok = %v, want true", m["ok"])
	}
	if d := getFloat64(m, "delivered", -1); d != 2 {
		t.Fatalf("delivered = %v, want 2", m["delivered"])
	}
	if _, ok := m["session_id"]; ok {
		t.Fatalf("session_id should be omitted when not supplied, got %v", m["session_id"])
	}
	if _, ok := m["hint"]; ok {
		t.Fatalf("hint should be absent when a tab received it, got %v", m["hint"])
	}
	calls := cap.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(calls))
	}
	if got := calls[0]; got.Level != "success" || got.Title != "CI" || got.Message != "build finished" || got.DurationSec != 5 || got.SessionID != "" {
		t.Fatalf("unexpected delivery: %+v", got)
	}
}

func TestNotifyUser_RequiresMessage(t *testing.T) {
	s := newTestServer(t)
	for _, args := range []map[string]any{{}, {"message": "   "}} {
		res, err := s.handleNotifyUser(context.Background(), makeRequest(args))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("expected an error when message is missing or blank")
		}
		if code, _ := decodeToolError(t, res); code != CodeInvalidArgument {
			t.Fatalf("error_code = %q, want %q", code, CodeInvalidArgument)
		}
	}
}

func TestNotifyUser_RejectsUnknownLevel(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleNotifyUser(context.Background(), makeRequest(map[string]any{
		"message": "hi",
		"level":   "critical",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected an error for an unknown level")
	}
	if code, _ := decodeToolError(t, res); code != CodeInvalidArgument {
		t.Fatalf("error_code = %q, want %q", code, CodeInvalidArgument)
	}
}

func TestNotifyUser_UnknownSessionIsRejected(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleNotifyUser(context.Background(), makeRequest(map[string]any{
		"message":    "hi",
		"session_id": "no-such-session",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected an error for an unknown session_id")
	}
	if code, _ := decodeToolError(t, res); code != CodeSessionNotFound {
		t.Fatalf("error_code = %q, want %q", code, CodeSessionNotFound)
	}
}

func TestNotifyUser_DefaultsAndSessionForwarded(t *testing.T) {
	s := newTestServer(t)
	cap := &notifyCapture{}
	s.SetUINotifier(cap.notifier())
	sessionID, _ := startTestSession(t, s)

	res, err := s.handleNotifyUser(context.Background(), makeRequest(map[string]any{
		"message":    "needs your input",
		"session_id": sessionID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("notify_user failed: %v %+v", err, res)
	}
	if got := parseResult(t, res)["session_id"]; got != sessionID {
		t.Fatalf("session_id = %v, want %q", got, sessionID)
	}
	calls := cap.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(calls))
	}
	if got := calls[0]; got.Level != "info" || got.DurationSec != 10 || got.SessionID != sessionID || got.Title != "termcp" {
		t.Fatalf("unexpected defaults: %+v", got)
	}
}

func TestNotifyUser_NoNotifierStillSucceeds(t *testing.T) {
	s := newTestServer(t) // no SetUINotifier call
	res, err := s.handleNotifyUser(context.Background(), makeRequest(map[string]any{"message": "hi"}))
	if err != nil || res.IsError {
		t.Fatalf("notify_user failed: %v %+v", err, res)
	}
	m := parseResult(t, res)
	if d := getFloat64(m, "delivered", -1); d != 0 {
		t.Fatalf("delivered = %v, want 0", m["delivered"])
	}
	if _, ok := m["hint"]; !ok {
		t.Fatal("expected a hint when nothing was delivered")
	}
}

// termCallTool drives the single MCP tool through a real client and returns the
// handle it reports, or fails. It is how these tests keep exercising the wire
// instead of the in-process handler: the point of an end-to-end notification test
// is the transport, and a test that bypassed it would pass with no dispatch at
// all.
