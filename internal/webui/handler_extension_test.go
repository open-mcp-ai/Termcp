package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// extensionTestHandler stands up the panel's full backing stack: a loopback SSH
// server, a session manager, both stores and a running reconciler, so the
// endpoints answer with real state rather than a stub.
func extensionTestHandler(t *testing.T) *Handler {
	t.Helper()
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
	r := extensions.NewReconciler(
		extensions.NewStore(dir), sessMgr, msgMgr,
		sshconfig.NewStore(dir), forward.NewForwardManager(),
	)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop() })
	return &Handler{Sessions: sessMgr, MsgMgr: msgMgr, Extensions: r}
}

func getExtensions(t *testing.T, h *Handler) []extensionSummary {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/extensions = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Extensions []extensionSummary `json:"extensions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode extensions body: %v (%s)", err, rr.Body.String())
	}
	return body.Extensions
}

func saveTestExtension(t *testing.T, h *Handler, ext *extensions.Extension) {
	t.Helper()
	if err := h.Extensions.Store().Save(ext); err != nil {
		t.Fatal(err)
	}
}

// askTestExtension asks for an on-demand extension, which is what the panel's own
// create route does (issue #183): a template that declares no forward is not
// dialled until something asks. Tests that save straight to the store — bypassing
// the route — say the ask themselves, and a test about a template that must STAY
// down simply does not call this.
func askTestExtension(t *testing.T, h *Handler, name string) {
	t.Helper()
	if _, err := h.Extensions.Acquire(name, 0); err != nil {
		t.Fatalf("asking for %s: %v", name, err)
	}
}

// TestExtensionsEndpointEmpty pins that a handler with no reconciler still
// answers, with an empty list rather than an error: the panel renders nothing.
func TestExtensionsEndpointEmpty(t *testing.T) {
	h := &Handler{}
	if rows := getExtensions(t, h); len(rows) != 0 {
		t.Errorf("want no extensions without a reconciler, got %d", len(rows))
	}
}

// TestExtensionsEndpointListsStatus pins the panel's row: the template's shape,
// the reconciler's verdict, and the handles only an available extension carries.
func TestExtensionsEndpointListsStatus(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Description = "docs window"
	saveTestExtension(t, h, &ext)
	askTestExtension(t, h, "docs")

	// An extension starts unavailable and is pushed toward available; wait for
	// the reconciler to prove the session before asserting on the handles.
	deadline := time.Now().Add(8 * time.Second)
	var rows []extensionSummary
	for {
		rows = getExtensions(t, h)
		if row := findExtension(rows, "docs"); row != nil && row.Status == extensions.StatusAvailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("extension never became available, last rows: %+v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}

	row := findExtension(rows, "docs")
	if row == nil {
		t.Fatalf("extension docs missing from %v", rows)
	}
	if row.Target != "internal" || row.Description != "docs window" {
		t.Errorf("template fields not echoed: %+v", row)
	}
	if !row.Enabled {
		t.Error("an extension with no enabled key should be enabled")
	}
	if row.SessionID == "" {
		t.Error("an available extension should carry its session id")
	}
	if row.ShellID != "" {
		t.Errorf("a no-shell skill extension should not carry a shell id, got %q", row.ShellID)
	}
}

// TestExtensionsEndpointDisabled pins the parked state: the reconciler does not
// connect it, and the card says why it is not coming up.
func TestExtensionsEndpointDisabled(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Extension{}
	ext.Name = "parked"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.SetEnabled(false)
	saveTestExtension(t, h, &ext)

	deadline := time.Now().Add(2 * time.Second)
	for {
		row := findExtension(getExtensions(t, h), "parked")
		if row != nil && row.Status == extensions.StatusDisabled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disabled extension should report disabled at once, got %+v", getExtensions(t, h))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionsEndpointFailure pins that a template that cannot come up shows
// the reason on its row, so the panel explains the red lamp instead of just
// showing it.
func TestExtensionsEndpointFailure(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Extension{}
	ext.Name = "nowhere"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)
	askTestExtension(t, h, "nowhere")

	deadline := time.Now().Add(8 * time.Second)
	for {
		row := findExtension(getExtensions(t, h), "nowhere")
		if row != nil && row.Status == extensions.StatusUnavailable && row.Failure != nil {
			if row.Failure.Stage != "target" {
				t.Errorf("want a target-stage failure, got %+v", row.Failure)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failing extension never reported its reason, got %+v", getExtensions(t, h))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionDetail pins that the single-extension view serves the whole
// template, notes included, and the full failure history.
func TestExtensionDetail(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Extension{Mode: "pipe"}
	ext.Name = "mcpbox"
	ext.Target = "internal"
	ext.Command = "cat"
	ext.Allow = []string{capability.ShellInput, capability.ShellOutput}
	ext.Notes = "drive it as line-delimited JSON-RPC"
	saveTestExtension(t, h, &ext)

	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions/mcpbox", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/extensions/mcpbox = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode extension detail: %v (%s)", err, rr.Body.String())
	}
	if notes, _ := body["notes"].(string); notes != ext.Notes {
		t.Errorf("notes = %v, want the template's notes", body["notes"])
	}
	if allow, _ := body["allow"].([]any); len(allow) == 0 {
		t.Error("the declared whitelist should be served")
	}
	if _, ok := body["extension"]; !ok {
		t.Errorf("detail should nest the panel row, got %v", body)
	}

	// A name that was never saved is a 404, not an empty object.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions/ghost", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET unknown extension = %d, want 404", rr.Code)
	}
}

// TestExtensionsWSFrame pins that the socket frame carries the same rows the
// REST call serves, so the panel's two read paths cannot drift.
func TestExtensionsWSFrame(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	rows := summarizeExtensions(h.Extensions)
	if row := findExtension(rows, "docs"); row == nil {
		t.Fatalf("WS frame misses the extension, got %+v", rows)
	} else if row.Status != extensions.StatusIdle && row.Status != extensions.StatusUnavailable && row.Status != extensions.StatusProbing && row.Status != extensions.StatusAvailable {
		/* IDLE belongs in this set: an on-demand extension that nothing has asked
		   for yet is reported as idle rather than unavailable, and this assertion
		   is the one place the frame's status vocabulary is written down. Leaving
		   it out made the test a race — green once the reconciler had started
		   probing, red on the frames before that. */
		t.Errorf("WS row status %q is not one the reconciler reports", row.Status)
	}
}

// TestExtensionRetryEndpoint pins the panel's one write path: the button and
// the agent's extension(action=retry) are the same reconciler call, so a
// disabled extension is refused on both and a failing one gets another attempt.
func TestExtensionRetryEndpoint(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Extension{}
	ext.Name = "nowhere"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	parked := extensions.Extension{}
	parked.Name = "parked"
	parked.Target = "internal"
	parked.Paths = []string{"/srv/"}
	parked.SetEnabled(false)
	saveTestExtension(t, h, &parked)

	mux := http.NewServeMux()
	h.Register(mux)

	post := func(name string) int {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/extensions/"+name+"/retry", nil))
		if rr.Code != http.StatusOK {
			return rr.Code
		}
		var body struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode retry body for %s: %v (%s)", name, err, rr.Body.String())
		}
		if !body.OK {
			return 0
		}
		return 200
	}

	// A disabled extension is not reconciled, so there is nothing to retry.
	if code := post("parked"); code != 0 {
		t.Errorf("retry on a disabled extension should report ok=false, got status %d", code)
	}
	// A failing extension is retried: the endpoint answers ok and the reconciler
	// records another attempt rather than leaving the failure count where it was.
	if code := post("nowhere"); code != 200 {
		t.Errorf("retry on a failing extension should succeed, got status %d", code)
	}

	// A name that was never saved is refused, not silently accepted as a retry.
	if code := post("ghost"); code != 0 {
		t.Errorf("retry on an unknown extension should report ok=false, got status %d", code)
	}

	// No reconciler at all: the endpoint still answers, with ok=false, so the
	// panel's button degrades instead of 500ing.
	empty := &Handler{}
	emptyMux := http.NewServeMux()
	empty.Register(emptyMux)
	rr := httptest.NewRecorder()
	emptyMux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/extensions/ghost/retry", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("retry without a reconciler = %d, want 200", rr.Code)
	}
}

func findExtension(rows []extensionSummary, name string) *extensionSummary {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

/* The authoring routes. These pin that the browser's create/update/delete are
   the same write the MCP `extension` tool makes: the same store, the same
   validation, and therefore the same answer for the same template. */

// callExtension issues one authoring request and returns the status code and the
// decoded row the route answered with (nil when the body carries no row).
func callExtension(t *testing.T, mux *http.ServeMux, method, target, body string) (int, extensionSummary) {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, target, reader)
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		return rr.Code, extensionSummary{}
	}
	var out struct {
		Extension extensionSummary `json:"extension"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s %s body: %v (%s)", method, target, err, rr.Body.String())
	}
	return rr.Code, out.Extension
}

