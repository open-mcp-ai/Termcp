package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// marksBody is the endpoint's response as a client sees it.
type marksBody struct {
	Marks      []markSpan `json:"marks"`
	TotalBytes int64      `json:"total_bytes"`
	SessionID  string     `json:"session_id"`
}

func getMarks(t *testing.T, mux *http.ServeMux, id string) (*httptest.ResponseRecorder, marksBody) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/shells/"+id+"/marks", nil))
	var body marksBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil && rr.Code == http.StatusOK {
		t.Fatalf("decode marks body: %v (%s)", err, rr.Body.String())
	}
	return rr, body
}

// assertSpanChain pins the derived-end invariant: each span ends where the next
// begins and the last ends at total_bytes, which is the byte log's size.
func assertSpanChain(t *testing.T, body marksBody) {
	t.Helper()
	for i, m := range body.Marks {
		want := body.TotalBytes
		if i+1 < len(body.Marks) {
			want = body.Marks[i+1].Start
		}
		if m.End != want {
			t.Errorf("marks[%d].end = %d, want %d (next start / total_bytes)", i, m.End, want)
		}
		if i > 0 && m.Start < body.Marks[i-1].Start {
			t.Errorf("marks[%d].start = %d, before marks[%d].start = %d; marks are in offset order", i, m.Start, i-1, body.Marks[i-1].Start)
		}
	}
}

