package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// POST /api/term is the same protocol over HTTP.
//
// These tests pin the four things that make that claim true rather than merely
// intended, because each of them could silently rot into "a second, smaller API
// that happens to look similar":
//
//  1. the reply is the SAME envelope the tool returns, slot for slot;
//  2. the command space is not a subset — a directive the tool can run, HTTP runs;
//  3. a failed command is a failing SLOT, not a failed request (a batch keeps its
//     successful slots), which is the one thing a REST client has to be able to
//     depend on;
//  4. the entrance is recorded, so a held command line can say which surface
//     submitted it instead of always claiming to be an agent.

// postTerm is one request to the handler under test.
func postTerm(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/term", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.TermHandler().ServeHTTP(rec, req)
	return rec
}

// termSlot decodes the reply. A single slot arrives bare (the tool does the same
// with structuredContent), a batch arrives wrapped in {"slots": [...]}, so the
// helper normalises both into a list.
func termSlots(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("reply is not JSON: %v, body: %s", err, rec.Body.String())
	}
	if raw, ok := body["slots"]; ok {
		list, ok := raw.([]any)
		if !ok {
			t.Fatalf("slots is %T, want an array", raw)
		}
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			out = append(out, item.(map[string]any))
		}
		return out
	}
	return []map[string]any{body}
}

// TestHTTPTermReturnsTheSameEnvelope: the HTTP reply carries the same fields the
// tool's structured projection does, for the same command.
//
// The point is not the JSON shape (which a test can assert trivially) but that
// there is ONE projection: if these ever disagree, an agent and a script are
// reading two descriptions of one operation and only one of them is true.
func TestHTTPTermReturnsTheSameEnvelope(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd":"help"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a successful command is a 200)", rec.Code)
	}
	slots := termSlots(t, rec)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0]["protocol"]; got != "TERM/1" {
		t.Errorf("protocol = %v, want TERM/1", got)
	}
	if got := slots[0]["code"]; got != float64(200) {
		t.Errorf("code = %v, want 200", got)
	}
	if body, _ := slots[0]["body"].(string); !strings.Contains(body, "help [directive]") {
		t.Errorf("body does not look like the help listing: %q", body)
	}

	// The same command through the tool surface must produce the same text.
	tool := termText(t, s, map[string]any{"cmd": "help"})
	if viaHTTP := rec.Header().Get("X-Termcp-Text"); viaHTTP != escapeHeader(tool) {
		t.Errorf("HTTP text projection differs from the tool's:\nhttp: %q\ntool: %q", viaHTTP, tool)
	}
}

// TestHTTPTermIsTheWholeCommandSpace: a window parameter the schema promises must
// work over HTTP too, or the HTTP entrance is a different protocol wearing the
// same name.
func TestHTTPTermIsTheWholeCommandSpace(t *testing.T) {
	s := newTestServer(t)

	// bytes_limit=0 is the "consume nothing" window: a generated reply comes back
	// empty with 204, which proves the argument reached parseTermArgs rather than
	// being ignored as an unknown body field.
	slots := termSlots(t, postTerm(t, s, `{"cmd":"help","bytes_limit":0}`))
	if got := slots[0]["code"]; got != float64(204) {
		t.Fatalf("code = %v, want 204 for a zero-byte window", got)
	}
	if body, _ := slots[0]["body"].(string); body != "" {
		t.Errorf("body = %q, want empty under bytes_limit=0", body)
	}

	// An out-of-range window is the argument error the tool gives, over HTTP.
	rec := postTerm(t, s, `{"cmd":"help","total_bytes_limit":0}`)
	slots = termSlots(t, rec)
	if got := slots[0]["code"]; got != float64(400) {
		t.Errorf("code = %v, want 400 for an invalid window", got)
	}
	if got := slots[0]["Error-Code"]; got != "invalid_argument" {
		t.Errorf("Error-Code = %v, want invalid_argument", got)
	}
}