// TestExtensionCreate pins the browser's create: the declared fields are enough,
// the conventional values for them are filled in, and the row is echoed back.
func TestExtensionCreate(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	code, row := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"docs","target":"internal","paths":["/srv/"]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/extensions = %d, want 201", code)
	}
	if row.Target != "internal" {
		t.Errorf("created row = %+v, want the sent target", row)
	}
	// The store is the shared layer: what the route wrote is what the tool reads.
	ext, err := h.Extensions.Store().Load("docs")
	if err != nil {
		t.Fatalf("created extension not persisted: %v", err)
	}
	if len(ext.Paths) != 1 || ext.Paths[0] != "/srv/" {
		t.Errorf("paths = %v, want [/srv/]", ext.Paths)
	}
	if len(ext.Allow) == 0 {
		t.Error("a read-only file window should get its tool whitelist on create")
	}

	// A duplicate is a conflict, so the author hears about the name collision
	// instead of overwriting a template in place.
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"docs","target":"internal","paths":["/srv/"]}`); code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", code)
	}
	// A template that cannot be built is refused before it is saved, so the
	// panel never shows a template the reconciler cannot stand up.
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"nothing","target":"internal"}`); code != http.StatusBadRequest {
		t.Errorf("a template that declares no capability = %d, want 400", code)
	}
	// No name means no file to write it to.
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"target":"internal","paths":["/srv/"]}`); code != http.StatusBadRequest {
		t.Errorf("create without a name = %d, want 400", code)
	}
	// Malformed JSON is reported, not silently treated as "change nothing".
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions", `{not json`); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
}

// TestExtensionCreateBareTemplate pins that a terminal declared by its fields is a
// first-class template: it takes no conventional values beyond what its shape
// implies, because there is no category to pick first.
func TestExtensionCreateCustom(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"scratch","target":"internal","mode":"pty"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST a bare terminal = %d, want 201", code)
	}
	// A terminal takes no conventional values, so it keeps only what the author
	// sent — a pty shell with no whitelisted tools and no notes of its own.
	ext, err := h.Extensions.Store().Load("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Mode != "pty" {
		t.Errorf("mode = %q, want pty", ext.Mode)
	}
	if ext.Notes != "" || len(ext.Allow) != 0 {
		t.Errorf("a bare terminal should carry no conventional values, got notes=%q allow=%v", ext.Notes, ext.Allow)
	}
}

// TestExtensionUpdate pins the patch semantics the two callers share: an omitted
// field keeps its stored value and a present-but-empty array clears it.
func TestExtensionUpdate(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	ext := extensions.Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Description = "original"
	saveTestExtension(t, h, &ext)

	// Omitted fields keep their stored value; the description survives a patch
	// that only widens the paths.
	code, row := callExtension(t, mux, http.MethodPut, "/api/extensions/docs",
		`{"paths":["/srv/","/opt/"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /api/extensions/docs = %d, want 200", code)
	}
	if row.Description != "original" {
		t.Errorf("update dropped the stored description: %+v", row)
	}
	after, err := h.Extensions.Store().Load("docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Paths) != 2 {
		t.Errorf("paths = %v, want the two sent", after.Paths)
	}
	// An empty array clears the field — "no whitelisted tools" and "unchanged
	// tools" are different authoring intents and both have to be expressible.
	// The skill preset fills a whitelist, so this is the field that can be
	// cleared; paths cannot, because a skill with no paths is a window onto
	// nothing and Validate refuses it (pinned in the extension package's tests).
	if code, _ := callExtension(t, mux, http.MethodPut, "/api/extensions/docs",
		`{"allow":[]}`); code != http.StatusOK {
		t.Fatalf("clearing allow = %d, want 200", code)
	}
	if after, err := h.Extensions.Store().Load("docs"); err != nil {
		t.Fatal(err)
	} else if len(after.Allow) != 0 {
		t.Errorf("empty array should clear allow, got %v", after.Allow)
	}

	// A name that was never saved is a 404, not a create-by-another-name.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/api/extensions/ghost", strings.NewReader(`{}`)))
	if rr.Code != http.StatusNotFound {
		t.Errorf("PUT unknown extension = %d, want 404", rr.Code)
	}
	// An update can still be rejected by validation.
	if code, _ := callExtension(t, mux, http.MethodPut, "/api/extensions/docs",
		`{"mode":"carrier-pigeon"}`); code != http.StatusBadRequest {
		t.Errorf("invalid mode = %d, want 400", code)
	}
}

// TestExtensionDelete pins that a delete reaches the store the reconciler reads,
// so the card the author removed is gone from the panel's own list.
func TestExtensionDelete(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	ext := extensions.Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	rr := httptest.NewRecorder()
	// A single name answers 204 with no body, the same shape the profile route
	// uses; the batch form above is the one that reports per-name outcomes.
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/extensions/docs", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/extensions/docs = %d, want 204 (%s)", rr.Code, rr.Body.String())
	}
	if rows := getExtensions(t, h); findExtension(rows, "docs") != nil {
		t.Errorf("deleted extension still listed: %+v", rows)
	}

	// Deleting again is a no-op rather than an error: the store treats a missing
	// file as already-deleted (idempotent delete), so a double-click in the panel
	// does not report a failure for work that is already done.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/extensions/docs", nil))
	if rr.Code != http.StatusNoContent {
		t.Errorf("delete a missing extension = %d, want 204 (idempotent)", rr.Code)
	}
	if err := h.Extensions.Store().Delete("docs"); err != nil {
		t.Errorf("store delete should also be idempotent: %v", err)
	}
}

