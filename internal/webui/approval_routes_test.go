package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// approvalTestEnv is a live internal session wired to the real mux, which is
// what a browser or curl actually talks to.
type approvalTestEnv struct {
	mux     *http.ServeMux
	sessMgr *session.Manager
	sess    *session.Session
	shellID string
	// handler is the same Handler the mux serves, so a test can set the
	// ExecuteOperation seam the decision endpoint calls.
	handler *Handler
}

func newApprovalEnv(t *testing.T) *approvalTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})

	sess, err := sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	return &approvalTestEnv{mux: mux, sessMgr: sessMgr, sess: sess, shellID: sess.PrimaryShellID(), handler: h}
}

func (e *approvalTestEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	e.mux.ServeHTTP(rr, r)
	return rr
}

func (e *approvalTestEnv) decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", rr.Body.String(), err)
	}
	return m
}

// The mode endpoint reports the current state, including whether the WebSocket
// character stream is still accepted.
func TestApprovalModeEndpointReportsState(t *testing.T) {
	e := newApprovalEnv(t)

	rr := e.do(t, http.MethodGet, "/api/sessions/"+e.sess.ID+"/approval", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rr.Code, rr.Body.String())
	}
	m := e.decode(t, rr)
	if m["approval_mode"] != false {
		t.Errorf("approval_mode = %v, want false by default", m["approval_mode"])
	}
	if m["websocket_input"] != true {
		t.Errorf("websocket_input = %v, want true while ungated", m["websocket_input"])
	}
}

