package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// staticDocPaths are the agent-facing documents published by the embedded
// static server. They must be fetchable over plain HTTP (curl), which is the
// no-MCP integration path.
var staticDocPaths = []string{
	"/api.md",
	"/skills.md",
}

// TestStaticDocsServed verifies the embedded docs are reachable at their paths.
func TestStaticDocsServed(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{}).Register(mux)

	for _, p := range staticDocPaths {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, rr.Code)
			continue
		}
		if body := rr.Body.String(); len(body) < 200 {
			t.Errorf("GET %s returned %d bytes, want a real document", p, len(body))
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("GET %s Content-Type = %q, want text/markdown", p, ct)
		}
	}
}

// TestMarkdownContentTypeIsPlatformIndependent pins the .md media type even when
// the host's mime table has no .md entry (a bare Linux container serves
// text/plain by sniffing, Windows text/markdown): markdownContentType must win
// over whatever http.ServeContent would sniff.
func TestMarkdownContentTypeIsPlatformIndependent(t *testing.T) {
	// The inner handler mimics http.FileServer serving a file whose extension
	// nobody agrees on: on a mime-table system (.bin → application/octet-stream
	// in Debian's /etc/mime.types) ServeContent reports that; on a bare machine it
	// sniffs text/plain. Either way the wrapper must win for /api.md and stay
	// out of the way otherwise.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "doc.bin", time.Time{}, strings.NewReader("# doc\n"))
	})
	h := markdownContentType(inner)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api.md", nil))
	if ct := rr.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("GET /api.md Content-Type = %q, want text/markdown; charset=utf-8", ct)
	}

	// Non-markdown assets keep whatever the inner handler decided — whatever
	// that is on this host (text/plain sniffed, or octet-stream from a mime
	// table), it must not be the wrapper's markdown type.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if ct := rr.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("GET /index.html Content-Type = %q, want the wrapper to leave non-.md assets untouched", ct)
	}
}

// TestSkillFrontmatter validates the downloadable skill file against the Agent
// Skills requirements (name + description in YAML frontmatter).
func TestSkillFrontmatter(t *testing.T) {
	b, err := readAsset("skills.md")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout (core.autocrlf) hands go:embed CRLF; the frontmatter
	// delimiter is defined as a line, so normalize before checking it.
	b = strings.ReplaceAll(b, "\r\n", "\n")
	if !strings.HasPrefix(b, "---\n") {
		t.Fatal("SKILL.md must start with YAML frontmatter")
	}
	end := strings.Index(b[4:], "\n---")
	if end < 0 {
		t.Fatal("SKILL.md frontmatter is not terminated")
	}
	front := b[4 : 4+end]
	for _, field := range []string{"name: termcp", "description:"} {
		if !strings.Contains(front, field) {
			t.Errorf("SKILL.md frontmatter misses %q", field)
		}
	}
	// The description decides whether an agent loads the skill, so it must name
	// the locator syntax users paste ("open termcp://rock64").
	if !strings.Contains(front, "termcp://") {
		t.Error("SKILL.md description must mention termcp:// locators")
	}
	// The body must teach how to act on a locator over plain HTTP.
	for _, want := range []string{"termcp://", "/api/resolve"} {
		if !strings.Contains(b, want) {
			t.Errorf("SKILL.md body misses %q", want)
		}
	}
	// Install instructions must name the directory Claude Code actually reads
	// (it does not read ~/.agents/skills) plus the shared convention.
	for _, want := range []string{"~/.claude/skills/termcp/SKILL.md", "~/.agents/skills/termcp/SKILL.md"} {
		if !strings.Contains(b, want) {
			t.Errorf("SKILL.md install note misses %q", want)
		}
	}
}