// TestShellMarksOnRestoredSession covers the read path a live-only lookup would
// miss: a session loaded from disk (RestoreDead) has no in-memory shell, so the
// endpoint must resolve the shell through the retained snapshot and read the
// persisted index. Times and offsets are asserted literally, which is also what
// pins the unit: clock.Now() and log.jsonl store Unix milliseconds, and this
// response must pass them through rather than convert.
func TestShellMarksOnRestoredSession(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	const sessID, shellID = "s-marks", "sh-marks"

	// Two spans of bytes with a mark at each transition, and a trailing span
	// whose mark was never appended: end is derived from the file size, so the
	// last mark still ends at the true end of the log.
	if _, err := store.AppendLog(sessID, shellID, []byte("OUT1")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMark(sessID, shellID, api.LogMark{Status: api.LogOutput, Time: 1758499205123, Offset: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendLog(sessID, shellID, []byte("IN")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMark(sessID, shellID, api.LogMark{Status: api.LogAPIInput, Time: 1758499206000, Offset: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendLog(sessID, shellID, []byte("OUT2")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(api.Session{ID: sessID, Name: "marks", Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell(sessID, api.Session{ID: shellID, Name: "shell-1", Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := storage.New(dir)
	msgMgr := message.NewManager(store2)
	sessMgr := session.NewManager(msgMgr, store2, nil)
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	rr, body := getMarks(t, mux, shellID)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET marks on restored session = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if body.SessionID != sessID {
		t.Errorf("session_id = %q, want %q", body.SessionID, sessID)
	}
	if body.TotalBytes != int64(len("OUT1INOUT2")) {
		t.Errorf("total_bytes = %d, want %d", body.TotalBytes, len("OUT1INOUT2"))
	}
	want := []markSpan{
		{Status: string(api.LogOutput), Time: 1758499205123, Start: 0, End: 4},
		{Status: string(api.LogAPIInput), Time: 1758499206000, Start: 4, End: int64(len("OUT1INOUT2"))},
	}
	if len(body.Marks) != len(want) {
		t.Fatalf("got %d marks, want %d: %+v", len(body.Marks), len(want), body.Marks)
	}
	for i := range want {
		if body.Marks[i] != want[i] {
			t.Errorf("marks[%d] = %+v, want %+v", i, body.Marks[i], want[i])
		}
	}
	assertSpanChain(t, body)

	// A session_id path value falls back to the session's primary shell, the same
	// compatibility output-range offers.
	rr2, body2 := getMarks(t, mux, sessID)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET marks by session_id = %d (%s), want 200", rr2.Code, rr2.Body.String())
	}
	if len(body2.Marks) != len(want) || body2.TotalBytes != body.TotalBytes {
		t.Errorf("session_id lookup = %+v, want the same spans as the shell_id lookup %+v", body2, body)
	}
}

// TestShellMarksLiveAndDeadSession pins that the same endpoint serves a running
// session and then the same session after it goes DEAD: the mark index is on
// disk either way, so closing a session must not take its history away.
func TestShellMarksLiveAndDeadSession(t *testing.T) {
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

	sess, err := sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	rr, body := getMarks(t, mux, sess.PrimaryShellID())
	if rr.Code != http.StatusOK {
		t.Fatalf("GET marks on live session = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if body.SessionID != sess.ID {
		t.Errorf("session_id = %q, want %q", body.SessionID, sess.ID)
	}
	if body.Marks == nil {
		t.Error("marks must be an empty array, never null: a client indexes it without a guard")
	}
	assertSpanChain(t, body)

	// DEAD in place: the registry entry stays and its persisted history must
	// remain readable (the live-only lookup would 404 here).
	sess.Terminate(true, 0)
	rr, body = getMarks(t, mux, sess.PrimaryShellID())
	if rr.Code != http.StatusOK {
		t.Fatalf("GET marks on DEAD session = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	assertSpanChain(t, body)
}

// TestShellMarksBodyShape pins the response as a client reads it, not as Go
// decodes it: the key names are the contract (a renamed field still unmarshals
// into a struct if the tag follows it), and an empty index must be [] rather than
// null, or a client that maps over it renders nothing instead of nothing.
func TestShellMarksBodyShape(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	const sessID, shellID = "s-shape", "sh-shape"
	if _, err := store.AppendLog(sessID, shellID, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMark(sessID, shellID, api.LogMark{Status: api.LogOutput, Time: 1758499205123, Offset: 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(api.Session{ID: sessID, Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	// A second shell with bytes but no marks yet: its response must still carry an
	// array for "marks", never null.
	const bareShellID = "sh-bare"
	if _, err := store.AppendLog(sessID, bareShellID, []byte("more")); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []string{shellID, bareShellID} {
		if err := store.SaveShell(sessID, api.Session{ID: sh, Status: api.SessionExited}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := storage.New(dir)
	t.Cleanup(func() { _ = store2.Close() })
	sessMgr := session.NewManager(message.NewManager(store2), store2, nil)
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Handler{Sessions: sessMgr}).Register(mux)

	get := func(shellID string) string {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/shells/"+shellID+"/marks", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET marks for %s = %d (%s), want 200", shellID, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	raw := get(shellID)
	for _, want := range []string{`"marks":[`, `"status":"o"`, `"time":1758499205123`, `"start":0`, `"end":2`, `"total_bytes":2`, `"session_id":"` + sessID + `"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("body misses %s: %s", want, raw)
		}
	}
	if bare := get(bareShellID); !strings.Contains(bare, `"marks":[`) {
		t.Errorf("marks must serialize as an array, not null: %s", bare)
	}
}

// TestShellTimelineRailIsWired guards the frontend half of the feature, which no
// Go build can see: the module is loaded before the code that calls it, the call
// site exists, the strip draws one cell per terminal row, and the stylesheet
// defines the strip. Each is a one-line deletion away from a silently absent rail.
func TestShellTimelineRailIsWired(t *testing.T) {
	index := readAssetLF(t, "index.html")
	jsAt := strings.Index(index, "static/js/timeline.js")
	callerAt := strings.Index(index, "static/js/terminal-window.js")
	if jsAt == -1 || callerAt == -1 || jsAt > callerAt {
		t.Error("index.html must load timeline.js before terminal-window.js, which calls into it")
	}
	tv := readAssetLF(t, "static/js/terminal-panels.js")
	if !strings.Contains(tv, "initShellTimeline(win);") {
		t.Error("terminal-panels.js no longer mounts the timeline rail")
	}
	tl, err := readAsset("static/js/timeline.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"function initShellTimeline(",
		"function renderShellTimeline(",
		"function shellRailSchedule(",
		"/marks",                     // the endpoint this rail is for
		"MutationObserver",           // refresh triggers on new output, not per chunk
		"function railVisibleRange(", // the rail draws the visible slice, not the whole log
		// The row layout comes from the server, which is the side that has the log:
		// the browser cannot replay a megabyte of scrollback per repaint, and a
		// mapping recorded as output streamed in is stale the moment the window is
		// resized or the channel is reloaded.
		"function railSpansFor(", // a row's bytes, not a byte placed into a row
		"/rail?cols=",            // laid out at the terminal's own width
		"function railEnsureWindow(",
		"st.lastBody", // the response in hand, reused by a scroll repaint
	} {
		if !strings.Contains(tl, want) {
			t.Errorf("timeline.js misses %q", want)
		}
	}
	// A windowed response is only good for the channel and the bytes it was asked
	// about: one landing after the user switched tabs must not paint the new channel
	// with the old one's marks, and one overtaken by a newer request must not
	// overwrite it with a window the viewport has already left.
	for _, want := range []string{"fetchSeq", "_activeChannelSid"} {
		if !strings.Contains(tl, want) {
			t.Errorf("timeline.js no longer guards an in-flight marks response (%q)", want)
		}
	}
	// A cell is drawn per visible row and the row's status comes from the marks on
	// it, so the old per-byte plotting must not come back: a rail laid out by byte
	// share contradicts the text beside it, because a row is not a byte range.
	if strings.Contains(tl, "railRowAtByte") || strings.Contains(tl, "layoutRailPoints") {
		t.Error("the rail places marks by byte offset again; a cell belongs to a terminal row")
	}
	// The rail is a map of the rows on screen, not a control: scrollLines
	// (imperative scrolling) would move the viewport under a cursor aimed at a
	// cell, so the module must not reach for one.
	if strings.Contains(tl, "scrollLines") {
		t.Error("the rail must not scroll the terminal; it follows it")
	}
	if !strings.Contains(tl, "buffer.active") {
		t.Error("timeline.js no longer reads the xterm buffer; the viewport window cannot be computed without it")
	}
	css := readAppCSS(t)
	for _, want := range []string{".term-rail {", ".term-rail-cell {", ".term-rail-pop {", ".term-rail-output", ".term-rail-input", ".term-rail-agent"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css misses %q", want)
		}
	}
}

// TestShellMarksUnknownShell pins the 404 for an id this instance never knew.
func TestShellMarksUnknownShell(t *testing.T) {
	sessMgr := session.NewManager(nil, nil, nil)
	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	rr, _ := getMarks(t, mux, "nope")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET marks on unknown shell = %d (%s), want 404", rr.Code, rr.Body.String())
	}
}

// marksFixture writes a shell whose marks sit at 0, 4, 8 and 12 over 16 bytes,
// so a window can be asked for between them.
func marksFixture(t *testing.T) (*http.ServeMux, string, string) {
	t.Helper()
	dir := t.TempDir()
	store := storage.New(dir)
	const sessID, shellID = "s-window", "sh-window"
	statuses := []api.LogStatus{api.LogOutput, api.LogAPIInput, api.LogOutput, api.LogAIInput}
	for i, at := range []int64{0, 4, 8, 12} {
		if _, err := store.AppendLog(sessID, shellID, []byte("abcd")); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMark(sessID, shellID, api.LogMark{
			Status: statuses[i], Time: 1758499205000 + at, Offset: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveSession(api.Session{ID: sessID, Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell(sessID, api.Session{ID: shellID, Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := storage.New(dir)
	t.Cleanup(func() { _ = store2.Close() })
	sessMgr := session.NewManager(message.NewManager(store2), store2, nil)
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Handler{Sessions: sessMgr}).Register(mux)
	return mux, sessID, shellID
}

func getMarksQuery(t *testing.T, mux *http.ServeMux, id, query string) (*httptest.ResponseRecorder, marksBody) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/shells/"+id+"/marks"+query, nil))
	var body marksBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil && rr.Code == http.StatusOK {
		t.Fatalf("decode marks body: %v (%s)", err, rr.Body.String())
	}
	return rr, body
}

// TestShellMarksWindowIsExact pins what a windowed response has to contain for
// the spans in it to be true: the span the window starts inside (or the cells for
// the first visible rows have no status), every mark that starts inside, and the
// first mark at or after the end (without which the last overlapping span has no
// end to derive).
//
// A window is the client's promise that it only draws those bytes, so answering
// with anything less is not a smaller answer but a wrong one: a missing carry
// leaves rows blank, a missing closer makes the last span swallow the rest of the
// log.
func TestShellMarksWindowIsExact(t *testing.T) {
	mux, _, shellID := marksFixture(t)

	// The marks are at 0/4/8/12 over 16 bytes. [5,9) starts inside the span at 4
	// and ends inside the span at 8, so it needs both — and the 12 that closes 8,
	// which is used as the last span's end rather than carried as a span of its own.
	rr, body := getMarksQuery(t, mux, shellID, "?start=5&end=9")
	if rr.Code != http.StatusOK {
		t.Fatalf("windowed GET = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	got := []int64{}
	for _, m := range body.Marks {
		got = append(got, m.Start)
	}
	want := []int64{4, 8}
	if len(got) != len(want) {
		t.Fatalf("window [5,9) returned starts %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("window [5,9) returned starts %v, want %v", got, want)
		}
	}
	if body.TotalBytes != 16 {
		t.Errorf("total_bytes = %d, want 16", body.TotalBytes)
	}
	// The spans must still tile: each ends where the next begins, and the last one
	// ends at the mark that closes it — a true end even though that boundary is
	// outside the window (which is why the response carries the closer).
	for i, m := range body.Marks {
		if i+1 < len(body.Marks) {
			if m.End != body.Marks[i+1].Start {
				t.Errorf("marks[%d] (start %d) ends at %d, want the next mark's %d", i, m.Start, m.End, body.Marks[i+1].Start)
			}
			continue
		}
		if m.End <= m.Start {
			t.Errorf("the last span %d..%d ends at or before its start", m.Start, m.End)
		}
		if m.End > body.TotalBytes {
			t.Errorf("the last span ends at %d, past total_bytes %d", m.End, body.TotalBytes)
		}
	}
	if last := body.Marks[len(body.Marks)-1]; last.End != 12 {
		t.Errorf("the last span ends at %d, want the closing mark's 12", last.End)
	}
	// The spans decide the window: the carry must cover its start.
	if body.Marks[0].End <= 5 {
		t.Errorf("the first span ends at %d and does not reach the window start 5", body.Marks[0].End)
	}

	// A window inside one span (no mark starts in it at all) still has to describe
	// it: the carry alone, ended by the mark that closes it.
	_, inside := getMarksQuery(t, mux, shellID, "?start=9&end=11")
	if len(inside.Marks) != 1 || inside.Marks[0].Start != 8 || inside.Marks[0].End != 12 {
		t.Errorf("window [9,11) = %+v, want one span 8..12", inside.Marks)
	}

	// A zero-width window is a legitimate question (an empty viewport, a row that
	// owns no bytes): it answers with the span covering that byte.
	_, empty := getMarksQuery(t, mux, shellID, "?start=4&end=4")
	if len(empty.Marks) != 1 || empty.Marks[0].Start != 4 || empty.Marks[0].End != 8 {
		t.Errorf("window [4,4) = %+v, want the span 4..8", empty.Marks)
	}

	// A window past the end of the log: the last span alone, ending at total_bytes.
	_, past := getMarksQuery(t, mux, shellID, "?start=14&end=99")
	if len(past.Marks) != 1 || past.Marks[0].Start != 12 || past.Marks[0].End != 16 {
		t.Errorf("window [14,99) = %+v, want the last span 12..16", past.Marks)
	}
}

// TestShellMarksWindowMatchesFullRead pins the window against the full index it
// is a slice of: for every window over the fixture, a windowed response has to
// agree with the full read about every span it returns, and the spans of the full
// read that overlap the window have to all be there. A window that silently
// dropped or duplicated a mark would still tile, which is why the comparison is
// made mark by mark rather than by shape.
func TestShellMarksWindowMatchesFullRead(t *testing.T) {
	mux, _, shellID := marksFixture(t)
	_, full := getMarks(t, mux, shellID)
	if len(full.Marks) != 4 {
		t.Fatalf("the fixture should have 4 marks, got %d", len(full.Marks))
	}
	for start := int64(0); start <= full.TotalBytes+1; start++ {
		for end := start; end <= full.TotalBytes+2; end++ {
			q := "?start=" + strconv.FormatInt(start, 10) + "&end=" + strconv.FormatInt(end, 10)
			rr, got := getMarksQuery(t, mux, shellID, q)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s = %d (%s), want 200", q, rr.Code, rr.Body.String())
			}
			if got.TotalBytes != full.TotalBytes {
				t.Errorf("%s: total_bytes = %d, want %d", q, got.TotalBytes, full.TotalBytes)
			}
			if len(got.Marks) == 0 {
				t.Errorf("%s: no marks; every window over a marked log has at least a span", q)
				continue
			}
			// Every returned span must be a span of the full index, unchanged.
			byStart := map[int64]markSpan{}
			for _, m := range full.Marks {
				byStart[m.Start] = m
			}
			for _, m := range got.Marks {
				ref, ok := byStart[m.Start]
				if !ok {
					t.Errorf("%s: span %d is not in the full index", q, m.Start)
					continue
				}
				if m != ref {
					t.Errorf("%s: span %d = %+v, full index has %+v", q, m.Start, m, ref)
				}
			}
			// Every span overlapping the window must be covered: the first returned
			// span ends at or after start (or, for a window past the log, is the
			// last), and no overlapping span is missing between first and last.
			first, last := got.Marks[0], got.Marks[len(got.Marks)-1]
			// A window may reach past the log: the last span then ends at the last byte,
			// which is the furthest anything can reach.
			reach := end
			if reach > got.TotalBytes {
				reach = got.TotalBytes
			}
			if first.Start > start && first.End > first.Start {
				t.Errorf("%s: the first span starts at %d, after the window start", q, first.Start)
			}
			if last.Start < reach && last.End < reach {
				t.Errorf("%s: the last span %d..%d does not reach %d", q, last.Start, last.End, reach)
			}
			for _, ref := range full.Marks {
				if ref.Start <= start || ref.Start >= end {
					continue // below the window, or at/after its end
				}
				found := false
				for _, m := range got.Marks {
					if m.Start == ref.Start {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: the mark at %d starts inside the window and is missing", q, ref.Start)
				}
			}
		}
	}
}

// TestShellMarksWindowParamsAreChecked pins the refusals: a half-named window and
// a window that cannot mean anything are reported, not quietly widened to the
// whole index. A client asking for [10, 5) has a bug, and answering it with the
// full index hides that bug behind a body that looks fine.
func TestShellMarksWindowParamsAreChecked(t *testing.T) {
	mux, _, shellID := marksFixture(t)
	for _, q := range []string{
		"?start=4",
		"?end=8",
		"?start=4&end=",
		"?start=&end=8",
		"?start=-1&end=8",
		"?start=-1&end=-1",
		"?start=8&end=4",
		"?start=abc&end=8",
		"?start=4&end=abc",
	} {
		rr, _ := getMarksQuery(t, mux, shellID, q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("GET marks%s = %d (%s), want 400", q, rr.Code, rr.Body.String())
		}
	}
	// A window that is merely unusual is not an error: the full one is a window
	// like any other, and so is an empty one at a real offset.
	for _, q := range []string{"?start=0&end=16", "?start=0&end=0", "?start=0&end=999"} {
		rr, _ := getMarksQuery(t, mux, shellID, q)
		if rr.Code != http.StatusOK {
			t.Errorf("GET marks%s = %d (%s), want 200", q, rr.Code, rr.Body.String())
		}
	}
}

// TestShellMarksWithoutWindowIsTheWholeIndex pins the compatibility promise: the
// endpoint's parameterless response is the full index, exactly as before the
// window existed, and asking for the whole range explicitly gives the same body.
// Old clients keep working, and the two shapes cannot drift apart.
func TestShellMarksWithoutWindowIsTheWholeIndex(t *testing.T) {
	mux, _, shellID := marksFixture(t)
	rrPlain, plain := getMarks(t, mux, shellID)
	if rrPlain.Code != http.StatusOK {
		t.Fatalf("plain GET = %d (%s), want 200", rrPlain.Code, rrPlain.Body.String())
	}
	rrFull, fullBody := getMarksQuery(t, mux, shellID, "?start=0&end="+strconv.FormatInt(plain.TotalBytes, 10))
	if rrFull.Code != http.StatusOK {
		t.Fatalf("full-window GET = %d (%s), want 200", rrFull.Code, rrFull.Body.String())
	}
	if fullBody.SessionID != plain.SessionID || fullBody.TotalBytes != plain.TotalBytes {
		t.Errorf("a full window reports %q/%d, the plain request %q/%d", fullBody.SessionID, fullBody.TotalBytes, plain.SessionID, plain.TotalBytes)
	}
	if len(plain.Marks) != 4 {
		t.Fatalf("the plain body has %d marks, want all 4", len(plain.Marks))
	}
	if rrPlain.Body.String() != rrFull.Body.String() {
		t.Errorf("a plain request and an explicit full window differ:\nplain: %s\nwindow: %s", rrPlain.Body.String(), rrFull.Body.String())
	}
}
