package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// A download resolves its byte window in one of two ways, and which one applies
// depends on the session, not on the request: a session with an SSH transport
// streams over SFTP, and only a session without one falls back to serving the
// file from the termcp host. These tests drive the route through the real mux
// with a real internal session, because the status codes and the Content-Range
// only exist at the HTTP boundary and are what a resuming client depends on.

// downloadEnv is a running internal session wired to the real mux. An internal
// session has an SSH loopback transport (the process is reached over an
// in-memory sshd), so downloads take the SFTP path - the same code a remote
// session uses. That is deliberate: it means these tests cover the path that
// production actually reaches.
type downloadEnv struct {
	mux     *http.ServeMux
	sessMgr *session.Manager
	sess    *session.Session
	dir     string
}

func newDownloadEnv(t *testing.T) *downloadEnv {
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
	sessMgr := session.NewManager(message.NewManager(store), store, srv)
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
	return &downloadEnv{mux: mux, sessMgr: sessMgr, sess: sess, dir: dir}
}

// file writes a payload into the session's directory and returns its path.
func (e *downloadEnv) file(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// get performs a download request for path, with optional query and Range.
func (e *downloadEnv) get(t *testing.T, path, query, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/sessions/" + e.sess.ID + "/files/download?path=" + path
	if query != "" {
		url += "&" + query
	}
	r := httptest.NewRequest("GET", url, nil)
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, r)
	return rr
}

const downloadPayload = "abcdefghijklmnopqrstuvwxyz"

func TestDownloadFileMissingPathIsRejected(t *testing.T) {
	e := newDownloadEnv(t)

	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/sessions/"+e.sess.ID+"/files/download", nil))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a download with no path has nothing to serve", rr.Code)
	}
}

// TestDownloadFileWholeFile is the plain case: no Range, no ?offset=, so the
// whole file must arrive with a Content-Length a browser can save against.
func TestDownloadFileWholeFile(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)

	rr := e.get(t, src, "", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != downloadPayload {
		t.Errorf("body = %q, want the whole file", got)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "26" {
		t.Errorf("Content-Length = %q, want 26", cl)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream: a download is not rendered", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "payload.bin") {
		t.Errorf("Content-Disposition = %q, want it to name payload.bin", cd)
	}
	if ar := rr.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes: a resumable client needs it", ar)
	}
}

// TestDownloadFilePartialWindow covers the ?offset=&length= path. The window must
// be exact, and a request that asks for one must be answered 200 with the window
// itself, not 206: it is a fresh download of a slice, not a range of a resource.
func TestDownloadFilePartialWindow(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)

	rr := e.get(t, src, "offset=10&length=5", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "klmno" {
		t.Errorf("body = %q, want klmno (bytes 10..14)", got)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "5" {
		t.Errorf("Content-Length = %q, want 5", cl)
	}
}

// TestDownloadFileLengthRunsPastEOF pins the clamping: a window that starts near
// the end and asks for more than remains is shortened rather than refused. That
// is what a resuming download does when it asks again after the file shrank.
func TestDownloadFileLengthRunsPastEOF(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)

	rr := e.get(t, src, "offset=20&length=1000", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "uvwxyz" {
		t.Errorf("body = %q, want uvwxyz (the remainder after byte 20)", got)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "6" {
		t.Errorf("Content-Length = %q, want the clamped 6", cl)
	}
}

// TestDownloadFileRangeIsPartial pins the Range contract, which is what a browser
// video player and curl -C both rely on: 206, the window as the body, and a
// Content-Range that states where the window sits in the whole file.
func TestDownloadFileRangeIsPartial(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)

	rr := e.get(t, src, "", "bytes=0-3")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "abcd" {
		t.Errorf("body = %q, want abcd", got)
	}
	if cr := rr.Header().Get("Content-Range"); cr != "bytes 0-3/26" {
		t.Errorf("Content-Range = %q, want bytes 0-3/26", cr)
	}
}

// TestDownloadFileSuffixRange covers "bytes=-N", the form a client uses when it
// wants the tail of a file whose length it does not know - the case that makes a
// log download possible without first asking for the size.
func TestDownloadFileSuffixRange(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)

	rr := e.get(t, src, "", "bytes=-4")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "wxyz" {
		t.Errorf("body = %q, want wxyz (the last four bytes)", got)
	}
	if cr := rr.Header().Get("Content-Range"); cr != "bytes 22-25/26" {
		t.Errorf("Content-Range = %q, want bytes 22-25/26", cr)
	}
}

