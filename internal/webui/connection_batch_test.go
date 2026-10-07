package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestConnectionBatchHTTPAndTemporaryFlag(t *testing.T) {
	store := sshconfig.NewStore(t.TempDir())
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)

	profile := "kind = \"remote\"\nhost = \"sample.example\"\nuser = \"tester\"\npassword = \"placeholder\"\n"
	put := httptest.NewRecorder()
	mux.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/api/connections/one?temporary=true", strings.NewReader(profile)))
	if put.Code != http.StatusNoContent || !store.IsTemporary("one") {
		t.Fatalf("temporary PUT: status=%d body=%s", put.Code, put.Body.String())
	}
	list := httptest.NewRecorder()
	mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/connections", nil))
	var response struct {
		Connections []connectionSummary `json:"connections"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Connections) != 2 || !response.Connections[1].Temporary {
		t.Fatalf("list omitted temporary flag: %+v", response.Connections)
	}
	export := httptest.NewRecorder()
	mux.ServeHTTP(export, httptest.NewRequest(http.MethodGet, "/api/connections/batch", nil))
	if export.Code != http.StatusOK || !strings.Contains(export.Body.String(), `name = "one"`) {
		t.Fatalf("export: status=%d body=%s", export.Code, export.Body.String())
	}
	other := sshconfig.NewStore(t.TempDir())
	h2 := &Handler{SSH: other}
	mux2 := http.NewServeMux()
	h2.Register(mux2)
	imported := httptest.NewRecorder()
	mux2.ServeHTTP(imported, httptest.NewRequest(http.MethodPost, "/api/connections/batch?temporary=true", strings.NewReader(export.Body.String())))
	if imported.Code != http.StatusCreated || !other.IsTemporary("one") {
		t.Fatalf("import: status=%d body=%s", imported.Code, imported.Body.String())
	}
	duplicate := httptest.NewRecorder()
	mux2.ServeHTTP(duplicate, httptest.NewRequest(http.MethodPost, "/api/connections/batch?temporary=true", strings.NewReader(export.Body.String())))
	if duplicate.Code != http.StatusCreated || !other.IsTemporary("one-2") {
		t.Fatalf("duplicate import: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	var renamed sshconfig.BatchImportResult
	if err := json.Unmarshal(duplicate.Body.Bytes(), &renamed); err != nil {
		t.Fatal(err)
	}
	if renamed.Imported != 1 || len(renamed.Renamed) != 1 || renamed.Renamed[0] != (sshconfig.BatchRename{From: "one", To: "one-2"}) {
		t.Fatalf("duplicate rename response: %+v", renamed)
	}
}

// newConnectionTestHandler wires a handler over a store holding profiles one
// and two (persistent) for the batch-endpoint tests below.
func newConnectionTestHandler(t *testing.T) (*Handler, *sshconfig.Store, *http.ServeMux) {
	t.Helper()
	store := sshconfig.NewStore(t.TempDir())
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	profile := "kind = \"remote\"\nhost = \"sample.example\"\nuser = \"tester\"\npassword = \"placeholder\"\n"
	for _, name := range []string{"one", "two"} {
		put := httptest.NewRecorder()
		mux.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/api/connections/"+name, strings.NewReader(profile)))
		if put.Code != http.StatusNoContent {
			t.Fatalf("PUT %s: status=%d body=%s", name, put.Code, put.Body.String())
		}
	}
	return h, store, mux
}

// The comma-batch delete reports one outcome per name, deletes the real ones,
// and refuses the built-in profile without stopping the rest — a missing name
// reports connection_not_found instead of pretending to have deleted it.
func TestConnectionBatchDeleteHTTP(t *testing.T) {
	_, store, mux := newConnectionTestHandler(t)
	del := httptest.NewRecorder()
	mux.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/api/connections/one,two,internal,ghost", nil))
	if del.Code != http.StatusOK {
		t.Fatalf("batch delete: status=%d body=%s", del.Code, del.Body.String())
	}
	var response struct {
		Results []struct {
			ID   string `json:"id"`
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		} `json:"results"`
	}
	if err := json.Unmarshal(del.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		ok   bool
		code string
	}{
		"one":      {true, ""},
		"two":      {true, ""},
		"internal": {false, "reserved_profile"},
		"ghost":    {false, "connection_not_found"},
	}
	if len(response.Results) != len(want) {
		t.Fatalf("batch delete results: %+v", response.Results)
	}
	for _, r := range response.Results {
		w := want[r.ID]
		if r.OK != w.ok || r.Code != w.code {
			t.Fatalf("result for %q: ok=%v code=%q, want ok=%v code=%q", r.ID, r.OK, r.Code, w.ok, w.code)
		}
	}
	for _, name := range []string{"one", "two"} {
		if _, err := store.Load(name); err == nil {
			t.Fatalf("profile %q survived batch delete", name)
		}
	}
	if _, err := store.Load("internal"); err != nil {
		t.Fatalf("internal profile must survive: %v", err)
	}
}

// names=a,b filters the exported TOML to exactly the named profiles; a name
// that no longer resolves is skipped rather than failing the download, and no
// names parameter keeps exporting everything.
func TestConnectionExportSelectedHTTP(t *testing.T) {
	_, _, mux := newConnectionTestHandler(t)
	one := httptest.NewRecorder()
	mux.ServeHTTP(one, httptest.NewRequest(http.MethodGet, "/api/connections/batch?names=one", nil))
	if one.Code != http.StatusOK || !strings.Contains(one.Body.String(), `name = "one"`) || strings.Contains(one.Body.String(), `name = "two"`) {
		t.Fatalf("export one: status=%d body=%s", one.Code, one.Body.String())
	}
	stale := httptest.NewRecorder()
	mux.ServeHTTP(stale, httptest.NewRequest(http.MethodGet, "/api/connections/batch?names=one,ghost", nil))
	if stale.Code != http.StatusOK || !strings.Contains(stale.Body.String(), `name = "one"`) {
		t.Fatalf("export with stale names: status=%d body=%s", stale.Code, stale.Body.String())
	}
	reserved := httptest.NewRecorder()
	mux.ServeHTTP(reserved, httptest.NewRequest(http.MethodGet, "/api/connections/batch?names=one,internal", nil))
	if reserved.Code != http.StatusBadRequest {
		t.Fatalf("export naming internal: status=%d body=%s", reserved.Code, reserved.Body.String())
	}
	all := httptest.NewRecorder()
	mux.ServeHTTP(all, httptest.NewRequest(http.MethodGet, "/api/connections/batch", nil))
	if all.Code != http.StatusOK || !strings.Contains(all.Body.String(), `name = "one"`) || !strings.Contains(all.Body.String(), `name = "two"`) {
		t.Fatalf("export all: status=%d body=%s", all.Code, all.Body.String())
	}
}
