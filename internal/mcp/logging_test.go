package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *captureHandler) Level() slog.Level                            { return slog.LevelDebug } // required by Go 1.24+ slog optimization
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(_ string) slog.Handler      { return c }

func (c *captureHandler) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

func withCapturedLogger(t *testing.T) *captureHandler {
	t.Helper()
	prev := slog.Default()
	cap := &captureHandler{}
	logger := slog.New(cap)
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

func attrValue(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value
			found = true
			return false
		}
		return true
	})
	return v, found
}

func TestWithLogging_PassesThroughResultAndError(t *testing.T) {
	ctx := context.Background()
	wantResult := mcpgo.NewToolResultText("payload")
	wantErr := errors.New("boom")

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return wantResult, wantErr
	}

	wrapped := withLogging("test_tool", h)
	gotResult, gotErr := wrapped(ctx, mcpgo.CallToolRequest{})

	if gotResult != wantResult {
		t.Fatalf("result not preserved: got %v, want %v", gotResult, wantResult)
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error not preserved: got %v, want %v", gotErr, wantErr)
	}
}

func TestWithLogging_LogsDebugEntryAndExit(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 records (entry+exit), got %d", len(records))
	}

	entry := records[0]
	if entry.Level != slog.LevelDebug {
		t.Fatalf("entry record: expected Debug level, got %v", entry.Level)
	}
	if v, ok := attrValue(entry, "tool"); !ok || v.String() != "test_tool" {
		t.Fatalf("entry record: expected tool=test_tool, got %v (found=%v)", v, ok)
	}

	exit := records[len(records)-1]
	if exit.Level != slog.LevelDebug {
		t.Fatalf("exit record: expected Debug level, got %v", exit.Level)
	}
	if v, ok := attrValue(exit, "tool"); !ok || v.String() != "test_tool" {
		t.Fatalf("exit record: expected tool=test_tool, got %v (found=%v)", v, ok)
	}
	if _, ok := attrValue(exit, "duration_ms"); !ok {
		t.Fatal("exit record: expected duration_ms attr")
	}
}

func TestWithLogging_LogsErrorOnGoError(t *testing.T) {
	cap := withCapturedLogger(t)

	wantErr := errors.New("kaboom")
	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return nil, wantErr
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr passthrough, got %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 records, got %d", len(records))
	}
	exit := records[len(records)-1]
	if exit.Level != slog.LevelError {
		t.Fatalf("exit record: expected Error level on Go error, got %v", exit.Level)
	}
	v, ok := attrValue(exit, "err")
	if !ok {
		t.Fatal("exit record: expected err attr on Go error")
	}
	if v.String() != "kaboom" {
		t.Fatalf("exit record: expected err=kaboom, got %q", v.String())
	}
}

func TestWithLogging_LogsWarnOnIsErrorResult(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultError("invalid arg"), nil
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 records, got %d", len(records))
	}
	exit := records[len(records)-1]
	if exit.Level != slog.LevelWarn {
		t.Fatalf("exit record: expected Warn level on IsError result (visible at default log level), got %v", exit.Level)
	}
	v, ok := attrValue(exit, "is_error")
	if !ok {
		t.Fatal("exit record: expected is_error attr on IsError result")
	}
	if !v.Bool() {
		t.Fatalf("exit record: expected is_error=true, got %v", v)
	}
	if ev, eok := attrValue(exit, "error"); !eok || ev.String() != "invalid arg" {
		t.Fatalf("exit record: expected error='invalid arg' on IsError result, got %q (found=%v)", ev, eok)
	}
}

func TestWithLogging_ExtractsSessionAndReaderIDs(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("test_tool", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc-123",
		"reader_id":  float64(7),
		"text":       "ignored",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "session_id")
	if !ok {
		t.Fatal("entry: expected session_id attr")
	}
	if v.String() != "abc-123" {
		t.Fatalf("entry: expected session_id=abc-123, got %q", v.String())
	}

	v, ok = attrValue(entry, "reader_id")
	if !ok {
		t.Fatal("entry: expected reader_id attr")
	}
	if v.Int64() != 7 {
		t.Fatalf("entry: expected reader_id=7, got %v", v)
	}
}

