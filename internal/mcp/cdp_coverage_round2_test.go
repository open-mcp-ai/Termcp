package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestCdpSendInjectsSessionIdIntoPageFrame covers cdpSend when a target is named:
// the sessionId must be injected even though the call is fire-and-forget.
func TestCdpSendInjectsSessionIdIntoPageFrame(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	fake, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Attach T1 first so the target is known.
	if got := termText(t, s, map[string]any{"cmd": "open " + h + " --target T1"}); !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("attach failed: %s", got)
	}

	// Send to the attached page with --send.
	pageHandle := h + "@T1"
	sent := termText(t, s, map[string]any{"cmd": `call ` + pageHandle + ` --send {"method":"Page.navigate","params":{"url":"about:blank"}}`})
	if !strings.HasPrefix(sent, "TERM/1 202") {
		t.Fatalf("expected 202 from --send, got:\n%s", sent)
	}
	waitFor(t, "frame to reach the fake browser", func() bool {
		_, ok := fake.saw("Page.navigate")
		return ok
	})
	record, _ := fake.saw("Page.navigate")
	if !strings.Contains(record, "SID-T1") {
		t.Errorf("cdpSend did not inject sessionId: %q", record)
	}
}

// TestCdpOneShotReusesExistingSession pins that cdp@<target> reuses an already-attached
// session on that target rather than attaching a second time and tearing it down.
func TestCdpOneShotReusesExistingSession(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	fake, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Attach T1 normally.
	termText(t, s, map[string]any{"cmd": "open " + h + " --target T1"})

	// Count attaches so far.
	countAttaches := func() int {
		c := 0
		for _, s := range fake.sawAll() {
			if strings.HasPrefix(s, "Target.attachToTarget|") {
				c++
			}
		}
		return c
	}
	before := countAttaches()

	// Transient call on the SAME target: must reuse the session, so no second attach.
	termText(t, s, map[string]any{"cmd": `call internal cdp@T1 {"method":"Page.reload"}`})

	after := countAttaches()
	if after != before {
		t.Errorf("one-shot attached again (%d -> %d) instead of reusing the session", before, after)
	}

	// And it must NOT detach the reused session.
	for _, s := range fake.sawAll() {
		if strings.HasPrefix(s, "Target.detachFromTarget|") {
			t.Errorf("a reused session was detached: %v", fake.sawAll())
			break
		}
	}
}

// TestCdpTransientAttachErrorReportsProblem pins what a caller sees when the transient
// target does not exist.
func TestCdpTransientAttachErrorReportsProblem(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://` + r.Host + `/devtools/browser/x"}`))
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var frame map[string]any
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			// Every attach fails with a protocol-level error.
			_ = conn.WriteJSON(map[string]any{
				"id":    frame["id"],
				"error": map[string]any{"message": "No target with given id found"},
			})
		}
	}))
	t.Cleanup(srv.Close)

	s := newTestServer(t)
	internalSession(t, s)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": `call internal cdp@STALE {"method":"Page.reload"}`})
	if !strings.HasPrefix(got, "TERM/1 404") {
		t.Fatalf("expected 404 for stale target, got:\n%s", got)
	}
	if !strings.Contains(got, "cannot attach STALE") {
		t.Errorf("refusal did not name the target: %s", got)
	}
}

// TestCdpDomainEnableIsRecordedAndReadBack pins that .enable calls are recorded so
// `get` can answer what is enabled on that page.
func TestCdpDomainEnableIsRecordedAndReadBack(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "open " + h + " --target T1"})
	pageHandle := h + "@T1"

	// Enable Page domain.
	res := termText(t, s, map[string]any{"cmd": `call ` + pageHandle + ` {"method":"Page.enable"}`})
	if !strings.HasPrefix(res, "TERM/1 200") {
		t.Fatalf("Page.enable failed:\n%s", res)
	}

	// Describe the page — enabled list must include "Page".
	desc := termText(t, s, map[string]any{"cmd": "get " + pageHandle})
	if !strings.Contains(desc, "Page") {
		t.Errorf("Page domain not reported as enabled in get output:\n%s", desc)
	}
}