// TestExtensionAuthoringWithoutReconciler pins that a handler without the
// reconciler degrades to a clear refusal instead of a 500: the panel's authoring
// buttons dim, they do not crash the page.
func TestExtensionAuthoringWithoutReconciler(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.Register(mux)

	for _, tc := range []struct {
		method, target, body string
	}{
		{http.MethodPost, "/api/extensions", `{"name":"x","target":"internal","paths":["/srv/"]}`},
		{http.MethodPut, "/api/extensions/x", `{}`},
		{http.MethodDelete, "/api/extensions/x", ""},
	} {
		rr := httptest.NewRecorder()
		var req *http.Request
		if tc.body != "" {
			req = httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
		} else {
			req = httptest.NewRequest(tc.method, tc.target, nil)
		}
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a reconciler = %d, want 503", tc.method, tc.target, rr.Code)
		}
	}
}

// TestExtensionsExportImportRoutes pins the panel's download and upload
// end to end: the file an export produces is the file an import accepts, and a
// name that is already taken comes back renamed rather than overwriting what is
// there.
func TestExtensionsExportImportRoutes(t *testing.T) {
	h := extensionTestHandler(t)
	ext := extensions.Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/docs/"}
	saveTestExtension(t, h, &ext)

	mux := http.NewServeMux()
	h.Register(mux)

	// Export the selection. The response is the download itself, so the browser
	// can hand it straight to a file.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions/batch?names=docs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("export = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/toml") {
		t.Errorf("export content type = %q, want application/toml", ct)
	}
	document := rr.Body.String()
	if !strings.Contains(document, "name = 'docs'") && !strings.Contains(document, `name = "docs"`) {
		t.Fatalf("export should name the template it holds:\n%s", document)
	}

	// Import the same document back into the store that produced it: nothing is
	// overwritten, so the entry arrives under a free name and the answer says so.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/extensions/batch", strings.NewReader(document))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	var result extensions.BatchImportResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode import result: %v (%s)", err, rr.Body.String())
	}
	if result.Imported != 1 || len(result.Renamed) != 1 {
		t.Fatalf("re-importing a stored template should rename it, got %+v", result)
	}
	if _, err := h.Extensions.Store().Load(result.Renamed[0].To); err != nil {
		t.Errorf("the renamed template should be stored: %v", err)
	}
	if got, err := h.Extensions.Store().Load("docs"); err != nil || got.Paths[0] != "/srv/docs/" {
		t.Errorf("the original template should be untouched, got %+v (%v)", got, err)
	}
}