func TestWithLogging_OmitsAbsentSessionAndReaderIDs(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]
	if _, ok := attrValue(entry, "session_id"); ok {
		t.Fatal("entry: session_id attr should be absent when not in args")
	}
	if _, ok := attrValue(entry, "reader_id"); ok {
		t.Fatal("entry: reader_id attr should be absent when not in args")
	}
}

func TestWithLogging_ExtractsTextParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("shell_input", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc-123",
		"text":       "echo hello world",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "text")
	if !ok {
		t.Fatal("entry: expected text attr")
	}
	if v.String() != "echo hello world" {
		t.Fatalf("entry: expected text='echo hello world', got %q", v.String())
	}
}

func TestWithLogging_ExtractsCommandParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("session_start", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"command":    "ssh",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "command")
	if !ok {
		t.Fatal("entry: expected command attr")
	}
	if v.String() != "ssh" {
		t.Fatalf("entry: expected command=ssh, got %q", v.String())
	}
}

func TestWithLogging_ExtractsModeParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("session_start", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"mode":       "pipe",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]
	v, ok := attrValue(entry, "mode")
	if !ok {
		t.Fatal("entry: expected mode attr")
	}
	if v.String() != "pipe" {
		t.Fatalf("entry: expected mode=pipe, got %q", v.String())
	}
}

func TestWithLogging_ExtractsRowsAndCols(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("session_start", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"rows":       float64(40),
		"cols":       float64(120),
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]

	for _, key := range []string{"rows", "cols"} {
		v, ok := attrValue(entry, key)
		if !ok {
			t.Fatalf("entry: expected %s attr", key)
		}
		if v.Int64() <= 0 {
			t.Fatalf("entry: expected %s > 0, got %v", key, v)
		}
	}
}

func TestWithLogging_ExtractsTimeoutAndShellKey(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("press_key", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"shell_id": "sh-1",
		"timeout":  float64(3.5),
		"key":      "enter",
		"repeat":   float64(2),
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]

	if v, ok := attrValue(entry, "timeout"); !ok {
		t.Fatal("entry: expected timeout attr")
	} else if v.Float64() != 3.5 {
		t.Fatalf("entry: expected timeout=3.5, got %v", v)
	}

	if v, ok := attrValue(entry, "shell_id"); !ok {
		t.Fatal("entry: expected shell_id attr")
	} else if v.String() != "sh-1" {
		t.Fatalf("entry: expected shell_id=sh-1, got %q", v.String())
	}

	if v, ok := attrValue(entry, "key"); !ok {
		t.Fatal("entry: expected key attr")
	} else if v.String() != "enter" {
		t.Fatalf("entry: expected key=enter, got %q", v.String())
	}
}

func TestWithLogging_ExtractsForceAndGracePeriod(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("terminate_session", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id":   "abc",
		"force":        true,
		"grace_period": float64(3),
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]

	if v, ok := attrValue(entry, "force"); !ok {
		t.Fatal("entry: expected force attr")
	} else if !v.Bool() {
		t.Fatal("entry: expected force=true")
	}

	if v, ok := attrValue(entry, "grace_period"); !ok {
		t.Fatal("entry: expected grace_period attr")
	} else if v.Int64() != 3 {
		t.Fatalf("entry: expected grace_period=3, got %v", v)
	}
}

func TestWithLogging_ExtractsArgsParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("session_start", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"command":    "ping",
		"args":       []any{"-c", "5", "google.com"},
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]
	v, ok := attrValue(entry, "args")
	if !ok {
		t.Fatal("entry: expected args attr")
	}
	if v.String() != `["-c","5","google.com"]` {
		t.Fatalf("entry: expected args JSON, got %q", v.String())
	}
}

func TestWithLogging_TruncatesArgsOver10(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("session_start", h)

	raw := make([]any, 15)
	for i := range raw {
		raw[i] = "arg"
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"command":    "cmd",
		"args":       raw,
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]
	v, ok := attrValue(entry, "args")
	if !ok {
		t.Fatal("entry: expected args attr")
	}
	// Should contain "... (15 items total)"
	if !strings.Contains(v.String(), "15 items total") {
		t.Fatalf("entry: expected truncated args with item count, got %q", v.String())
	}
}