// TestHTTPTermBatchIsOneReplyPerStatement pins the slot rule over HTTP: `;` runs
// several statements and gives one slot each, so a batch reply is a list, not a
// concatenated body.
//
// It also pins the narrower truth a caller has to know: a directive this build
// does not have fails at PARSE time, before any statement runs, so it is one
// failing slot for the whole command rather than a good slot next to a bad one.
// That is the tool's behaviour exactly — the HTTP entrance parses with the same
// parser — and a test that expected two slots would be asserting a second,
// friendlier protocol that does not exist.
func TestHTTPTermBatchIsOneReplyPerStatement(t *testing.T) {
	s := newTestServer(t)

	// Two statements that both succeed: one reply, two slots, each labelled with
	// the command it ran.
	slots := termSlots(t, postTerm(t, s, `{"cmd":"ps; help"}`))
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2 (one per statement)", len(slots))
	}
	if got := slots[0]["Path"]; got != "ps" {
		t.Errorf("first slot Path = %v, want ps", got)
	}
	if got := slots[1]["Path"]; got != "help" {
		t.Errorf("second slot Path = %v, want help", got)
	}

	// An unknown directive is a parse error: one slot, its own code, and the whole
	// directive table so the caller learns the shape without a second call.
	rec := postTerm(t, s, `{"cmd":"ps; nosuchdirective x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: nothing in the command parsed", rec.Code)
	}
	slots = termSlots(t, rec)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1 for a parse failure", len(slots))
	}
	if got := slots[0]["Error-Code"]; got != "unknown_directive" {
		t.Errorf("Error-Code = %v, want unknown_directive", got)
	}
	if usage, _ := slots[0]["Usage"].(string); !strings.Contains(usage, "|") {
		t.Errorf("Usage = %q, want the directive table", usage)
	}
}

// TestHTTPTermAllFailedIsAFailingStatus: a reply where nothing succeeded has no
// success to report, so the HTTP status follows the slot instead of pretending.
// Without this, `curl -f` cannot tell a typo from a working call.
func TestHTTPTermAllFailedIsAFailingStatus(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd":"nosuchdirective x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when every slot failed", rec.Code)
	}
}

// TestHTTPTermEntranceIsRecorded pins that the entrance reaches the record.
//
// This is the defect the REST entrance would have shipped with: the source was
// hardcoded "mcp" at its two call sites, so a command line submitted over HTTP
// would have been queued, and later logged, as an agent's. A reviewer reading
// "source: mcp" on a request a script sent is being told something false about
// who is asking.
func TestHTTPTermEntranceIsRecorded(t *testing.T) {
	s := newTestServer(t)
	if got := callerSource(withCallerSource(t.Context(), "rest")); got != "rest" {
		t.Errorf("callerSource = %q, want rest", got)
	}
	// The tool surface sets nothing, so it must keep reading as an agent.
	if got := callerSource(t.Context()); got != "mcp" {
		t.Errorf("callerSource with no claim = %q, want mcp", got)
	}
	// And the transcript attribution follows it: a REST write is the operator's
	// own input, not an agent's.
	if got := callerInputSource(withCallerSource(t.Context(), "rest")); got != session.InputFromAPI {
		t.Errorf("input source for rest = %v, want the API source", got)
	}
	if got := callerInputSource(t.Context()); got != session.InputFromAI {
		t.Errorf("input source with no claim = %v, want the AI source", got)
	}

	// The handler installs the claim itself, so a caller inherits it without
	// doing anything. This is the assertion that FAILS if the handler forgets:
	// testing withCallerSource alone would pass no matter what the handler does,
	// which is the difference between testing a helper and testing the wire.
	if got := sourceSeenByHandler(t, s); got != "rest" {
		t.Errorf("POST /api/term reached the command with source %q, want rest", got)
	}
}

// sourceSeenByHandler drives the real handler and reports the source a held
// command line was recorded with.
//
// The observable is the review queue, not the helper: a session is put in review
// mode, the request is made over HTTP, and the queue stamps Source on whatever it
// holds. Reading callerSource() directly would pass no matter what the handler
// does, which is the difference between testing a helper and testing the wire.
func sourceSeenByHandler(t *testing.T, s *Server) string {
	t.Helper()
	sessionID, _ := startGatedSession(t, s, 1, time.Minute)
	// A channel directive takes a handle, and a handle is `<session>:<index>` — the
	// form `ps` prints. A bare session id is refused at parse time, so this must
	// not be built from the session id alone.
	rec := postTerm(t, s, `{"cmd":"run `+sessionID+`:1 echo probe"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a held command line is a slot, not a request failure)", rec.Code)
	}
	slots := termSlots(t, rec)
	if got := slots[0]["code"]; got != float64(202) {
		t.Fatalf("code = %v, want 202 review_pending: %s", got, rec.Body.String())
	}
	queue := s.sessMgr.Get(sessionID).ApprovalQueue()
	if queue == nil {
		t.Fatal("a reviewed session has no approval queue")
	}
	pending := queue.List()
	if len(pending) == 0 {
		t.Fatal("nothing queued: the held command line was not submitted for review")
	}
	return pending[0].Source
}

