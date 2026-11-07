package server_test

// An embedder's point of view, on purpose: this file is in the _test package so
// that everything it touches is what a program outside this repository can see.
// A test in package server could reach an unexported field and stay green while
// the exported surface was unusable.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
	"github.com/open-mcp-ai/termcp/term/server"
)

// caps is a capability layer that knows one verb. Everything else a TERM/1 server
// does — sequencing, short-circuiting, slot alignment, the envelope — is supposed
// to arrive without it.
type caps struct{}

func (caps) Handle(_ context.Context, st term.Statement) []term.Slot {
	if st.Op != "greet" {
		return []term.Slot{{
			Code:    400,
			Headers: map[string]string{"Error-Code": "unknown_directive"},
			Body:    "unknown directive: " + st.Op + "\n",
		}}
	}
	return []term.Slot{{Code: 200, Body: "hello " + strings.Join(st.Args, " ") + "\n"}}
}

func newShell(t *testing.T, opts server.Options) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.New(opts))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, body string) (int, map[string]any, string) {
	t.Helper()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return resp.StatusCode, out, resp.Header.Get("X-Termcp-Text")
}

// The claim the whole extraction rests on: a Handler and a Grammar are enough.
func TestAHandlerAndAVocabularyAreAServer(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{
		{Name: "greet", Usage: "greet <name>", Detail: "greets one name"},
		{Name: "nope", Usage: "nope"},
	})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	status, body, text := post(t, ts, `{"cmd":"greet world"}`)
	if status != 200 {
		t.Fatalf("greet world = %d, want 200 (body %v)", status, body)
	}
	// The header carries the RENDERED envelope, status line included — that is what
	// makes a curl call readable without decoding JSON. The body is the contract.
	//
	// The newlines in it are LITERAL `\n`, not raw ones: a header value cannot
	// contain a newline, so the rendering is escaped rather than dropped or
	// truncated. A test that asked for the raw form here would be asserting a bug.
	if !strings.HasPrefix(text, "TERM/1 200 OK") || !strings.Contains(text, "hello world") {
		t.Errorf("X-Termcp-Text = %q, want the rendered envelope carrying the slot body", text)
	}
}

// A capability refusal is not a transport failure: the request succeeded, the
// reply reports what happened inside it. If this ever returns 500 the distinction
// between "the server is broken" and "this command failed" is gone.
func TestACapabilityFailureIsNotATransportFailure(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{
		{Name: "greet", Usage: "greet <name>"},
		{Name: "nope", Usage: "nope"},
	})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	// `nope` IS in the vocabulary, so the line parses and the handler is the one
	// that refuses it. That distinction is the whole test: a parse failure would
	// never reach a capability layer, and a 400 here would say "the request was
	// broken" instead of "this command was refused".
	status, body, _ := post(t, ts, `{"cmd":"greet a || nope"}`)
	if status != 200 {
		t.Fatalf("a batch with one success = %d, want 200: %v", status, body)
	}
	slots, ok := body["slots"].([]any)
	if !ok {
		t.Fatalf("a two-slot reply must arrive as {\"slots\": [...]}, got %T", body["slots"])
	}
	// Slot alignment is the protocol's promise: slot i is statement i, including
	// the branch that never ran.
	if len(slots) != 2 {
		t.Fatalf("2 statements = %d slots, want 1:1 alignment: %v", len(slots), slots)
	}
	first, _ := slots[0].(map[string]any)
	if first["code"] != float64(200) {
		t.Errorf("the statement that ran = %v, want 200", first["code"])
	}
	second, _ := slots[1].(map[string]any)
	if second["code"] != float64(208) {
		t.Errorf("the || branch that did not run = %v, want 208", second["code"])
	}
}

// An empty call is legal and is the guide. The generated guide lists the
// embedder's OWN verbs — a server whose vocabulary shares nothing with this
// project's must not be told about this project's.
func TestAnEmptyCallAnswersTheEmbeddersOwnGuide(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{
		{Name: "greet", Usage: "greet <name>"},
		{Name: "wave", Usage: "wave"},
	})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	for _, body := range []string{"{}", `{"cmd":""}`} {
		status, out, _ := post(t, ts, body)
		if status != 200 {
			t.Fatalf("%s = %d, want 200: an empty call never errors", body, status)
		}
		text, _ := out["body"].(string)
		for _, want := range []string{"greet <name>", "wave"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s guide is missing %q:\n%s", body, want, text)
			}
		}
		// Both verbs are the embedder's; neither is termcp's.
		for _, never := range []string{"shell_input", "session_id"} {
			if strings.Contains(text, never) {
				t.Errorf("%s guide mentions this project's vocabulary %q:\n%s", body, never, text)
			}
		}
	}
}

