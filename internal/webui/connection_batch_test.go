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