func TestWithLogging_LogsOutputPreviewOnExit(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("hello from process"), nil
	}
	wrapped := withLogging("read_output", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	exit := records[len(records)-1]
	v, ok := attrValue(exit, "output_preview")
	if !ok {
		t.Fatal("exit: expected output_preview attr")
	}
	if v.String() != "hello from process" {
		t.Fatalf("exit: expected output_preview='hello from process', got %q", v.String())
	}
}

func TestWithLogging_TruncatesLongOutputPreview(t *testing.T) {
	cap := withCapturedLogger(t)

	longOutput := strings.Repeat("y", 250)
	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(longOutput), nil
	}
	wrapped := withLogging("read_output", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	exit := records[len(records)-1]
	v, ok := attrValue(exit, "output_preview")
	if !ok {
		t.Fatal("exit: expected output_preview attr")
	}
	logged := v.String()
	if len(logged) > 203 || len(logged) < 198 {
		t.Fatalf("expected truncated length ~200, got %d: %q", len(logged), logged)
	}
	if !strings.HasSuffix(logged, "...") {
		t.Fatalf("expected truncated output ending with '...', got %q", logged)
	}
}

func TestWithLogging_TruncatesLongText(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("shell_input", h)

	longText := strings.Repeat("x", 250)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"text":       longText,
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "text")
	if !ok {
		t.Fatal("entry: expected text attr")
	}
	logged := v.String()
	if len(logged) > 203 || len(logged) < 198 {
		t.Fatalf("expected truncated length ~200, got %d: %q", len(logged), logged)
	}
	if !strings.HasSuffix(logged, "...") {
		t.Fatalf("expected truncated text ending with '...', got %q", logged)
	}
}

// A panicking tool handler must become a normal failed tool result, not a
// protocol error and not the end of the process. Both audiences are checked,
// because the point of the middleware is that they get different things: the
// client a shape it already knows how to branch on, the operator the panic.
func TestPanicRecovery_ToolErrorForClientAndStackForLog(t *testing.T) {
	cap := withCapturedLogger(t)

	s := newTestServer(t)
	s.mcpServer.AddTool(mcpgo.NewTool("zz_boom"), withLogging("zz_boom",
		func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			panic("sentinel-panic-value")
		}))

	h := s.StreamableHTTPHandler()
	post := func(body, sid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	rr := post(initBody, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("initialize status=%d body=%s", rr.Code, rr.Body.String())
	}
	sid := rr.Header().Get("Mcp-Session-Id")
	if sid == "" {
		sid = rr.Header().Get("mcp-session-id")
	}
	if sid == "" {
		t.Fatalf("no session id; headers=%v", rr.Header())
	}

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"zz_boom","arguments":{}}}`
	rr2 := post(callBody, sid)
	body := rr2.Body.String()

	// --- client ------------------------------------------------------------
	if rr2.Code != http.StatusOK {
		t.Fatalf("panic produced status %d, want 200 with a failed tool result; body=%s", rr2.Code, body)
	}
	if !strings.Contains(body, CodeInternalError) {
		t.Errorf("response lacks error_code=%q: %s", CodeInternalError, body)
	}
	if !strings.Contains(body, `"isError":true`) {
		t.Errorf("response is not marked isError: %s", body)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err == nil {
		if e, ok := env["error"]; ok && e != nil {
			t.Errorf("panic surfaced as a JSON-RPC protocol error instead of a tool result: %v", e)
		}
	}
	// The panic value names internals, so it must not reach the client.
	if strings.Contains(body, "sentinel-panic-value") {
		t.Error("panic value leaked into the client response")
	}

	// --- operator ----------------------------------------------------------
	// Recovering a panic without logging it would trade a loud crash for a quiet
	// wrong answer, so the log is part of the contract, not a nicety.
	var logged []string
	for _, r := range cap.snapshot() {
		var sb strings.Builder
		sb.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			sb.WriteString(" ")
			sb.WriteString(a.Key)
			sb.WriteString("=")
			sb.WriteString(a.Value.String())
			return true
		})
		logged = append(logged, sb.String())
	}
	all := strings.Join(logged, "\n")
	for _, want := range []string{
		"panic recovered in tool handler",
		"sentinel-panic-value", // the panic value itself
		"goroutine",            // debug.Stack output
		"zz_boom",              // which tool panicked
	} {
		if !strings.Contains(all, want) {
			t.Errorf("log is missing %q; logged:\n%s", want, all)
		}
	}
}