// TestShellIORoutes exercises the REST terminal-I/O endpoints against a live
// internal session: input, named key, and resize.
func TestShellIORoutes(t *testing.T) {
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

	sess, err := sessMgr.Create(session.Config{
		// Remote nil = the built-in loopback sshd (the "internal" profile).
		Mode: api.ModePTY,
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	shellID := sess.PrimaryShellID()

	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	post := func(path, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rr, req)
		return rr
	}

	if rr := post("/api/shells/"+shellID+"/input", `{"text":"echo termcp-io-probe","press_enter":true}`); rr.Code != http.StatusOK {
		t.Fatalf("input = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("/api/shells/"+shellID+"/key", `{"key":"enter"}`); rr.Code != http.StatusOK {
		t.Fatalf("key = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("/api/shells/"+shellID+"/resize", `{"rows":40,"cols":120}`); rr.Code != http.StatusOK {
		t.Fatalf("resize = %d: %s", rr.Code, rr.Body.String())
	}

	// The echoed command must show up in the retained output. The login shell
	// may take seconds to start (rc/profile), so the deadline is generous.
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _, err := sess.PrimaryShell().OutputByteRange(0, 1<<20)
		if err == nil && strings.Contains(string(out), "termcp-io-probe") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never contained the probe: %q", string(out))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Error paths: unknown shell, bad JSON, missing key.
	if rr := post("/api/shells/nope/input", `{"text":"x"}`); rr.Code != http.StatusNotFound {
		t.Errorf("unknown shell = %d, want 404", rr.Code)
	}
	if rr := post("/api/shells/"+shellID+"/input", `{`); rr.Code != http.StatusBadRequest {
		t.Errorf("bad JSON = %d, want 400", rr.Code)
	}
	if rr := post("/api/shells/"+shellID+"/key", `{}`); rr.Code != http.StatusBadRequest {
		t.Errorf("missing key = %d, want 400", rr.Code)
	}
	if rr := post("/api/shells/"+shellID+"/resize", `{"rows":0,"cols":80}`); rr.Code != http.StatusBadRequest {
		t.Errorf("zero rows = %d, want 400", rr.Code)
	}
}

// TestApiPageShowsSkillDownload keeps the Web UI's API/MCP page pointing at the
// skill download address: users should not have to assemble the URL by hand.
func TestApiPageShowsSkillDownload(t *testing.T) {
	b, err := readAsset("api.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="url-skill"`,         // skill URL element (filled from origin by the page script)
		`id="cfg-skill-install"`, // copy-ready install command
		`href="/skills.md"`,      // direct download link
		`href="/api.md"`,         // document link
		`var skill = origin + '/skills.md'`,
		`~/.claude/skills/`,      // Claude Code's skills dir (the only one CC actually reads)
		`~/.agents/skills/`,      // shared convention for other agents
		`"$DIR/termcp/SKILL.md"`, // install target: folder name = skill name
	} {
		if !strings.Contains(b, want) {
			t.Errorf("api.html misses %s", want)
		}
	}
}

// readAsset reads one embedded asset as text.
func readAsset(p string) (string, error) {
	b, err := fs.ReadFile(Assets(), p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestSyncedDocsMatchSource keeps the served copies of the repository docs in
// sync with docs/. Line endings are normalized so a CRLF checkout does not
// produce false failures; content drift still fails.
func TestSyncedDocsMatchSource(t *testing.T) {
	pairs := []struct{ src, asset string }{
		{"../../docs/api.md", "api.md"},
	}
	for _, p := range pairs {
		want, err := os.ReadFile(p.src)
		if err != nil {
			t.Skipf("source doc %s not available: %v", p.src, err)
		}
		got, err := fs.ReadFile(Assets(), p.asset)
		if err != nil {
			t.Fatalf("asset %s missing: %v (run `make sync-assets`)", p.asset, err)
		}
		if normalizeNL(string(want)) != normalizeNL(string(got)) {
			t.Errorf("internal/webui/assets/%s is stale; run `make sync-assets`", p.asset)
		}
	}
}

func normalizeNL(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