// TestDownloadFileUnsatisfiableRange is the one case that must not be a 200 with
// an empty body: a client asking for bytes past the end has to be told 416, or it
// will treat silence as a truncated file and retry forever.
func TestDownloadFileUnsatisfiableRange(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)

	rr := e.get(t, src, "", "bytes=999-1000")

	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416 (body: %s)", rr.Code, rr.Body.String())
	}
	if cr := rr.Header().Get("Content-Range"); cr != "bytes */26" {
		t.Errorf("Content-Range = %q, want bytes */26: it tells the client the real size", cr)
	}
}

// TestDownloadFileDirectoryIsRejected pins that a directory is a 400, not a
// confusing empty 200: SFTP can list a directory but not stream one.
func TestDownloadFileDirectoryIsRejected(t *testing.T) {
	e := newDownloadEnv(t)

	rr := e.get(t, e.dir, "", "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a directory (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "directory") {
		t.Errorf("body = %q, want it to say the path is a directory", rr.Body.String())
	}
}

// TestDownloadFileMissingIsNotFound pins the status for a path that is not there,
// and that the answer is the JSON error the UI can display - not a plain-text
// one it would have to special-case.
func TestDownloadFileMissingIsNotFound(t *testing.T) {
	e := newDownloadEnv(t)
	missing := filepath.Join(e.dir, "gone.bin")

	rr := e.get(t, missing, "", "")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "error") {
		t.Errorf("body = %q, want a JSON error the UI can show", rr.Body.String())
	}
}

// TestDownloadFileDeadSessionIsConflict pins the close-vs-delete distinction on
// this route: a session that has exited keeps its output readable but has no
// transport left, so a download must answer 409 with a pointer to the endpoint
// that does work, rather than 404 (which would look like a missing file).
func TestDownloadFileDeadSessionIsConflict(t *testing.T) {
	e := newDownloadEnv(t)
	src := e.file(t, "payload.bin", downloadPayload)
	e.sessMgr.Terminate(e.sess.ID, true, 0)

	rr := e.get(t, src, "", "")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a closed session (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "/output-range") {
		t.Errorf("body = %q, want it to point at /output-range, which still works", rr.Body.String())
	}
}

// TestDownloadFileUnknownSessionIsNotFound pins that an unknown id is a 404 and
// not a 409: there is a difference between a session that never existed and one
// that existed and has ended.
func TestDownloadFileUnknownSessionIsNotFound(t *testing.T) {
	e := newDownloadEnv(t)

	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/sessions/does-not-exist/files/download?path=/tmp/x", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestDownloadRangeHelper pins the precedence rule the extracted helper decides,
// on its own, without a server: the Range header wins over the query parameters,
// and the query parameters are read only when it is absent. The bounds in the
// header are deliberately NOT parsed here - that is parseRange's job later, for
// the SFTP path, and http.ServeFile's for the local one - so the helper must come
// back with zeros rather than a half-decoded window.
func TestDownloadRangeHelper(t *testing.T) {
	t.Run("query parameters when no Range", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?offset=7&length=3", nil)
		offset, length, isRange := downloadRange(r)
		if offset != 7 || length != 3 || isRange {
			t.Errorf("got offset=%d length=%d isRange=%v, want 7/3/false", offset, length, isRange)
		}
	})

	t.Run("Range wins and leaves the query alone", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?offset=7&length=3", nil)
		r.Header.Set("Range", "bytes=100-199")
		offset, length, isRange := downloadRange(r)
		if !isRange {
			t.Error("isRange = false with a Range header present")
		}
		if offset != 0 || length != 0 {
			t.Errorf("got offset=%d length=%d, want 0/0 (the header is decoded later)", offset, length)
		}
	})

	t.Run("unparsable query values stay zero", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?offset=abc&length=", nil)
		offset, length, isRange := downloadRange(r)
		if offset != 0 || length != 0 || isRange {
			t.Errorf("got offset=%d length=%d isRange=%v, want zeros", offset, length, isRange)
		}
	})
}