// TestCdpListShowsLiveChildren exercises cdpState.live() through ls <peer>.
func TestCdpListShowsLiveChildren(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://` + r.Host + `/devtools/browser/x"}`))
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var frame map[string]any
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			m, _ := frame["method"].(string)
			switch m {
			case "Target.getTargets":
				_ = conn.WriteJSON(map[string]any{
					"id": frame["id"],
					"result": map[string]any{
						"targetInfos": []any{
							map[string]any{"targetId": "T1", "type": "page", "title": "A", "url": "about:blank"},
						},
					},
				})
			case "Target.attachToTarget":
				_ = conn.WriteJSON(map[string]any{
					"id":     frame["id"],
					"result": map[string]any{"sessionId": "SID-T1"},
				})
			default:
				_ = conn.WriteJSON(map[string]any{"id": frame["id"], "result": map[string]any{}})
			}
		}
	}))
	t.Cleanup(srv.Close)

	s := newTestServer(t)
	internalSession(t, s)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Attach T1 so cdpState.live() has an entry.
	termText(t, s, map[string]any{"cmd": "open " + h + " --target T1"})

	// `ls <peer>` calls cdpList which formats `h.cdp.live()`.
	listing := termText(t, s, map[string]any{"cmd": "ls " + h})
	if !strings.Contains(listing, "Sessions live under this peer: T1") {
		t.Errorf("listing did not show live child T1:\n%s", listing)
	}
}

// TestHttpBaseOfCoversEveryScheme pins the URL derivation from socket to http.
func TestHttpBaseOfCoversEveryScheme(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ws://127.0.0.1:9222/devtools/browser/x", "http://127.0.0.1:9222"},
		{"wss://remote:9222/devtools/browser/x", "https://remote:9222"},
		{"http://127.0.0.1:9222", "http://127.0.0.1:9222"},
		{"https://127.0.0.1:9222", "https://127.0.0.1:9222"},
		{"invalid url", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := httpBaseOf(tc.in); got != tc.want {
			t.Errorf("httpBaseOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHumanBytesCoversEveryScale covers bytes/KB/MB branches.
func TestHumanBytesCoversEveryScale(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{500, "500 bytes"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestATargetListKeepsTheURLFromEatingTheTable pins issue #249.3.
//
// A devtools url carries the page's whole query string, so one tracking-heavy page can
// be a kilobyte on its own: every row below it stops lining up, and a caller reading with
// a small `bytes_limit` gets the LIST cut rather than the url that caused it. The fix is
// at the shared renderer, so `ls <peer>` and the attach receipt get the same bound.
//
// The assertion is about the ROW, not the whole reply: a long url must be shortened with
// a visible ellipsis, and a short one must be untouched — the second half is what keeps
// this from being satisfied by truncating everything.
func TestATargetListKeepsTheURLFromEatingTheTable(t *testing.T) {
	long := "https://example.test/dashboard?utm_source=" + strings.Repeat("x", 400)

	// fakeCdpWithURL answers each target with the url the caller names.
	fakeCdpWithURL := func(t *testing.T, url string) string {
		t.Helper()
		up := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/json/version" {
				_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://` + r.Host + `/devtools/browser/x"}`))
				return
			}
			conn, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				var frame map[string]any
				if err := conn.ReadJSON(&frame); err != nil {
					return
				}
				if m, _ := frame["method"].(string); m == "Target.getTargets" {
					_ = conn.WriteJSON(map[string]any{
						"id": frame["id"],
						"result": map[string]any{"targetInfos": []any{
							map[string]any{"targetId": "T1", "type": "page", "title": "A", "url": url},
						}},
					})
					continue
				}
				_ = conn.WriteJSON(map[string]any{"id": frame["id"], "result": map[string]any{}})
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	layout := func(t *testing.T, url string) string {
		t.Helper()
		s := newTestServer(t)
		internalSession(t, s)
		h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + fakeCdpWithURL(t, url)}))
		defer termText(t, s, map[string]any{"cmd": "close " + h})
		return termText(t, s, map[string]any{"cmd": "ls " + h})
	}

	out := layout(t, long)
	if strings.Contains(out, long) {
		t.Errorf("a %d-character url was rendered whole, so the list cannot line up:\n%s", len(long), out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("the url was shortened without saying so; a caller cannot tell it from the real one:\n%s", out)
	}

	short := "https://example.test/a"
	if got := layout(t, short); !strings.Contains(got, short) {
		t.Errorf("a %d-character url was shortened; the bound must not touch short ones:\n%s", len(short), got)
	}
}
