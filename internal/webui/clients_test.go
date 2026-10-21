package webui

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The client list answers "who is attached to this instance?" — the question the
// issue asks. Two kinds of caller exist and they are genuinely different: an MCP
// client is an agent driving sessions and announces a name during initialize; a
// Web UI viewer is a browser tab holding a socket. What the tests below pin is
// the part that is invisible in a browser smoke test and easy to get wrong:
//
//   - a viewer is listed with the address it came from, port stripped, so one
//     person on one machine is one row however many ephemeral ports they use;
//   - the watched-session count is read at list time, not captured when the
//     socket opened (it changes constantly, and a stale count would be a lie);
//   - a closed tab disappears;
//   - the MCP half is reported as the MCP server sees it, and an instance with no
//     MCP server says so instead of returning null.

func TestViewerRegistryListsAttachedTabs(t *testing.T) {
	reg := newViewerRegistry()

	watching := 0
	id := reg.add("10.0.0.5:54321", "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0", func() int { return watching })
	if got := reg.list(0); len(got) != 1 {
		t.Fatalf("one tab attached, got %d viewers", len(got))
	} else {
		v := got[0]
		// The port is per-connection noise: the same person reloading gets a new
		// one, and listing it would make one viewer look like many.
		if v.Addr != "10.0.0.5" {
			t.Errorf("addr = %q, want the host without its port", v.Addr)
		}
		if v.UserAgent == "" {
			t.Error("the user agent identifies the tab; it must be listed")
		}
		if v.Since == "" {
			t.Error("a viewer needs a since timestamp")
		}
	}

	// The count follows what the tab is streaming now, not what it was holding
	// when it attached.
	watching = 3
	if got := reg.list(0)[0].Watching; got != 3 {
		t.Errorf("watching = %d, want the current 3 (the count must be read, not captured)", got)
	}
	watching = 0
	if got := reg.list(0)[0].Watching; got != 0 {
		t.Errorf("watching = %d, want 0 after the tab stopped streaming", got)
	}

	// Oldest first, so the panel does not reshuffle on every frame.
	time.Sleep(2 * time.Millisecond)
	reg.add("10.0.0.9:1234", "second", func() int { return 1 })
	got := reg.list(0)
	if len(got) != 2 || got[0].Addr != "10.0.0.5" {
		t.Fatalf("viewers must be oldest-first; got %+v", got)
	}

	// A closed tab is gone.
	reg.remove(id)
	got = reg.list(0)
	if len(got) != 1 || got[0].Addr != "10.0.0.9" {
		t.Fatalf("the closed tab is still listed: %+v", got)
	}
}

// A user agent is attacker-controlled and unbounded; the panel must not render it
// whole.
func TestViewerUserAgentIsBounded(t *testing.T) {
	reg := newViewerRegistry()
	reg.add("1.2.3.4:1", string(make([]byte, 4096)), nil)
	if got := reg.list(0)[0].UserAgent; len(got) > 200 {
		t.Fatalf("user agent was not truncated: %d bytes", len(got))
	}
}