// Enabling through HTTP switches the session, and the second call reports it.
func TestApprovalModeEnableAndDisable(t *testing.T) {
	e := newApprovalEnv(t)

	rr := e.do(t, http.MethodPatch, "/api/sessions/"+e.sess.ID+"/approval",
		`{"enabled":true,"timeout_seconds":60}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH enable = %d: %s", rr.Code, rr.Body.String())
	}
	m := e.decode(t, rr)
	if m["approval_mode"] != true {
		t.Errorf("approval_mode = %v, want true", m["approval_mode"])
	}
	// The operator's terminal is never gated, so this stays true. The field
	// answers "can I type?", and the answer is yes even while review is on.
	if m["websocket_input"] != true {
		t.Error("websocket_input must stay true: review gates the AI's MCP surface, not the operator's terminal")
	}
	if !e.sess.ApprovalEnabled() {
		t.Error("the session object should be gated")
	}

	rr = e.do(t, http.MethodGet, "/api/sessions/"+e.sess.ID+"/approval", "")
	m = e.decode(t, rr)
	if m["approval_mode"] != true {
		t.Errorf("GET after enable: approval_mode = %v, want true", m["approval_mode"])
	}
	if need, _ := m["need"].(float64); need != 1 {
		t.Errorf("need = %v, want 1", m["need"])
	}

	rr = e.do(t, http.MethodPatch, "/api/sessions/"+e.sess.ID+"/approval", `{"enabled":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH disable = %d: %s", rr.Code, rr.Body.String())
	}
	if e.sess.ApprovalEnabled() {
		t.Error("the session should no longer be gated")
	}
}

// Review mode has one reviewer, so an omitted threshold means one and a request
// for more is refused rather than silently downgraded: a caller must not believe
// it configured a threshold the server ignores.
func TestApprovalModeThresholdIsAlwaysOne(t *testing.T) {
	e := newApprovalEnv(t)

	// Omitted need: accepted, and the server reports one.
	rr := e.do(t, http.MethodPatch, "/api/sessions/"+e.sess.ID+"/approval", `{"enabled":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH enable without need = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if m := e.decode(t, rr); m["need"] != float64(1) {
		t.Errorf("need = %v, want 1", m["need"])
	}
	if !e.sess.ApprovalEnabled() {
		t.Error("the session should be gated")
	}

	// A higher threshold is refused: it cannot be honoured.
	if rr := e.do(t, http.MethodPatch, "/api/sessions/"+e.sess.ID+"/approval", `{"enabled":true,"need":3}`); rr.Code != http.StatusBadRequest {
		t.Errorf("need=3 = %d, want 400", rr.Code)
	}

	rr = e.do(t, http.MethodPatch, "/api/sessions/"+e.sess.ID+"/approval",
		`{"enabled":true,"need":1,"timeout_seconds":-5}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("negative timeout = %d, want 400", rr.Code)
	}
}

// Unknown session and wrong method are reported, not silently accepted.
func TestApprovalModeEndpointErrors(t *testing.T) {
	e := newApprovalEnv(t)

	if rr := e.do(t, http.MethodGet, "/api/sessions/nope/approval", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown session = %d, want 404", rr.Code)
	}
	// A method the pattern does not register is a 404 from ServeMux (405 is only
	// produced when the same pattern exists for another method). What matters is
	// that it is not a silent success.
	if rr := e.do(t, http.MethodDelete, "/api/sessions/"+e.sess.ID+"/approval", ""); rr.Code < 400 {
		t.Errorf("DELETE = %d, want a 4xx refusal", rr.Code)
	}
}

// queueViaMCPPath submits a command line the way the MCP tool does.
//
// The decision tests need something in the queue, and REST no longer puts it
// there: the two entrances are deliberately different surfaces (MCP is the AI's
// and is gated; REST is the operator's and is not). Going through the same
// session calls the MCP handler makes keeps these tests exercising the real
// queue instead of a fixture that could drift from it.
func queueViaMCPPath(t *testing.T, sess *session.Session, shellID, text string, pressEnter bool) {
	t.Helper()
	cs := sess.GetChildShell(shellID)
	if cs == nil {
		t.Fatalf("shell %s is not attached", shellID)
	}
	if err := cs.StageForApproval("mcp", text); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if pressEnter {
		if _, err := cs.CommitStagedForApproval("mcp", "enter", 1); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

// Review mode must NOT gate REST terminal input: this is the operator's surface.
//
// The Web UI's file browser is built on this API, and the terminal endpoint is
// its script mirror. Gating it locked the operator out of their own tooling to
// stop someone who — holding the same token — could approve their own request
// just as directly. What review protects against is an agent making unattended
// changes, and the agent's surface is MCP.
func TestRESTInputIsNotGatedUnderReview(t *testing.T) {
	e := newApprovalEnv(t)
	if err := e.sess.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}

	marker := "rest_operator_marker"
	rr := e.do(t, http.MethodPost, "/api/shells/"+e.shellID+"/input",
		`{"text":"`+marker+`","press_enter":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("input = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if m := e.decode(t, rr); m["review_pending"] == true {
		t.Error("the operator's own input was held for review")
	}

	// It must actually run, not merely be accepted: "not gated" that silently
	// drops the bytes is the same bug wearing a different mask.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readSessionOutput(e.sess), marker) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the operator's input never ran; output:\n%s", readSessionOutput(e.sess))
}

// The same for a bare key: ctrl+c has to reach the shell, or a person cannot
// interrupt a command they are watching.
func TestRESTKeyIsNotGatedUnderReview(t *testing.T) {
	e := newApprovalEnv(t)
	if err := e.sess.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	rr := e.do(t, http.MethodPost, "/api/shells/"+e.shellID+"/key", `{"key":"ctrl+c","repeat":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("key = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if reqs := e.sess.ApprovalQueue().List(); len(reqs) != 0 {
		t.Errorf("the operator's keypress was queued for review: %d request(s)", len(reqs))
	}
}

// Approving through HTTP executes the queued bytes.
func TestRESTApproveExecutesTheQueuedInput(t *testing.T) {
	e := newApprovalEnv(t)
	if err := e.sess.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}

	marker := "rest_approve_marker"
	queueViaMCPPath(t, e.sess, e.shellID, marker, true)
	pendingID := onlyPendingID(t, e.sess)

	// Before approval the marker must not be in the output.
	time.Sleep(300 * time.Millisecond)
	if out := readSessionOutput(e.sess); strings.Contains(out, marker) {
		t.Fatalf("input ran before approval:\n%s", out)
	}

	rr := e.do(t, http.MethodPost, "/api/approvals/"+pendingID+"/approve", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readSessionOutput(e.sess), marker) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("approved input never ran; output:\n%s", readSessionOutput(e.sess))
}

// Rejection is fail-closed: the bytes never reach the shell.
func TestRESTRejectIsFailClosed(t *testing.T) {
	e := newApprovalEnv(t)
	if err := e.sess.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}

	marker := "rest_reject_marker"
	queueViaMCPPath(t, e.sess, e.shellID, marker, true)
	pendingID := onlyPendingID(t, e.sess)

	rr := e.do(t, http.MethodPost, "/api/approvals/"+pendingID+"/reject", `{"reason":"no"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("reject = %d: %s", rr.Code, rr.Body.String())
	}

	time.Sleep(500 * time.Millisecond)
	if out := readSessionOutput(e.sess); strings.Contains(out, marker) {
		t.Errorf("a rejected request executed anyway:\n%s", out)
	}
}

// A decision needs no body at all: it is the click that decides, and there is no
// name to collect. Requiring one produced a claim, not an identity, under a
// single deployment token.
func TestRESTDecisionNeedsNoBody(t *testing.T) {
	e := newApprovalEnv(t)
	if err := e.sess.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	queueViaMCPPath(t, e.sess, e.shellID, "x", true)
	pendingID := onlyPendingID(t, e.sess)

	// No body at all.
	rr := e.do(t, http.MethodPost, "/api/approvals/"+pendingID+"/approve", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve without a body = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	req := e.decode(t, rr)["request"].(map[string]any)
	if req["state"] != "approved" {
		t.Errorf("state = %v, want approved", req["state"])
	}
	// The author is the local reviewer, not a name the caller supplied.
	approvals, _ := req["approvals"].([]any)
	if len(approvals) != 1 || approvals[0] != "local" {
		t.Errorf("approvals = %v, want [local]", approvals)
	}
}

func TestApprovalListEndpoint(t *testing.T) {
	e := newApprovalEnv(t)
	if err := e.sess.EnableApproval(1, time.Minute); err != nil {
		t.Fatal(err)
	}
	queueViaMCPPath(t, e.sess, e.shellID, "listed", true)

	rr := e.do(t, http.MethodGet, "/api/approvals", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rr.Code, rr.Body.String())
	}
	m := e.decode(t, rr)
	items, _ := m["approvals"].([]any)
	if len(items) != 1 {
		t.Fatalf("approvals = %d, want 1", len(items))
	}
	entry := items[0].(map[string]any)
	req := entry["request"].(map[string]any)
	if req["state"] != "pending" {
		t.Errorf("state = %v, want pending", req["state"])
	}
	if need, _ := entry["need"].(float64); need != 1 {
		t.Errorf("need = %v, want 1", entry["need"])
	}
}

// An unknown pending id is a 404, not a silent success.
func TestApprovalDecisionUnknownID(t *testing.T) {
	e := newApprovalEnv(t)
	rr := e.do(t, http.MethodPost, "/api/approvals/does-not-exist/approve", `{"approver":"alice"}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rr.Code)
	}
}

// readSessionOutput drains whatever the session has buffered.
func readSessionOutput(s *session.Session) string {
	out := ""
	for {
		chunk, err := s.ReadOutput(context.Background(), 200*time.Millisecond, true, 0, 0)
		out += chunk
		if err != nil || chunk == "" {
			return out
		}
	}
}

// onlyPendingID returns the one queued request's id. The HTTP response no longer
// carries one (the caller cannot decide it), so tests read the queue.
func onlyPendingID(t *testing.T, sess *session.Session) string {
	t.Helper()
	reqs := sess.ApprovalQueue().List()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 queued request, got %d", len(reqs))
	}
	return reqs[0].ID
}

