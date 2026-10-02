package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// The batch forms of the session lifecycle routes: {id} carrying a
// comma-separated list. Single-id behavior (204/404) must stay untouched; a
// batch answers 200 with per-id outcomes and never lets one bad entry abort the
// rest. These tests reuse newApprovalEnv — it is the live-session harness (real
// in-process SSH server + manager + mux) that every session route test shares.

func extraSession(t *testing.T, e *approvalTestEnv) *session.Session {
	t.Helper()
	sess, err := e.sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func decodeBatchResults(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON %q: %v", rr.Body.String(), err)
	}
	return body.Results
}

func TestBatchPurgeSessions(t *testing.T) {
	e := newApprovalEnv(t)
	s2 := extraSession(t, e)
	s3 := extraSession(t, e)
	ids := []string{e.sess.ID, s2.ID, s3.ID}

	rr := e.do(t, http.MethodDelete, "/api/sessions/"+ids[0]+","+ids[1]+","+ids[2], "")
	if rr.Code != http.StatusOK {
		t.Fatalf("batch DELETE = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	results := decodeBatchResults(t, rr)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, id := range ids {
		if results[i]["id"] != id || results[i]["ok"] != true {
			t.Errorf("results[%d] = %v, want {%s ok:true}", i, results[i], id)
		}
		if got := e.sessMgr.Get(id); got != nil {
			t.Errorf("session %s still registered after batch purge", id)
		}
	}
}

func TestBatchPurgeSessionsPartialFailure(t *testing.T) {
	e := newApprovalEnv(t)
	s2 := extraSession(t, e)

	rr := e.do(t, http.MethodDelete, "/api/sessions/"+e.sess.ID+",ghost,"+s2.ID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("batch DELETE = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	results := decodeBatchResults(t, rr)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0]["ok"] != true || results[0]["id"] != e.sess.ID {
		t.Errorf("results[0] = %v, want first session ok", results[0])
	}
	if results[1]["ok"] != false || results[1]["code"] != "session_not_found" {
		t.Errorf("results[1] = %v, want ghost session_not_found", results[1])
	}
	if results[2]["ok"] != true || results[2]["id"] != s2.ID {
		t.Errorf("results[2] = %v, want second session ok", results[2])
	}
	// The middle failure must not have aborted the entry after it.
	if e.sessMgr.Get(e.sess.ID) != nil || e.sessMgr.Get(s2.ID) != nil {
		t.Error("a missing entry in the middle stopped the batch")
	}
}

func TestBatchTerminateSessions(t *testing.T) {
	e := newApprovalEnv(t)
	s2 := extraSession(t, e)

	rr := e.do(t, http.MethodPost, "/api/sessions/"+e.sess.ID+","+s2.ID+"/terminate", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("batch terminate = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	results := decodeBatchResults(t, rr)
	if len(results) != 2 || results[0]["ok"] != true || results[1]["ok"] != true {
		t.Fatalf("results = %v, want both ok", results)
	}
	// Terminate only closes: both entries stay in the registry as DEAD.
	for _, id := range []string{e.sess.ID, s2.ID} {
		sess := e.sessMgr.Get(id)
		if sess == nil {
			t.Fatalf("terminate removed session %s from the registry", id)
		}
		if st := sess.Info().Status; st != api.SessionExited {
			t.Errorf("session %s status = %s, want %s", id, st, api.SessionExited)
		}
	}
}

// A single id keeps the pre-batch contract exactly: 204 on success, 404 when
// the session does not exist.
func TestSingleSessionDeleteContract(t *testing.T) {
	e := newApprovalEnv(t)

	rr := e.do(t, http.MethodDelete, "/api/sessions/"+e.sess.ID, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("single DELETE = %d, want 204: %s", rr.Code, rr.Body.String())
	}
	rr = e.do(t, http.MethodDelete, "/api/sessions/nope", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("single DELETE of missing = %d, want 404", rr.Code)
	}
}