// A guide that points at a verb the server does not have sends a lost caller into
// a second error. The generated text used to end by recommending `help <verb>`
// unconditionally — true for this project's server, false for an embedder's.
func TestTheGuideOnlyMentionsVerbsThatExist(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{{Name: "greet", Usage: "greet <name>"}})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	_, out, _ := post(t, ts, `{}`)
	text, _ := out["body"].(string)
	if strings.Contains(text, "help") {
		t.Errorf("the guide recommends `help`, which this vocabulary does not declare: %s", text)
	}

	// And with a help verb declared, the pointer comes back — one rule, both cases.
	g2 := directive.NewGrammar([]directive.Op{
		{Name: "greet", Usage: "greet <name>"},
		{Name: "help", Usage: "help [verb]"},
	})
	ts2 := newShell(t, server.Options{Term: term.New(g2, term.WithHandler(caps{}))})
	_, out2, _ := post(t, ts2, `{}`)
	text2, _ := out2["body"].(string)
	if !strings.Contains(text2, "help <verb>") {
		t.Errorf("a vocabulary with `help` should say so: %s", text2)
	}
}

// A transport that meets a syntax error has to answer it in the envelope the
// caller reads, not as a transport error: the error text IS the product.
func TestASyntaxErrorArrivesAsAnEnvelope(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{{Name: "greet", Usage: "greet <name>"}})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	status, out, _ := post(t, ts, `{"cmd":"greet a &&"}`)
	if status != 400 {
		t.Fatalf("a syntax error = %d, want 400: %v", status, out)
	}
	text, _ := out["body"].(string)
	if text == "" {
		t.Error("a syntax error with no body teaches nothing; it must say what was wrong")
	}
}

// `cmd` of the wrong type is refused, not folded into "absent". The two used to be
// the same answer, and a caller that sent a number was handed the guide.
func TestANonStringCmdIsRefusedNotIgnored(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{{Name: "greet", Usage: "greet <name>"}})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	status, out, _ := post(t, ts, `{"cmd":42}`)
	if status != 400 {
		t.Fatalf(`{"cmd":42} = %d, want 400: %v`, status, out)
	}
	text, _ := out["body"].(string)
	if !strings.Contains(text, "must be a string") {
		t.Errorf("the refusal must name the type it got, got %q", text)
	}
}

// Options.Params takes over the whole argument object, which means it also takes
// over the entry rules. Pinned because the doc says so: an embedder that sets it
// and then wonders why `{}` is not a guide should find a test, not a surprise.
func TestParamsOwnsTheArgumentObject(t *testing.T) {
	called := 0
	ts := newShell(t, server.Options{Params: server.HandlerFunc(func(_ context.Context, args map[string]any) envelope.Reply {
		called++
		if _, ok := args["window"]; !ok {
			t.Errorf("Params received %v; it must get the whole object", args)
		}
		return envelope.Reply{Slots: []envelope.Slot{{Code: 200, Body: "mine\n"}}}
	})})

	status, _, text := post(t, ts, `{"cmd":"","window":5}`)
	if status != 200 || !strings.Contains(text, "mine") {
		t.Fatalf("Params reply = %d %q, want 200 carrying the rendered slot", status, text)
	}
	if called != 1 {
		t.Errorf("Params called %d times, want 1", called)
	}
}

// ReplyStatus is exported because a custom entrance needs the same rule as this
// one; two copies of "did the request succeed" is two answers to one question.
func TestReplyStatusSplitsRequestFromCommand(t *testing.T) {
	cases := []struct {
		name  string
		reply envelope.Reply
		want  int
	}{
		{"no slots", envelope.Reply{}, 204},
		{"one ok", envelope.Reply{Slots: []envelope.Slot{{Code: 200}}}, 200},
		{"one refused", envelope.Reply{Slots: []envelope.Slot{{Code: 404}}}, 404},
		{"partial", envelope.Reply{Slots: []envelope.Slot{{Code: 404}, {Code: 200}}}, 200},
		{"all failed", envelope.Reply{Slots: []envelope.Slot{{Code: 404}, {Code: 400}}}, 404},
	}
	for _, tc := range cases {
		if got := server.ReplyStatus(tc.reply); got != tc.want {
			t.Errorf("%s: ReplyStatus = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestAnUnknownVerbAnswersWithThisServersOwnVerbs is the second half of the
// unknown-directive rule. Naming the failure is only half an answer; the caller
// needs the shape that would have worked. termcp's own entrance has always replied
// with its directive table, and an embedder's must reply with ITS table — copying
// this project's list would name verbs the server does not have, which is the same
// defect as a guide that recommends a `help` verb it never declared.
func TestAnUnknownVerbAnswersWithThisServersOwnVerbs(t *testing.T) {
	g := directive.NewGrammar([]directive.Op{
		{Name: "greet", Usage: "greet <name>"},
		{Name: "wave", Usage: "wave"},
	})
	ts := newShell(t, server.Options{Term: term.New(g, term.WithHandler(caps{}))})

	_, out, _ := post(t, ts, `{"cmd":"nosuchverb x"}`)
	usage, _ := out["Usage"].(string)
	if usage == "" {
		t.Fatalf("an unknown verb got no list of the verbs that exist: %v", out)
	}
	for _, want := range []string{"greet", "wave"} {
		if !strings.Contains(usage, want) {
			t.Errorf("the verb list is missing %q: %q", want, usage)
		}
	}
	// And it is THIS server's vocabulary, not termcp's.
	for _, never := range []string{"connect", "session", "shell"} {
		if strings.Contains(usage, never) {
			t.Errorf("the verb list mentions %q, which this server does not declare: %q", never, usage)
		}
	}
}
