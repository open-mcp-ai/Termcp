package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// fakeBrowser is a DevTools-shaped peer that also remembers what it was asked.
//
// The real browser test proves the door speaks Chrome; this one proves WHAT THE DOOR
// SENT — which method, with which sessionId, attached or detached — because those are
// the things a browser cannot report about itself. It answers every frame by method
// so the attach/one-shot paths have a sessionId to work with.
type fakeBrowser struct {
	mu   sync.Mutex
	sent []string // "<method> <sessionId>"
}

func (b *fakeBrowser) record(method, sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, method+"|"+sessionID)
}

func (b *fakeBrowser) saw(method string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sent {
		if strings.HasPrefix(s, method+"|") {
			return s, true
		}
	}
	return "", false
}

// waitFor polls, because a send returns before the peer has necessarily read the
// frame: asserting on what the peer saw is inherently a race with the wire.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (b *fakeBrowser) sawAll() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

// fakeDevtools serves /json/version and a socket that answers by method.
func fakeDevtools(t *testing.T) (*fakeBrowser, *httptest.Server) {
	t.Helper()
	b := &fakeBrowser{}
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
			method, _ := frame["method"].(string)
			sessionID, _ := frame["sessionId"].(string)
			b.record(method, sessionID)
			result := map[string]any{"ok": true}
			switch method {
			case "Target.attachToTarget":
				targetID, _ := frame["params"].(map[string]any)["targetId"].(string)
				result = map[string]any{"sessionId": "SID-" + targetID}
			case "Target.getTargetInfo":
				targetID, _ := frame["params"].(map[string]any)["targetId"].(string)
				result = map[string]any{"targetInfo": map[string]any{
					"targetId": targetID, "type": "page", "title": "Fake", "url": "about:blank",
				}}
			case "Runtime.evaluate":
				// 36 KB of multibyte text: bigger than one reply, and cut in the middle
				// of a character often enough that a naive slice would corrupt it.
				result = map[string]any{"result": map[string]any{"value": strings.Repeat("日本語", 4000)}}
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []any{}}
			}
			_ = conn.WriteJSON(map[string]any{"id": frame["id"], "result": result})
			// An event with the page's sessionId: what a page-scoped read must keep,
			// and what a filtered read of a different page must drop.
			_ = conn.WriteJSON(map[string]any{
				"sessionId": "SID-T1", "method": "Page.loadEventFired",
				"params": map[string]any{"timestamp": 1},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return b, srv
}

// TestCdpOneShotAttachesAndDetaches is the `cdp@<target>` form end to end: one call,
// no attachment left behind, and the sessionId injected into the frame.
//
// It exists because the shape is easy to get wrong in a way that still "works": a
// door that attaches and never detaches leaks a CDP session per command, and one
// that detaches a session it did not create tears down somebody else's work.
func TestCdpOneShotAttachesAndDetaches(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	fake, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// NO INDEX: the target alone is the one-shot form. With an index the target is
	// expected to be attached already, which is a different command.
	out := termText(t, s, map[string]any{"cmd": `call internal cdp@T1 {"method":"Page.reload"}`})
	if !statusLineIs(out, "200") {
		t.Fatalf("the one-shot form failed: %s", out)
	}
	waitFor(t, "the frame to reach the browser", func() bool {
		_, ok := fake.saw("Page.reload")
		return ok
	})
	if reload, _ := fake.saw("Page.reload"); !strings.Contains(reload, "SID-T1") {
		t.Errorf("the one-shot frame carried no sessionId: %q", reload)
	}
	if _, ok := fake.saw("Target.attachToTarget"); !ok {
		t.Errorf("no attach happened: %v", fake.sawAll())
	}
	waitFor(t, "the one-shot session to be detached", func() bool {
		_, ok := fake.saw("Target.detachFromTarget")
		return ok
	})
	_ = h
}

// A SEND DOES NOT WAIT. It answers 202 and the frame is on the wire — the caller
// reads the answer later out of the frame log, which is the only way a notification
// (which has no answer at all) can be sent.
func TestCdpSendReturnsBeforeTheAnswer(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	fake, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{"cmd": `call ` + h + ` --send {"method":"Browser.close"}`})
	if !statusLineIs(out, "202") {
		t.Fatalf("--send did not answer 202:\n%s", out)
	}
	waitFor(t, "the sent frame to arrive", func() bool {
		_, ok := fake.saw("Browser.close")
		return ok
	})
}

// `get <conn>@<target>` describes ONE page. Without the target it describes the
// connection, and the two must not answer the same thing.
func TestGetOnePageNamesTheTarget(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	page := handleFrom(t, termText(t, s, map[string]any{"cmd": "open " + h + " --target T1"}))

	conn := termText(t, s, map[string]any{"cmd": "get " + h})
	if !strings.Contains(conn, "attached targets") || !strings.Contains(conn, page) {
		t.Fatalf("the connection view did not list the page %q:\n%s", page, conn)
	}
	one := termText(t, s, map[string]any{"cmd": "get " + page})
	if !strings.Contains(one, "targetId:  T1") || !strings.Contains(one, "SID-T1") {
		t.Fatalf("the page view did not describe the page:\n%s", one)
	}
	if strings.Contains(one, "frames received") {
		t.Errorf("the page view answered like the connection view:\n%s", one)
	}
}

