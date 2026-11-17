package extensions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// newTestReconcilerIn builds a reconciler over an existing data directory, the way
// the server does at startup: it must not need a running SSH endpoint to LOAD.
func newTestReconcilerIn(t *testing.T, dir string) *Reconciler {
	t.Helper()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})
	return NewReconciler(NewStore(dir), sessMgr, msgMgr, sshconfig.NewStore(dir), forward.NewForwardManager())
}

// TestADisabledTemplateIsStillDisabledAfterARestart pins the second finding of
// issue #152, and pins it as an INVARIANT rather than as a reproduction.
//
// The report was that one template of 48 came back `enabled = true` after a restart,
// with nothing in the log about writing it. Nothing in this package writes a template
// on load — the reconciler only reads, and the two write paths (the MCP extension
// tool and the WebUI's routes) both preserve `enabled` when the caller does not name
// it — so the flip cannot come from a restart. What a test CAN do is hold the
// invariant down from the outside: seed templates disabled, bring the whole
// reconciler up, and require every file to be byte-identical afterwards.
//
// That is the useful half. If a future change makes loading rewrite a template, this
// fails with the file that changed; without it, the only symptom is a user noticing
// that a parked extension came back on.
func TestADisabledTemplateIsStillDisabledAfterARestart(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	const n = 48
	before := make(map[string]string, n)
	for i := 0; i < n; i++ {
		name := "ext-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		body := "target = \"internal\"\nmode = \"pipe\"\nenabled = false\ncommand = \"true\"\n"
		writeRawTemplate(t, store, name, body)
		before[name] = body
	}

	// The whole startup path: a reconciler built over this directory, the way the
	// server builds one. It may probe (nothing here is runnable), but it may not
	// write.
	r := newTestReconcilerIn(t, dir)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)

	names, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != n {
		t.Fatalf("the store lists %d templates, want %d", len(names), n)
	}
	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(dir, "extensions", name, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != before[name] {
			t.Errorf("template %q was rewritten while nothing asked for it:\n got: %q\nwant: %q",
				name, got, before[name])
		}
		ext, err := store.Load(name)
		if err != nil {
			t.Fatalf("template %q stopped loading: %v", name, err)
		}
		if ext.IsEnabled() {
			t.Errorf("template %q came back enabled; it was stored disabled", name)
		}
	}
}

// writeRawTemplate writes a config.toml straight to disk, the way a user who edits
// the data directory by hand does — the state the report was filed from.
func writeRawTemplate(t *testing.T, store *Store, name, body string) {
	t.Helper()
	dir := filepath.Join(store.root(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