// TestExtensionsImportRouteRefusesBrokenDocument pins that a bad upload is a 400
// with the store's own explanation, not a half-imported file.
func TestExtensionsImportRouteRefusesBrokenDocument(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/extensions/batch", strings.NewReader("[[hosts]]\nname = \"x\"\n"))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("import of a foreign document = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	if list, _ := h.Extensions.Store().List(); len(list) != 0 {
		t.Errorf("a refused import must store nothing, got %v", list)
	}
}

// TestExtensionsBatchDeleteRoute pins the batch contract the panel shares with the
// session and profile routes: a comma-separated name list answers 200 with per-name
// results, one bad entry does not stop the rest, and a single name still answers
// 204.
func TestExtensionsBatchDeleteRoute(t *testing.T) {
	h := extensionTestHandler(t)
	for _, name := range []string{"one", "two"} {
		ext := extensions.Extension{}
		ext.Name = name
		ext.Target = "internal"
		ext.Paths = []string{"/srv/"}
		saveTestExtension(t, h, &ext)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/extensions/one,two,gone", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("batch delete = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Results []struct {
			ID   string `json:"id"`
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode batch results: %v (%s)", err, rr.Body.String())
	}
	if len(body.Results) != 3 {
		t.Fatalf("want one result per name, got %+v", body.Results)
	}
	byID := map[string]string{}
	for _, r := range body.Results {
		if r.OK {
			byID[r.ID] = "ok"
			continue
		}
		byID[r.ID] = r.Code
	}
	if byID["one"] != "ok" || byID["two"] != "ok" {
		t.Errorf("both stored templates should have been deleted, got %+v", byID)
	}
	if byID["gone"] != "extension_not_found" {
		t.Errorf("a name that is already gone should say so, got %+v", byID)
	}
	if list, _ := h.Extensions.Store().List(); len(list) != 0 {
		t.Errorf("store should be empty after the batch, got %v", list)
	}

	// A single name keeps the original contract.
	ext := extensions.Extension{}
	ext.Name = "solo"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/extensions/solo", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("single delete = %d, want 204 (%s)", rr.Code, rr.Body.String())
	}
}