// --grep filters the window BEFORE the page cap, so a noisy domain cannot push the
// frames the caller asked for out of the reply.
func TestReadGrepFiltersTheWindow(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	termText(t, s, map[string]any{"cmd": `call ` + h + ` {"method":"Target.getTargets"}`})

	hit := termText(t, s, map[string]any{"cmd": `read ` + h + ` --since 0 --grep Page.loadEventFired`})
	if !strings.Contains(hit, "Page.loadEventFired") {
		t.Fatalf("--grep dropped the frames it should keep:\n%s", hit)
	}
	if strings.Contains(hit, "Target.getTargets") {
		t.Errorf("--grep kept a frame it should drop:\n%s", hit)
	}
}

// THE ADDRESS RULES, as answers rather than as documentation.
func TestPeerAddressRefusals(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)

	first := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + first})
	// The mcp peer has to BE an mcp peer: opening one against the DevTools endpoint is
	// exactly the misconfiguration `open` now refuses (502, nothing handed back), so
	// reusing that URL here would make this test depend on the old behaviour.
	mcpSrv := toolListPeer(t)
	mcpPeer := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal mcp ws" + strings.TrimPrefix(mcpSrv.URL, "http")}))
	defer termText(t, s, map[string]any{"cmd": "close " + mcpPeer})

	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"unknown connection", `call internal cdp:999 {"method":"Page.enable"}`, "no cdp:999"},
		{"page syntax on a kind without children", `call ` + mcpPeer + `@T1 {"method":"x"}`, "only a cdp peer has"},
		{"unknown target", `call ` + first + `@NOSUCH {"method":"x"}`, "NOSUCH names nothing"},
	}
	for _, tc := range cases {
		out := termText(t, s, map[string]any{"cmd": tc.cmd})
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: the reply does not say %q:\n%s", tc.name, tc.want, out)
		}
	}
	if res := termCall(t, s, map[string]any{"cmd": "close " + mcpPeer}); res.IsError {
		t.Fatalf("closing the mcp peer failed: %s", toolResultText(res))
	}
}

// TWO CONNECTIONS ON ONE SESSION MAKE AN UNQUALIFIED ADDRESS AMBIGUOUS. Guessing one
// would drive the wrong browser while the reply said it worked, so the answer names
// both instead.
func TestBareKindIsRefusedWhenTwoConnectionsExist(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)

	a := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	b := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + a})
	defer termText(t, s, map[string]any{"cmd": "close " + b})

	out := termText(t, s, map[string]any{"cmd": `call internal cdp {"method":"Target.getTargets"}`})
	if statusCode(t, out) != 400 || !strings.Contains(out, "ambiguous") {
		t.Fatalf("an unqualified address was not refused:\n%s", out)
	}
	if !strings.Contains(out, a) || !strings.Contains(out, b) {
		t.Errorf("the refusal does not name both connections (%q, %q):\n%s", a, b, out)
	}
}

// TestBigAnswerIsAReceiptAndPagesBack is the reason a reply is not just a truncated
// wall of text: an oversized frame is named, previewed, and pageable. Without this,
// a 36 KB DOM dump arrives as broken JSON — too big to have been worth the tokens and
// too cut to use.
func TestBigAnswerIsAReceiptAndPagesBack(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{"cmd": `call ` + h + ` {"method":"Runtime.evaluate","params":{"expression":"x"}}`})
	// The receipt now names the WINDOW that stopped it rather than saying "too big"
	// about a constant (issue #204). What this test is about is unchanged: the frame
	// is not inlined, it is named and pageable.
	if !strings.Contains(out, "so it is a receipt instead") {
		t.Fatalf("an oversized answer was inlined:\n%s", out)
	}
	frame, ok := headerValue(t, out, "Frame")
	if !ok {
		t.Fatalf("the receipt does not name the frame:\n%s", out)
	}
	bytesHeader, ok := headerValue(t, out, "Bytes")
	if !ok || bytesHeader == "0" {
		t.Fatalf("the receipt does not report the size:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("日本語", 200)) {
		t.Errorf("the receipt carried the payload anyway:\n%s", out)
	}

	// --at pages it out. The first page must be valid text, and continuing from
	// Next-Offset must reach the end without duplicating or dropping a byte.
	first := termText(t, s, map[string]any{"cmd": "read " + h + " --at " + frame + " --offset 0 --bytes 1000"})
	body, ok := termBody(first)
	if !ok {
		t.Fatalf("no body in the first page:\n%s", first)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("the first page is not valid UTF-8:\n%q", body)
	}
	next, ok := headerValue(t, first, "Next-Offset")
	if !ok {
		t.Fatalf("the first page does not say where to continue:\n%s", first)
	}
	second := termText(t, s, map[string]any{"cmd": "read " + h + " --at " + frame + " --offset " + next + " --bytes 1000"})
	body2, ok := termBody(second)
	if !ok || body2 == "" {
		t.Fatalf("the second page is empty:\n%s", second)
	}
	if !utf8.ValidString(body2) {
		t.Fatalf("the second page is not valid UTF-8:\n%q", body2)
	}

	// A page past the end is a refusal, not an empty success: "you are done" and "you
	// asked for something that is not there" are different answers.
	past := termText(t, s, map[string]any{"cmd": "read " + h + " --at " + frame + " --offset 99999999"})
	if code := statusCode(t, past); code != 400 {
		t.Fatalf("an offset past the end was accepted:\n%s", past)
	}
	// And a frame number that is not in the log is a 404 naming the latest.
	missing := termText(t, s, map[string]any{"cmd": "read " + h + " --at 999999"})
	if code := statusCode(t, missing); code != 404 {
		t.Fatalf("a missing frame was not a 404:\n%s", missing)
	}
}

// termBody returns the body of a rendered slot: everything after the blank line that
// ends the headers.
func termBody(text string) (string, bool) {
	if _, rest, ok := strings.Cut(text, "\n\n"); ok {
		return rest, true
	}
	return "", false
}