// A session created from a profile with default_approval must be gated from the
// moment it exists, without anyone flipping a switch.
//
// This is the whole point of the setting, and it is easy to get wrong in a way
// nothing else notices: wiring the profile field through to the config but not
// acting on it at creation leaves a session that LOOKS ordinary and accepts
// writes, which is exactly the failure the feature exists to prevent.
func TestSessionCreatedFromGatedProfileStartsGated(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})

	// A gated profile on disk, reached through the same store the handler uses.
	cfgStore := sshconfig.NewStore(dir)
	body := "kind = \"internal\"\ndefault_approval = true\n"
	if _, err := sshconfig.ParseAndValidate([]byte(body)); err != nil {
		t.Fatalf("fixture profile is invalid: %v", err)
	}
	if err := cfgStore.Save("gated", []byte(body)); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	plainBody := "kind = \"internal\"\n"
	if err := cfgStore.Save("plain", []byte(plainBody)); err != nil {
		t.Fatalf("save plain profile: %v", err)
	}

	h := &Handler{Sessions: sessMgr, SSH: cfgStore}
	mux := http.NewServeMux()
	h.Register(mux)
	post := func(sshConfig string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		reqBody := `{"ssh_config":"` + sshConfig + `","rows":24,"cols":80}`
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(reqBody)))
		if rr.Code != http.StatusOK {
			t.Fatalf("create from %q = %d: %s", sshConfig, rr.Code, rr.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Gated: the created session must report approval_mode, and a write to it
	// must be held rather than executed.
	gated := post("gated")
	gatedID, _ := gated["session_id"].(string)
	if gatedID == "" {
		t.Fatal("no session_id returned")
	}
	sess := sessMgr.Get(gatedID)
	if sess == nil {
		t.Fatal("created session not found")
	}
	if !sess.ApprovalEnabled() {
		t.Fatal("a session created from a default_approval profile is not gated; the setting did nothing")
	}
	if info := sess.Info(); !info.ApprovalMode {
		t.Error("Info() does not report approval_mode for a session gated at creation")
	}
	shellID := sess.PrimaryShellID()
	if !sess.GetChildShell(shellID).RequiresApproval() {
		t.Error("the primary shell does not require approval, so a write would go straight through")
	}

	// Plain: the same path must NOT gate, or the field is being ignored and
	// everything is gated.
	plain := post("plain")
	plainID, _ := plain["session_id"].(string)
	plainSess := sessMgr.Get(plainID)
	if plainSess == nil {
		t.Fatal("plain session not found")
	}
	if plainSess.ApprovalEnabled() {
		t.Error("a session from a profile without default_approval was gated")
	}
}

// An approved OPERATION (a file transfer, a port forward) must execute through
// ExecuteOperation, not through the command-line path.
//
// This is the seam the two packages meet at, and it was broken: executeApproved
// looked up a child shell for every request, but an operation's payload carries a
// session id and no shell id, so GetChildShell("") returned nil and the decision
// failed with `approved but execution failed: shell  is no longer attached`
// (note the empty id) before the operation was ever attempted. Review accepted the
// decision, reported a failure, and did nothing.
//
// The MCP-side test did not catch it because it called ExecuteApprovedOperation
// directly, skipping this dispatcher — the bug lived exactly in the gap between
// the two tests' reach. This one crosses that gap.
func TestApprovedOperationGoesToTheOperationExecutor(t *testing.T) {
	env := newApprovalEnv(t)
	if err := env.sess.EnableApproval(1, 0); err != nil {
		t.Fatalf("enable approval: %v", err)
	}

	var called []approval.Request
	env.handler.ExecuteOperation = func(req approval.Request) error {
		called = append(called, req)
		return nil
	}

	q := env.sess.ApprovalQueue()
	if q == nil {
		t.Fatal("the session has no approval queue")
	}
	// An operation: session-scoped, so no shell id — which is the whole point.
	// Routing this down the shell path is what failed.
	id, err := q.Submit(approval.Submission{
		Source:  "mcp",
		Kind:    approval.KindFileWrite,
		Summary: "write 5 bytes to /tmp/x",
		Payload: []byte(`{"tool":"file_write","args":{"remote_path":"/tmp/x"}}`),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	req, err := q.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if req.ShellID != "" {
		t.Fatalf("precondition: an operation carries no shell id, got %q", req.ShellID)
	}

	rr := env.do(t, http.MethodPost, "/api/approvals/"+id+"/approve", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s); an approved operation must not fail on a shell lookup",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if len(called) != 1 {
		t.Fatalf("ExecuteOperation called %d time(s), want 1", len(called))
	}
	if called[0].Kind != approval.KindFileWrite {
		t.Errorf("executor received kind %q, want %q", called[0].Kind, approval.KindFileWrite)
	}
	if called[0].ID != id {
		t.Errorf("executor received id %q, want %q", called[0].ID, id)
	}
}

// A command line must NOT go to the operation executor: it belongs to the shell
// path, and sending it elsewhere would silently drop the bytes.
func TestApprovedShellInputStillUsesTheShellPath(t *testing.T) {
	env := newApprovalEnv(t)
	if err := env.sess.EnableApproval(1, 0); err != nil {
		t.Fatalf("enable approval: %v", err)
	}

	ops := 0
	env.handler.ExecuteOperation = func(req approval.Request) error {
		ops++
		return nil
	}
	q := env.sess.ApprovalQueue()
	if q == nil {
		t.Fatal("the session has no approval queue")
	}
	id, err := q.SubmitShellInput(env.shellID, "mcp", "echo hi", []string{"enter"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	rr := env.do(t, http.MethodPost, "/api/approvals/"+id+"/approve", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s), want 200", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if ops != 0 {
		t.Errorf("ExecuteOperation was called %d time(s) for a command line; it must go to the shell", ops)
	}
}