// TestHTTPTermMalformedBodyIsAnEnvelope: a caller that mistyped its JSON reads the
// same format as one that mistyped a directive, instead of falling out of the
// protocol into a plain-text parse error.
func TestHTTPTermMalformedBodyIsAnEnvelope(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd": `)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	slots := termSlots(t, rec)
	if got := slots[0]["protocol"]; got != "TERM/1" {
		t.Errorf("protocol = %v, want TERM/1 even for a malformed body", got)
	}
	if got := slots[0]["Error-Code"]; got != "invalid_argument" {
		t.Errorf("Error-Code = %v, want invalid_argument", got)
	}
}

// TestCmdThatIsNotAStringIsA400 pins issue #131 P0-3, the worst-reported failure in
// the report.
//
// A `cmd` that is not a string used to be read with a loose getter that answered
// "" for anything it could not use, and "" is the GUIDE PAGE. So a call whose
// command had been mangled by a client bridge — the report's stdio wrapper — came
// back as a successful reply describing the whole protocol, and nothing said the
// command had been lost. From the caller's side, `run <h> echo OK` sometimes just
// returned the guide, at random, for hours.
//
// The three states have three answers, and they must stay distinct:
//
//	absent      -> 200 + guide   (the documented wake-up call)
//	null        -> 200 + guide   (JSON's spelling of "no value")
//	not a string -> 400          (a command was sent and did not survive)
func TestCmdThatIsNotAStringIsA400(t *testing.T) {
	s := newTestServer(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"number", `{"cmd": 42}`},
		{"array", `{"cmd": ["ps"]}`},
		{"object", `{"cmd": {"line": "ps"}}`},
		{"bool", `{"cmd": true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postTerm(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s; a mangled cmd must not look like the guide", rec.Code, tc.name)
			}
			slots := termSlots(t, rec)
			if got := slots[0]["Error-Code"]; got != "invalid_argument" {
				t.Errorf("Error-Code = %v, want invalid_argument", got)
			}
			body, _ := slots[0]["body"].(string)
			if !strings.Contains(body, "cmd must be a string") {
				t.Errorf("the error must name the argument: %q", body)
			}
		})
	}
}

// TestAbsentCmdIsStillTheGuide is the other half: refusing a mangled cmd must not
// turn the one legal empty call into an error.
func TestAbsentCmdIsStillTheGuide(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{
		`{}`,
		`{"cmd": null}`,
		`{"cmd": ""}`,
	} {
		rec := postTerm(t, s, body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", body, rec.Code)
			continue
		}
		slots := termSlots(t, rec)
		text, _ := slots[0]["body"].(string)
		if !strings.Contains(text, "termcp terminal") {
			t.Errorf("%s did not return the guide: %q", body, text[:min(120, len(text))])
		}
	}
}

// TestHTTPTermEmptyBodyIsTheGuide: an empty body means the same thing an empty
// cmd does. The guidance call must not be reachable only through the tool.
func TestHTTPTermEmptyBodyIsTheGuide(t *testing.T) {
	s := newTestServer(t)

	slots := termSlots(t, postTerm(t, s, ""))
	if got := slots[0]["code"]; got != float64(200) {
		t.Fatalf("code = %v, want 200", got)
	}
	body, _ := slots[0]["body"].(string)
	if !strings.Contains(body, "termcp terminal") {
		t.Errorf("empty body did not return the guide: %q", body[:min(120, len(body))])
	}
}

// TestHTTPTermAdmitsThroughTheCapabilityTable is the security half of the claim
// that this entrance reuses admission rather than re-deriving it.
//
// A session carrying a template's whitelist must refuse over HTTP exactly as it
// refuses over MCP — same capability, same refusal text. If this ever passes
// while the tool surface refuses (or the reverse), the entrance became a way
// around the whitelist, which is the failure the whole capability table exists
// to prevent.
func TestHTTPTermAdmitsThroughTheCapabilityTable(t *testing.T) {
	s := newTestServer(t)

	// A restricted session that may not write to its shell.
	sid := guardTestSession(t, s, []string{capability.ShellList}, nil)

	// `ps` only lists, so it is admitted.
	slots := termSlots(t, postTerm(t, s, `{"cmd":"ps"}`))
	if got := slots[0]["code"]; got != float64(200) {
		t.Errorf("ps over HTTP = %v, want 200 for a listing-only template", got)
	}

	// `run` writes to the shell, which the template does not grant.
	rec := postTerm(t, s, `{"cmd":"run `+sid+`:1 echo hi"}`)
	slots = termSlots(t, rec)
	code := slots[0]["code"]
	if code == float64(200) {
		t.Fatalf("run over HTTP succeeded against a template that does not grant shell.input: %s", rec.Body.String())
	}
	if got := slots[0]["Error-Code"]; got != "forbidden" {
		t.Errorf("Error-Code = %v, want forbidden", got)
	}

	// The tool surface must answer the same way for the same command: one
	// decision, two entrances.
	toolText := termText(t, s, map[string]any{"cmd": "run " + sid + ":1 echo hi"})
	if !strings.Contains(toolText, "403") {
		t.Errorf("the tool surface did not refuse the same command, so the two entrances disagree:\n%s", toolText)
	}
}

// TestStructuredContentIsOffByDefault pins the default that removes the duplicate.
//
// A tool result has two projections and they carry the SAME bytes: the rendered
// text in `content`, and the envelope again as `structuredContent`. Sending both
// bills every reply twice, and a client that prints what it receives shows a human
// the same answer in two dialects on every call — measured +25% on `help`, +57% on
// a 200-line read.
//
// The first attempt at this was two switches that both pointed the wrong way:
// `structured_only` was meant to turn structuredContent off but kept the JSON and
// dropped the text; `content_only` was the same intent spelled backwards, making
// "no duplicate" the thing a caller had to ask for. One positive flag with the
// quiet default says what it does.
//
// So the default is text only, the bytes are saved, and `structured_content: true`
// is how a JSON client asks for the twin.
func TestStructuredContentIsOffByDefault(t *testing.T) {
	s := newTestServer(t)

	// The default: content only, no JSON twin.
	def := termCall(t, s, map[string]any{"cmd": "help"})
	if len(def.Content) == 0 {
		t.Fatal("the default result must carry content: it is the projection that survives")
	}
	if def.StructuredContent != nil {
		t.Error("the default result must NOT carry structuredContent: that is the duplicate being paid for")
	}
	if def.IsError {
		t.Error("a successful reply must not be flagged as an error")
	}

	// Asked for: the JSON twin appears, and carries the same bytes as the text.
	both := termCall(t, s, map[string]any{"cmd": "help", "structured_content": true})
	if both.StructuredContent == nil {
		t.Fatal("structured_content:true must send structuredContent")
	}
	if len(both.Content) == 0 {
		t.Error("structured_content adds the JSON twin; it must not drop the content projection")
	}
	if got, want := toolResultText(both), toolResultText(def); got != want {
		t.Errorf("asking for the twin changed the content projection:\n got %q\nwant %q", got, want)
	}

	// The envelope itself is unchanged either way: this is a wire switch, not a
	// change of content. Read at the value level, where the projection is built.
	withTwin := s.Term(t.Context(), "help", map[string]any{"structured_content": true})
	plain := s.Term(t.Context(), "help", map[string]any{})
	if withTwin.Text() != plain.Text() {
		t.Error("structured_content changed the body; it must only add the JSON projection")
	}
	if len(withTwin.Structured()) != 1 || withTwin.Structured()[0]["body"] == "" {
		t.Fatal("the reply must always build its structured form: the switch is about the wire")
	}
	if got := withTwin.Structured()[0]["code"]; got != 200 {
		t.Errorf("code = %v (%T), want 200", got, got)
	}
	if got := withTwin.Structured()[0]["Path"]; got != "help" {
		t.Errorf("Path = %v, want help", got)
	}
}

// TestStructuredContentOverHTTP pins the same switch on the REST entrance.
//
// The two entrances differ in which projection is primary, and that is why the
// switch means the mirror image of itself here. Over MCP the `content` field is
// mandatory, so the text is the default and structuredContent is the optional
// twin. Over REST the BODY is the JSON projection and the rendered form rides
// along as the optional X-Termcp-Text header — so on this entrance the default
// already omits the text, and `structured_content` only ever adds the copy that
// is already the body.
//
// What both entrances must agree on is the rule, not the field: the duplicate is
// opt-in, and asking for it never changes the bytes, only whether they are sent a
// second time.
func TestStructuredContentOverHTTP(t *testing.T) {
	s := newTestServer(t)

	def := postTerm(t, s, `{"cmd":"help"}`)
	if def.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", def.Code)
	}
	// The body is the contract and is always present.
	if got := termSlots(t, def)[0]["body"]; got == nil || got == "" {
		t.Error("the JSON body must always carry the bytes")
	}

	// Asking for the twin does not remove the body: on REST the body IS the twin.
	asked := postTerm(t, s, `{"cmd":"help","structured_content":true}`)
	if asked.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", asked.Code)
	}
	if got := termSlots(t, asked)[0]["body"]; got == nil || got == "" {
		t.Error("structured_content must not empty the REST body: it is the JSON projection")
	}
}

// TestTermDocsAreGenerated pins that docs/api.md's argument table is the table
// this package renders.
//
// It replaces a name-matching test that was one bug away from useless: it first
// accepted any backticked mention of an argument anywhere in §16, so deleting the
// table row still passed because a prose paragraph happened to name it. Matching
// names was always a proxy — the thing that must be true is "the doc contains
// exactly what the generator produces", and that is a comparison, not a guess.
//
// What this catches, concretely: an argument added to termArgDecls (so it appears
// in the schema and is parsed) without regenerating the doc; a doc row edited by
// hand; a marker moved. The fix in every case is `go run ./cmd/termdoc`.
func TestTermDocsAreGenerated(t *testing.T) {
	doc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}
	body := string(doc)

	// The markers must exist: RenderTermDoc reports "unchanged" when they are
	// missing, so without this check a doc that lost them would pass silently —
	// which is the same class of blind spot the previous version had.
	if !strings.Contains(body, termDocBegin) || !strings.Contains(body, termDocEnd) {
		t.Fatalf("docs/api.md is missing the termdoc markers; the argument table cannot be generated")
	}

	if _, changed := RenderTermDoc(body); changed {
		t.Errorf("docs/api.md is stale: run `go run ./cmd/termdoc`")
	}
}

// TestTermDocTableCoversTheSchema pins the other direction: every argument the
// schema declares has a generated row, and no row names an argument the schema
// does not have.
//
// The generated-table test above would catch both, but only by reporting "stale" —
// which is a poorer diagnosis than "the table is missing X". This one names it.
func TestTermDocTableCoversTheSchema(t *testing.T) {
	s := newTestServer(t)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(callMCP(t, s, "tools/list", nil), &list); err != nil {
		t.Fatal(err)
	}
	var props map[string]any
	for i := range list.Tools {
		if list.Tools[i].Name == termToolName {
			props, _ = list.Tools[i].InputSchema["properties"].(map[string]any)
		}
	}
	if len(props) == 0 {
		t.Fatalf("tools/list has no %q schema", termToolName)
	}

	rows := docTableRows()
	for name := range props {
		if !strings.Contains(rows, "| `"+name+"` |") {
			t.Errorf("the rendered table has no row for the %q argument the schema declares", name)
		}
	}
	// And nothing extra: a row for an argument that does not exist is a doc
	// promising behaviour the build cannot deliver.
	for _, d := range termArgDecls {
		if _, ok := props[d.Name]; !ok {
			t.Errorf("the table declares %q but the schema does not", d.Name)
		}
	}
}

// TestEscapeHeaderKeepsCarriageReturns pins that the text projection of a reply is
// byte-identical to the JSON one.
//
// The two are generated from ONE value, so they must not disagree — and they did:
// escapeHeader handled '\n' and let '\r' fall through `default`, which appended
// nothing. A Path carrying a CR therefore survived in the body and vanished from
// X-Termcp-Text, so a caller reading the header saw a path that does not exist with
// no sign a byte had been removed.
func TestEscapeHeaderKeepsCarriageReturns(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb", `a\nb`},
		{"a\rb", `a\rb`},
		{"a\r\nb", `a\r\nb`},
		{"plain", "plain"},
		{"", ""},
	} {
		if got := escapeHeader(tc.in); got != tc.want {
			t.Errorf("escapeHeader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHeaderAndBodyAgreeOnAPathWithCR is the same property from the outside: one
// reply, two projections, one answer.
func TestHeaderAndBodyAgreeOnAPathWithCR(t *testing.T) {
	s := newTestServer(t)
	rec := postTerm(t, s, `{"cmd": "info \"a\rb\""}`)
	slots := termSlots(t, rec)
	bodyPath, _ := slots[0]["Path"].(string)
	if !strings.Contains(bodyPath, "\r") {
		t.Fatalf("the JSON Path lost the CR: %q; this test needs a path that has one", bodyPath)
	}
	// Undo the header escaping and compare: the header must still describe the same
	// path, CR included.
	header := rec.Header().Get("X-Termcp-Text")
	unescaped := strings.NewReplacer(`\n`, "\n", `\r`, "\r").Replace(header)
	if !strings.Contains(unescaped, bodyPath) {
		t.Errorf("projections disagree:\n  body   Path = %q\n  header text = %q", bodyPath, unescaped)
	}
}