// The MCP half reaches the API through the Handler's hook, and an instance with no
// MCP server must answer with an empty list rather than null — a client rendering
// the response should not have to special-case absent keys.
func TestHandlerListsClients(t *testing.T) {
	h := &Handler{}
	if got := h.Viewers(); len(got) != 0 {
		t.Errorf("a fresh handler has no viewers, got %d", len(got))
	}

	rr := httptest.NewRecorder()
	h.handleListClients(rr, httptest.NewRequest(http.MethodGet, "/api/clients", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	served := decodeBody(t, rr)
	if _, ok := served["mcp"].([]any); !ok {
		t.Errorf("mcp must be a JSON array, got %T", served["mcp"])
	}
	if _, ok := served["webui"].([]any); !ok {
		t.Errorf("webui must be a JSON array, got %T", served["webui"])
	}

	// With an MCP server, its clients are reported as-is.
	h.MCPConnections = func() []any {
		return []any{map[string]any{
			"session_id":   "sess-abc",
			"name":         "claude-ai",
			"version":      "0.1.0",
			"namespace":    "ns:alice",
			"connected_at": "2026-10-17T00:00:00Z",
		}}
	}
	rr = httptest.NewRecorder()
	h.handleListClients(rr, httptest.NewRequest(http.MethodGet, "/api/clients", nil))
	served = decodeBody(t, rr)
	rows, _ := served["mcp"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 MCP client, got %d", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	for field, want := range map[string]string{
		"name": "claude-ai", "version": "0.1.0",
		"namespace": "ns:alice", "session_id": "sess-abc",
	} {
		if got, _ := row[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// decodeBody unmarshals a response body for the assertions above.
func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON %q: %v", rr.Body.String(), err)
	}
	return m
}

// The server marks the caller's own row (`self`).
//
// This started as the fix for a count badge that included the page itself — a
// lone tab always read "1", and the empty state was unreachable. The badge is
// gone (see TestClientsTriggerHasNoCountBadge), but the mark it needed is not:
// the empty state is still about the clients BESIDES this page, so the panel has
// to know which row is yours in order to say "nothing connected yet" while
// listing one row, and in order to label that row "this tab" rather than
// leaving it a mystery extra client.
func TestViewerListMarksTheCallersOwnTab(t *testing.T) {
	reg := newViewerRegistry()
	self := reg.add("10.0.0.5:1111", "this tab", func() int { return 0 })
	other := reg.add("10.0.0.9:2222", "other tab", func() int { return 2 })

	rows := reg.list(self)
	if len(rows) != 2 {
		t.Fatalf("the caller's own row must still be listed (that is how a page shows the list works), got %d rows", len(rows))
	}
	var marked, unmarked int
	for _, v := range rows {
		if v.Self {
			marked++
			// The marked row must be the one that asked, not merely the first.
			if v.UserAgent != "this tab" {
				t.Errorf("the wrong row is marked as self: %+v", v)
			}
		} else {
			unmarked++
		}
	}
	if marked != 1 || unmarked != 1 {
		t.Fatalf("exactly one row must be self; got %d marked, %d not", marked, unmarked)
	}

	// A caller with no id (the REST route, which is nobody's tab) marks nothing.
	for _, v := range reg.list(0) {
		if v.Self {
			t.Errorf("list(0) must not mark any row as self: %+v", v)
		}
	}

	// The ids are identities, not positions: removing a tab must not renumber
	// the others, or a page holding its own id would start matching a stranger.
	before := map[string]bool{}
	for _, v := range reg.list(0) {
		before[v.ID] = true
	}
	reg.remove(other)
	after := reg.list(self)
	if len(after) != 1 {
		t.Fatalf("one tab left, got %d", len(after))
	}
	if !before[after[0].ID] {
		t.Errorf("the surviving row's id changed after another tab closed (%q was not in %v); ids must be stable", after[0].ID, before)
	}
	if !after[0].Self {
		t.Error("the surviving row is still the caller's own tab and must stay marked")
	}
}

// Every theme must get the panel's styling, including the built-in ones that
// ship their own CSS directory.
//
// A theme folder may override any chunk, and the file server prefers the theme's
// copy — which is the feature. The trap is a theme shipping an *old* copy of a
// shared chunk: it silently shadows every later addition, so a component added to
// the shared stylesheet renders unstyled in that theme only. That is what happened
// to the clients panel in cyberpunk (a stale base.css), and the same fork had
// already swallowed the update pill and the session namespace chip.
//
// So the rule is not "no copies" — it is "a copy must not be a stale fork". A
// theme may still restyle a component by declaring its own rules in theme.css,
// which is where theme-specific appearance belongs.
func TestEveryThemeStylesTheClientsPanel(t *testing.T) {
	// The shared chunk owns the layout: it must define the panel at all.
	shared := readAssetLF(t, "static/css/base.css")
	for _, sel := range []string{".clients-menu", ".clients-btn", ".clients-panel", ".clients-row", ".clients-badge"} {
		if !strings.Contains(shared, sel+" ") && !strings.Contains(shared, sel+"{") && !strings.Contains(shared, sel+",") && !strings.Contains(shared, sel+" :") {
			t.Errorf("static/css/base.css no longer styles %s; the panel would be unstyled everywhere", sel)
		}
	}

	themes, err := fs.Glob(Assets(), "themes/*/theme.css")
	if err != nil || len(themes) == 0 {
		t.Fatalf("no themes found: %v", err)
	}
	for _, themeCSS := range themes {
		dir := strings.TrimSuffix(themeCSS, "/theme.css")
		name := strings.TrimPrefix(dir, "themes/")

		/* Structural chunks describe how the page is built, so a theme that forks
		   one loses every later addition to it — the failure was exactly this, with
		   cyberpunk's base.css predating the panel, the update pill and the
		   namespace chip. These chunks have no legitimate reason to diverge.
		   Palette and appearance chunks (tokens, theme, workspace) are the theme's
		   own business and are deliberately not checked: their whole point is to
		   differ. */
		for _, file := range []string{"base.css", "approval.css", "terminal.css", "timeline.css"} {
			sharedPath := "static/css/" + file
			if _, err := fs.Stat(Assets(), sharedPath); err != nil {
				continue
			}
			themeCopy, err := readAsset(dir + "/assets/static/css/" + file)
			if err != nil {
				continue // no override: the shared chunk is used as-is, which is right
			}
			sharedBody, err := readAsset(sharedPath)
			if err != nil {
				continue
			}
			if missing := missingLines(sharedBody, themeCopy); len(missing) > 0 {
				t.Errorf("%s overrides the structural chunk static/css/%s but is missing %d line(s) the shared "+
					"copy has; every component added since that copy was taken renders unstyled in this theme "+
					"only. Delete the override to inherit the shared chunk (theme.css is the place for "+
					"theme-specific appearance). First missing: %q",
					name, file, len(missing), missing[0])
			}
		}
	}
}

// missingLines returns the non-blank lines of shared that do not appear in copy.
// It is a whole-line comparison, not a diff: a stale fork drops whole rules, and
// a rule split across lines would still match as long as its text is unchanged.
func missingLines(shared, copy string) []string {
	have := make(map[string]bool, 4096)
	for _, line := range strings.Split(copy, "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, line := range strings.Split(shared, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "}" || trimmed == "{" {
			continue
		}
		if !have[trimmed] {
			missing = append(missing, trimmed)
		}
	}
	return missing
}

// The header trigger carries no count badge.
//
// It used to: a number beside the label saying how many clients were attached
// besides this page. That number is almost always 0 or 1, it had to be explained
// (which tabs count, why "1" was not one client), and it duplicates what the
// panel says as soon as it is opened. The label is the whole trigger now.
//
// This is asserted on the three places the badge lived — the markup, the module
// that wrote it, and the stylesheet that dressed it — because removing it from
// only one of them leaves either a stray "0" or a rule no element wears.
func TestClientsTriggerHasNoCountBadge(t *testing.T) {
	index := readAssetLF(t, "index.html")
	clients := readAssetLF(t, "static/js/clients.js")
	css := readAssetLF(t, "static/css/base.css")

	if strings.Contains(index, "clients-count") {
		t.Error("index.html still carries the count badge element")
	}
	if strings.Contains(clients, "clients-count") {
		t.Error("clients.js still looks the count badge up and writes to it")
	}
	if strings.Contains(css, ".clients-count") {
		t.Error("base.css still dresses .clients-count, which no element wears")
	}

	// The trigger still identifies itself: the dot is the icon and the label is
	// what a narrow header hides. Losing both would leave an unlabelled square.
	if !strings.Contains(index, `class="clients-label"`) {
		t.Error("the trigger needs its own label span for the narrow-screen rule to hide")
	}
	if !strings.Contains(index, `id="clients-menu"`) {
		t.Error("clients.js still opens the panel by id; the markup must keep it")
	}
}

// On a narrow header the clients trigger keeps its glyph and drops its word,
// exactly as the docs link and the language/theme keys beside it do.
//
// The label is display:none rather than removed: the summary keeps its title and
// the span still exists for the i18n engine, so the accessible name and the
// tooltip survive at every width.
func TestClientsTriggerCollapsesToItsGlyphOnNarrowScreens(t *testing.T) {
	// Both copies: the shared sheet and the cyberpunk theme's own copy of it. A
	// theme that keeps the label would overflow the header on a phone, and the
	// theme's copy is the one that renders there.
	for _, path := range []string{
		"static/css/workspace.css",
		"themes/cyberpunk/assets/static/css/workspace.css",
	} {
		css := readAssetLF(t, path)
		narrow := between(t, css, "@media (max-width: 768px) {", "\n}\n")
		if !strings.Contains(narrow, ".clients-label { display: none; }") {
			t.Errorf("%s: the narrow-header block must hide the clients label", path)
		}
		if !strings.Contains(narrow, ".clients-btn { width: 32px; height: 32px; padding: 0; justify-content: center; }") {
			t.Errorf("%s: the clients trigger must become a 32px square beside the other keys", path)
		}
	}

	// And the wide header still shows the word: the rule belongs to the media
	// query, not to the base sheet.
	base := readAssetLF(t, "static/css/base.css")
	if !strings.Contains(base, ".clients-label { white-space: nowrap; }") {
		t.Error("base.css should keep the label on the wide header")
	}
	if strings.Contains(base, ".clients-label { display: none; }") {
		t.Error("base.css must not hide the label unconditionally")
	}
}

// Opening the panel must draw it, not wait for the next frame.
//
// Frames are change-driven — the server broadcasts when a session, a client or a
// tab changes — so on a quiet page the next one can be minutes away. The panel
// used to paint only from a frame that arrived WHILE it was open, which meant a
// freshly opened panel showed an empty box with the "nothing connected yet" hint
// under it. That reads as "nobody is attached" even though the reader's own tab
// is always one of the rows.
//
// The harness drives the real module: the lists are delivered once while the
// panel is closed, and then the open event fires with no new frame. What the
// assertion pins is that the second step draws from the lists already in hand.
func TestClientsPanelPaintsWhenItIsOpened(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the clients panel")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "clients.js")
	if err := os.WriteFile(script, []byte(clientsPanelHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(dir, "clients-module.js")
	if err := os.WriteFile(module, []byte(readAssetLF(t, "static/js/clients.js")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, module).CombinedOutput()
	if err != nil {
		t.Fatalf("the clients panel check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CLIENTS PANEL OK") {
		t.Fatalf("the panel harness did not report success:\n%s", out)
	}
}

// clientsPanelHarness drives clients.js against a small DOM stub.
const clientsPanelHarness = `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

/* --- DOM stub ----------------------------------------------------------- */
function el(id) {
  const node = {
    id: id || '', open: false, hidden: true, textContent: '', className: '',
    children: [], _listeners: {},
    appendChild(c) { node.children.push(c); return c; },
    setAttribute() {}, getAttribute() { return null; },
    addEventListener(name, fn) { (node._listeners[name] = node._listeners[name] || []).push(fn); },
    fire(name) { (node._listeners[name] || []).forEach(fn => fn()); }
  };
  return node;
}
const els = {
  'clients-menu': el('clients-menu'),
  'clients-body': el('clients-body'),
  'clients-empty': el('clients-empty')
};
const document = {
  readyState: 'complete',
  getElementById: id => els[id] || null,
  addEventListener() {},
  createElement: () => el('row')
};
const context = {
  console, JSON, String, Number, Array, Object, Date, isNaN, isFinite, parseInt,
  document,
  t: (k, opts) => k + (opts ? JSON.stringify(opts) : ''),
  shortClock: () => '12:00',
  _termcpLang: 'en'
};
vm.createContext(context);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), context);

const menu = els['clients-menu'];
assert.ok(menu._termcpClientsWired, 'the open event must be wired at load');

/* A frame arrives while the panel is closed. Nothing is drawn — the frame path
   stays cheap — but the lists are kept. */
context.renderClients(
  [{ name: 'agent-a', version: '1.0', connected_at: '2026-10-17T10:00:00Z' }],
  [{ addr: '127.0.0.1', user_agent: 'Chrome/120', self: true, watching: 2 }]
);
assert.strictEqual(els['clients-body'].children.length, 0,
  'a closed panel must not rebuild its list on every frame');

/* The reader opens it. No new frame is coming. */
menu.open = true;
menu.fire('toggle');
assert.strictEqual(els['clients-body'].children.length, 2,
  'opening the panel must draw the lists already in hand, or it reads as "nothing connected"');
assert.strictEqual(els['clients-empty'].hidden, true,
  'with a client and this tab attached, the empty hint must be hidden');

/* Reopening must not duplicate rows: the signature guard is what makes the
   paint idempotent across toggle and frame. */
menu.open = false; menu.fire('toggle');
menu.open = true; menu.fire('toggle');
assert.strictEqual(els['clients-body'].children.length, 2,
  'a repaint must replace the list, not append to it');

/* And a lone tab is still "nothing connected yet": the reader's own row is not
   another client. */
context.renderClients([], [{ addr: '127.0.0.1', self: true }]);
menu.open = false; menu.fire('toggle');
menu.open = true; menu.fire('toggle');
assert.strictEqual(els['clients-empty'].hidden, false,
  'a page with only itself open has no other clients');

console.log('CLIENTS PANEL OK');
`

// TestTheViewerSocketProvesItsPeerIsAlive pins the liveness contract.
//
// The registry removes a tab when its socket's read loop returns, so a read loop
// that never returns is a row that never leaves. That is exactly what happened:
// writePump pinged every 45 seconds, but readPump had no pong handler and no read
// deadline, so a browser that was closed, crashed, slept or changed networks left
// a socket the kernel still believed in. ReadMessage blocked forever, the deferred
// remove never ran, and the registry kept a row for a page that no longer existed.
// The symptom is the one reported: "why does it say two web clients when I have one
// page open?" — the ghost is one crashed tab, and the count climbs with every crash.
//
// All three parts are needed and each is checked, because any one of them alone
// looks like a fix and is not:
//   - a deadline, or nothing ever times out;
//   - a PongHandler refreshing it, or a quiet-but-healthy tab is killed instead;
//   - pings actually being sent, or no pong ever arrives to refresh anything.
func TestTheViewerSocketProvesItsPeerIsAlive(t *testing.T) {
	raw, err := os.ReadFile("ws.go")
	if err != nil {
		t.Fatalf("ws.go: %v", err)
	}
	ws := strings.ReplaceAll(string(raw), "\r\n", "\n")

	if !strings.Contains(ws, "SetReadDeadline(") {
		t.Fatal("ws.go sets no read deadline: a peer that vanishes without a FIN is never noticed, and its viewer row leaks")
	}
	if !strings.Contains(ws, "SetPongHandler(") {
		t.Error("a read deadline with no pong handler kills every quiet tab: the deadline is only ever refreshed by inbound frames")
	}
	if !strings.Contains(ws, "SetPingHandler(") {
		t.Error("the server must also answer a client's ping, or half the keep-alive is missing")
	}
	if !strings.Contains(ws, "websocket.PingMessage") {
		t.Error("nothing pings: without a ping there is no pong, so the deadline would fire on a healthy connection")
	}
	// The deadline has to be refreshed on every reader of the socket: the pong, an
	// inbound client frame, and the ping reply. Missing the last two would leave a
	// tab that is actively typing one deadline behind.
	body := between(t, ws, "func (c *uiWS) readPump() {", "\n}\n")
	if body == "" {
		t.Fatal("readPump is gone")
	}
	if n := strings.Count(body, "SetReadDeadline(time.Now().Add(wsReadIdle))"); n < 3 {
		t.Errorf("the read deadline is refreshed in %d places, want 3 (setup, pong, inbound frame)", n)
	}
	// And the removal must still be deferred in the handler that adds it, so the row
	// goes away however the loop ends.
	handler := between(t, ws, "viewerID := c.h.viewerHub().add(", "c.readPump()")
	if !strings.Contains(handler, "defer c.h.viewerHub().remove(viewerID)") {
		t.Error("the viewer must be removed by a defer, or an abnormal loop exit leaks the row")
	}
}