// TestExtensionHoleRoutes pins the two-file contract seen from the browser: a
// template is stored with its holes unfilled, the card says which values it needs
// rather than looking broken, and a value is set by name without anyone naming a
// file.
func TestExtensionHoleRoutes(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rr
	}

	template := `description = "debug the cluster"
mode = "pipe"
command = "{{command}}"
targets = "{{targets}}"

[params.command]
type = "string"
help = "the program that serves MCP over the pipe"

[params.targets]
type = "array"
help = "the hosts this runs on"
`
	created := request(http.MethodPost, "/api/extensions",
		`{"name":"k8s","template":`+jsonMarshal(template)+`}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create template = %d: %s", created.Code, created.Body.String())
	}

	// An installed-but-unfilled extension is listed with what it needs, not as a
	// failure: the two call for different actions from different people.
	var need struct {
		Extensions []struct {
			Name   string   `json:"name"`
			Needs  []string `json:"needs"`
			Status string   `json:"status"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(request(http.MethodGet, "/api/extensions", "").Body.Bytes(), &need); err != nil {
		t.Fatal(err)
	}
	if len(need.Extensions) != 1 {
		t.Fatalf("list = %s", request(http.MethodGet, "/api/extensions", "").Body.String())
	}
	row := need.Extensions[0]
	if row.Status != extensions.StatusIdle {
		t.Errorf("an unfilled extension = %q, want %q (it is waiting, not broken)", row.Status, extensions.StatusIdle)
	}
	if len(row.Needs) != 2 {
		t.Errorf("needs = %v, want both holes", row.Needs)
	}

	// The template view hands back the document WITH its holes. Rendering it here
	// would show a reader an extension that does not exist yet, and lose the holes
	// on the way back in on save.
	tomlView := request(http.MethodGet, "/api/extensions/k8s?format=toml", "")
	if tomlView.Code != http.StatusOK || !strings.Contains(tomlView.Body.String(), `"{{targets}}"`) {
		t.Fatalf("template view should be the hole document:\n%d %s", tomlView.Code, tomlView.Body.String())
	}

	// Preview renders the unsaved template with this installation's values and
	// validates it without changing the stored template.
	preview := request(http.MethodPost, "/api/extensions/k8s/preview", template)
	if preview.Code != http.StatusBadRequest || !strings.Contains(preview.Body.String(), "no value for {{command}}") {
		t.Fatalf("preview before filling holes = %d: %s", preview.Code, preview.Body.String())
	}

	// A value is set by name. The caller never mentions config.local.toml.
	set := request(http.MethodPut, "/api/extensions/k8s",
		`{"values":{"command":"kubectl-mcp","targets":[{"host":"pi","forward":[{"kind":"local","local_port":8443,"remote_port":443}]}]}}`)
	if set.Code != http.StatusOK {
		t.Fatalf("set values = %d: %s", set.Code, set.Body.String())
	}
	got, err := h.Extensions.Store().Load("k8s")
	if err != nil {
		t.Fatalf("the filled template should now build: %v", err)
	}
	if got.Command != "kubectl-mcp" || len(got.Targets) != 1 || got.Targets[0].Host != "pi" {
		t.Fatalf("rendered template = %+v", got)
	}
	// A port that arrived through JSON as 8443 stays an integer: the value file must
	// not hold 8443.0, which would fail to read back into an int field.
	if got.Targets[0].Forwards[0].LocalPort != 8443 {
		t.Errorf("forward port = %d, want 8443", got.Targets[0].Forwards[0].LocalPort)
	}

	preview = request(http.MethodPost, "/api/extensions/k8s/preview", template)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `command = "kubectl-mcp"`) || strings.Contains(preview.Body.String(), "{{command}}") {
		t.Fatalf("preview should return the rendered TOML: %d %s", preview.Code, preview.Body.String())
	}
	if again, err := h.Extensions.Store().Template("k8s"); err != nil || string(again) != template {
		t.Fatalf("preview must not save its template: %v", err)
	}

	// A value the template never asked for is refused rather than stored: it would
	// sit in the values file forever, invisible, looking like a setting that works.
	bad := request(http.MethodPut, "/api/extensions/k8s", `{"values":{"nope":"x"}}`)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("unknown value name = %d, want 400 (%s)", bad.Code, bad.Body.String())
	}

	// Replacing the template keeps this installation's values. That is the whole
	// reason there are two files.
	editedTemplate := strings.Replace(template, "the program that serves MCP over the pipe", "the program", 1)
	edited := request(http.MethodPut, "/api/extensions/k8s",
		`{"template":`+jsonMarshal(editedTemplate)+`}`)
	if edited.Code != http.StatusOK {
		t.Fatalf("replace template = %d: %s", edited.Code, edited.Body.String())
	}
	if again, err := h.Extensions.Store().Load("k8s"); err != nil || again.Command != "kubectl-mcp" {
		t.Errorf("a template upgrade must survive with its values: %+v (%v)", again, err)
	}
}

func jsonMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestExtensionsTOMLViewRoutes pins the editor's file view: the text the TOML view
// reads is the file the store holds, and saving that text back writes the same
// template — including a create, whose name comes from the query because the file
// itself is named by its directory.
func TestExtensionsTOMLViewRoutes(t *testing.T) {
	h := extensionTestHandler(t)
	ext := extensions.Extension{}
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/docs/"}
	saveTestExtension(t, h, &ext)

	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions/docs?format=toml", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("toml view = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/toml") {
		t.Errorf("toml view content type = %q", ct)
	}
	document := rr.Body.String()
	if !strings.Contains(document, "paths = [") {
		t.Fatalf("the file view should be the template's own TOML:\n%s", document)
	}
	if strings.Contains(document, "name =") {
		t.Errorf("a stored template has no name key (the directory is the name):\n%s", document)
	}

	// Saving the text it just handed over is a no-op the reconciler still hears:
	// the template stays, and the answer is the usual summary.
	edited := strings.Replace(document, `/srv/docs/`, `/srv/window/`, 1)
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/extensions/docs", strings.NewReader(edited))
	req.Header.Set("Content-Type", "application/toml")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("toml save = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	got, err := h.Extensions.Store().Load("docs")
	if err != nil {
		t.Fatal(err)
	}
	if got.Paths[0] != "/srv/window/" {
		t.Errorf("paths = %v, want the edited window", got.Paths)
	}

	// A document that names something else is refused: the name is the directory.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/extensions/docs", strings.NewReader("name = \"other\"\ntarget = \"internal\"\npaths = [\"/srv/\"]\n"))
	req.Header.Set("Content-Type", "application/toml")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a mismatched name = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}

	// A create from the file view: the name rides in the query, and a taken name is
	// a conflict rather than an overwrite.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/extensions?name=notes", strings.NewReader("target = \"internal\"\npaths = [\"/srv/notes/\"]\n"))
	req.Header.Set("Content-Type", "application/toml")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("toml create = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	if _, err := h.Extensions.Store().Load("notes"); err != nil {
		t.Errorf("the created template should be stored: %v", err)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/extensions?name=notes", strings.NewReader("target = \"internal\"\npaths = [\"/srv/\"]\n"))
	req.Header.Set("Content-Type", "application/toml")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("creating over a stored name = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
}
