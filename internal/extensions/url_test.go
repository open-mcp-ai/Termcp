package extensions

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// The endpoint form is an alternative to command/args, so the two rules that
// matter are "one of them" and "the URL is usable". Both are checked at save
// time: a template that cannot work should be refused by whoever wrote it, not
// discovered by the reconciler three walks later.
func TestURLTemplateValidation(t *testing.T) {
	base := func() *Extension {
		return &Extension{Name: "http", Target: "internal", Mode: "pipe"}
	}
	t.Run("url alone is valid", func(t *testing.T) {
		e := base()
		e.URL = "https://mcp.example.com/mcp"
		e.Normalize()
		if err := e.Validate(); err != nil {
			t.Fatalf("a URL template should validate: %v", err)
		}
	})
	t.Run("url and command together is refused", func(t *testing.T) {
		e := base()
		e.URL = "https://mcp.example.com/mcp"
		e.Command = "npx"
		e.Normalize()
		err := e.Validate()
		if err == nil || !strings.Contains(err.Error(), "alternatives") {
			t.Fatalf("err = %v, want one about alternatives", err)
		}
	})
	t.Run("a pipe with neither is still refused", func(t *testing.T) {
		e := base()
		e.Normalize()
		err := e.Validate()
		if err == nil || !strings.Contains(err.Error(), "requires a command") {
			t.Fatalf("err = %v, want the commandless-pipe refusal", err)
		}
	})
	t.Run("a pty with a url is refused", func(t *testing.T) {
		e := base()
		e.Mode = "pty"
		e.URL = "https://mcp.example.com/mcp"
		e.Normalize()
		err := e.Validate()
		if err == nil || !strings.Contains(err.Error(), "mode=pipe") {
			t.Fatalf("err = %v, want one about the mode", err)
		}
	})
	for _, tc := range []struct{ name, url, want string }{
		{"not a url", "://nope", "invalid url"},
		{"wrong scheme", "ftp://host/mcp", "http or https"},
		{"no host", "http:///mcp", "names no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			e.URL = tc.url
			e.Normalize()
			err := e.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// The endpoint form has to be reachable through the AUTHORING path, not only
// through a hand-built Extension.
//
// A create starts from the conventional defaults for the fields an author has not
// named. ApplyArgs used to set URL beside a defaulted command, so Validate refused
// the pair with "url and command are alternatives" — the author never sent a
// command, and the refusal named a rule they had not broken. Only a hand-written
// document worked, because a document is taken literally with no defaults behind it.
//
// The earlier end-to-end test (TestURLTemplateAvailableWithoutAnExternalBinary)
// missed this because it clears the command by hand before saving, which is
// precisely the step the real callers do not take.
func TestCreateURLTemplateThroughApplyArgs(t *testing.T) {
	t.Run("a create needs no hand-clearing of the defaults", func(t *testing.T) {
		ext := Extension{Mode: "pipe"}
		ext.Name, ext.Target = "http-ext", "internal"
		if err := ApplyArgs(&ext, map[string]any{
			"target": "internal",
			"url":    "https://mcp.example.com/mcp",
		}, false); err != nil {
			t.Fatalf("ApplyArgs: %v", err)
		}
		if ext.Command != "" || ext.Args != nil {
			t.Fatalf("the stdio default survived a URL template: command=%q args=%v", ext.Command, ext.Args)
		}
		if err := ext.Validate(); err != nil {
			t.Fatalf("the documented url shape must be creatable: %v", err)
		}
	})

	t.Run("the notes describe the shape that was created", func(t *testing.T) {
		// #159 D1: the preset filled in the STDIO recipe before the URL was known,
		// so a create kept instructions whose first line tells the reader to run a
		// command this template does not have. An author's own notes are still
		// theirs, which is the other half of the rule.
		viaURL := Extension{Mode: "pipe"}
		viaURL.Name, viaURL.Target = "http-ext", "internal"
		if err := ApplyArgs(&viaURL, map[string]any{
			"url": "https://mcp.example.com/mcp",
		}, false); err != nil {
			t.Fatalf("ApplyArgs: %v", err)
		}
		if viaURL.Notes != mcpHTTPNotes {
			t.Errorf("a url create kept the stdio recipe:\n%s", viaURL.Notes)
		}

		viaCommand := Extension{Mode: "pipe"}
		viaCommand.Name, viaCommand.Target = "stdio-ext", "internal"
		if err := ApplyArgs(&viaCommand, map[string]any{
			"command": "node", "args": []any{"server.js"},
		}, false); err != nil {
			t.Fatalf("ApplyArgs: %v", err)
		}
		if viaCommand.Notes != mcpNotes {
			t.Errorf("a stdio create should keep the stdio recipe, got:\n%s", viaCommand.Notes)
		}

		mine := Extension{Mode: "pipe"}
		mine.Name, mine.Target = "mine", "internal"
		if err := ApplyArgs(&mine, map[string]any{
			"url": "https://mcp.example.com/mcp", "notes": "ask the billing team first",
		}, false); err != nil {
			t.Fatalf("ApplyArgs: %v", err)
		}
		if mine.Notes != "ask the billing team first" {
			t.Errorf("an author's notes were overwritten: %q", mine.Notes)
		}
	})

	t.Run("an explicit command still wins and still collides", func(t *testing.T) {
		// The fix clears only what the caller did not name. A caller that names
		// both answered one question twice, and that refusal is the point of the
		// rule — so it must survive.
		ext := Extension{Mode: "pipe"}
		ext.Name, ext.Target = "both", "internal"
		if err := ApplyArgs(&ext, map[string]any{
			"url":     "https://mcp.example.com/mcp",
			"command": "npx",
		}, false); err != nil {
			t.Fatalf("ApplyArgs: %v", err)
		}
		if ext.Command != "npx" {
			t.Fatalf("an explicit command was dropped: %q", ext.Command)
		}
		err := ext.Validate()
		if err == nil || !strings.Contains(err.Error(), "alternatives") {
			t.Fatalf("err = %v, want the alternatives refusal", err)
		}
	})

	t.Run("a stored stdio template converts to an endpoint on update", func(t *testing.T) {
		// The same collision blocks the other direction: the command is already in
		// the stored template, and setting a url left it there. An update must be
		// able to change the shape, which is what the notes refresh also assumes.
		ext := Extension{Mode: "pipe"}
		ext.Name, ext.Target = "conv", "internal"
		if err := ApplyArgs(&ext, map[string]any{
			"command": "npx", "args": []any{"-y", "example-mcp-server"},
		}, false); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := ApplyArgs(&ext, map[string]any{"url": "https://mcp.example.com/mcp"}, true); err != nil {
			t.Fatalf("update: %v", err)
		}
		if ext.Command != "" || ext.Args != nil {
			t.Fatalf("the stored command survived the conversion: %q %v", ext.Command, ext.Args)
		}
		if err := ext.Validate(); err != nil {
			t.Fatalf("a converted template must validate: %v", err)
		}
	})

	t.Run("clearing the url restores the commandless-pipe refusal", func(t *testing.T) {
		// The url branch must not become a way to produce a pipe with nothing to
		// run: clearing the endpoint leaves neither side, and Validate says so.
		ext := Extension{Mode: "pipe"}
		ext.Name, ext.Target = "gone", "internal"
		if err := ApplyArgs(&ext, map[string]any{"url": "https://mcp.example.com/mcp"}, false); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := ApplyArgs(&ext, map[string]any{"url": ""}, true); err != nil {
			t.Fatalf("update: %v", err)
		}
		err := ext.Validate()
		if err == nil || !strings.Contains(err.Error(), "requires a command") {
			t.Fatalf("err = %v, want the commandless-pipe refusal", err)
		}
	})
}

// A blank header name is dropped rather than sent as an unparseable request
// line; the map is left nil when nothing survives, so an empty table and no
// table are the same template.
func TestNormalizeDropsBlankHeaders(t *testing.T) {
	e := &Extension{Target: "internal", Headers: map[string]string{
		"":          "dropped",
		"   ":       "dropped",
		" X-Real ":  "kept",
		"X-Api-Key": "also kept",
	}}
	e.Normalize()
	if _, ok := e.Headers[""]; ok {
		t.Error("a blank header name should be dropped")
	}
	if got := e.Headers["X-Real"]; got != "kept" {
		t.Errorf("a padded name should be trimmed and kept, got %q", got)
	}
	if len(e.Headers) != 2 {
		t.Errorf("headers = %v, want exactly the two named ones", e.Headers)
	}

	empty := &Extension{Target: "internal", Headers: map[string]string{"": "x"}}
	empty.Normalize()
	if empty.Headers != nil {
		t.Errorf("a map with nothing left should be nil, got %v", empty.Headers)
	}
}

// ---------------------------------------------------------------------------
// the command a URL template runs
// ---------------------------------------------------------------------------

// The URL form's argv is derived, never stored: it is the relay's name, the
// endpoint, then the headers. Deterministic order matters because the same
// template must produce the same argv every walk (the fingerprint is taken over
// it).
func TestBridgeCommandRendersURLAndHeaders(t *testing.T) {
	e := &Extension{
		Target: "internal", Mode: "pipe",
		URL:     "https://mcp.example.com/mcp",
		Headers: map[string]string{"X-Api-Key": "sekrit", "X-Tenant": "acme"},
	}
	cmd, args := bridgeCommand(e, "")
	if cmd != "termcp-mcp-bridge" {
		t.Errorf("command = %q, want the built-in relay", cmd)
	}
	want := []string{"https://mcp.example.com/mcp", "--header", "X-Api-Key=sekrit", "--header", "X-Tenant=acme"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
	// Running it twice must produce the identical argv: the headers are a map,
	// and a walk that rendered them in a different order would look like an edit.
	if _, again := bridgeCommand(e, ""); strings.Join(again, "\x00") != strings.Join(args, "\x00") {
		t.Errorf("rendering is not stable: %v then %v", args, again)
	}
}

// THE TARGET OF A url TEMPLATE IS THE MACHINE THE CONNECTION LEAVES FROM (#233),
// so any profile is accepted: the relay always runs on the loopback channel, and a
// remote target rides in the argv as `--via`, which makes 127.0.0.1 mean THAT
// host's loopback. What used to be refused — a remote target — is now the point.
func TestURLTargetNamesTheEgressMachine(t *testing.T) {
	e := &Extension{
		Name: "remote-http", Target: "somewhere", Mode: "pipe",
		URL: "https://mcp.example.com/mcp",
	}
	e.Normalize()
	if err := e.Validate(); err != nil {
		t.Fatalf("a remote target is the egress machine and must validate: %v", err)
	}

	// A remote target travels as --via; the loopback one does not need it.
	if _, args := bridgeCommand(e, "somewhere"); len(args) < 3 || args[1] != "--via" || args[2] != "somewhere" {
		t.Fatalf("a remote target must ride as --via, got %v", args)
	}
	if _, args := bridgeCommand(e, ""); len(args) != 1 || args[0] != e.URL {
		t.Fatalf("an internal target must not carry --via, got %v", args)
	}

	// The loopback spelling is still accepted, in every casing — the name is what
	// decides whether --via is needed at all.
	for _, spell := range []string{"internal", " Internal ", "INTERNAL"} {
		e.Target = spell
		e.Normalize()
		if err := e.Validate(); err != nil {
			t.Fatalf("internal (spelled %q) should be accepted: %v", spell, err)
		}
	}
}

// The endpoint form comes up without any process being installed on the host:
// the reconciler runs termcp's own relay on the channel, and the session reports
// available once the relay has proved it can stay up.
func TestURLTemplateAvailableWithoutAnExternalBinary(t *testing.T) {
	// The endpoint is created first so its teardown runs last (t.Cleanup is
	// LIFO): the relay holds a listening GET stream open for as long as the
	// session lives, so closing the server before the session that speaks to it
	// would wait on the very connection the test is about to tear down.
	mcpURL := startStreamableEchoEndpoint(t)
	r := startReconciler(t)

	ext := Extension{Mode: "pipe"}
	ext.Name = "http-mcp"
	ext.Target = "internal"
	// The author names a server instead of a program: clearing the preset's
	// command is what makes this a URL template.
	ext.Command, ext.Args = "", nil
	ext.URL = mcpURL
	ext.Headers = map[string]string{"X-Api-Key": "sekrit"}
	ApplyDefaults(&ext)
	if err := SaveStruct(r.store, &ext); err != nil {
		t.Fatalf("saving the endpoint template: %v", err)
	}

	wantExtension(t, r, "http-mcp")
	st := waitForStatus(t, r, "http-mcp", StatusAvailable)
	if st.ShellID == "" {
		t.Fatal("an endpoint extension should report the channel its relay runs on")
	}
	if st.SessionID == "" {
		t.Fatal("an endpoint extension should report its session")
	}

	// What the plan runs is the relay plus the URL and headers — never the
	// preset's command. This is the assertion that would catch ApplyDefaults
	// filling `npx` in beside the URL, which would leave the endpoint untouched.
	got, err := r.store.Load("http-mcp")
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "" {
		t.Errorf("Command = %q, want empty for a URL template", got.Command)
	}
	cmd, args := bridgeCommand(got, "")
	if cmd != "termcp-mcp-bridge" || len(args) == 0 || args[0] != mcpURL {
		t.Fatalf("the plan does not run the relay against the URL: %q %v", cmd, args)
	}

	// And the channel is not merely up: the endpoint answers through it. This is
	// the whole claim of the endpoint form — an MCP server reached over HTTP with
	// nothing installed on any host — so the assertion is a real initialize and a
	// real tool call, driven with the same bytes an agent would write.
	sess := r.sessMgr.Get(st.SessionID)
	shell := sess.GetChildShell(st.ShellID)
	if shell == nil {
		t.Fatal("the reported shell is not in the session")
	}
	reader, err := shell.RegisterReader()
	if err != nil {
		t.Fatal(err)
	}
	defer shell.UnregisterReader(reader)

	writeLine := func(line string) {
		t.Helper()
		if err := shell.SendTerminalBytesFrom([]byte(line), true, session.InputFromAI); err != nil {
			t.Fatalf("writing %q: %v", line, err)
		}
	}
	readReply := func(id string) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			out, _, _, err := shell.ReadTerminalStream(context.Background(), reader, time.Second, true, 0, 0, nil)
			if err == nil && strings.Contains(out, id) {
				return out
			}
		}
		t.Fatalf("no reply carrying %s arrived over the relayed channel", id)
		return ""
	}

	writeLine(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"exttest","version":"1"}}}`)
	if got := readReply(`"id":1`); !strings.Contains(got, "exttest") {
		t.Fatalf("initialize reply = %q", got)
	}
	writeLine(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	writeLine(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"over-http"}}}`)
	if got := readReply(`"id":2`); !strings.Contains(got, "echo:over-http") {
		t.Fatalf("tools/call reply = %q", got)
	}
}

// ---------------------------------------------------------------------------
// the preset recipe, and when it follows the template's shape
// ---------------------------------------------------------------------------

// A template that is a stdio server when it is written and an endpoint later
// must carry the recipe that describes it now: the notes are what the agent
// drives by, and a first line saying "a stdio MCP server reached through a pipe"
// would be a lie about the thing it is driving. The author's own text is the one
// thing a preset cannot know, so it always wins.
func TestNotesFollowTheTemplateShape(t *testing.T) {
	t.Run("a preset recipe is refreshed when the shape changes", func(t *testing.T) {
		e := &Extension{Name: "x", Target: "internal", Notes: mcpNotes}
		if err := ApplyArgs(e, map[string]any{"url": "https://mcp.example.com/mcp"}, true); err != nil {
			t.Fatal(err)
		}
		if e.Notes != mcpHTTPNotes {
			t.Error("the stdio recipe should have been replaced by the endpoint one")
		}
		// And back again: the rule is about the shape, not about a direction.
		if err := ApplyArgs(e, map[string]any{"url": "", "command": "npx", "args": []any{"-y", "srv"}}, true); err != nil {
			t.Fatal(err)
		}
		if e.Notes != mcpNotes {
			t.Error("returning to a command should restore the stdio recipe")
		}
	})

	t.Run("an author's own notes are never touched", func(t *testing.T) {
		e := &Extension{Name: "x", Target: "internal", Notes: "mine, keep it"}
		if err := ApplyArgs(e, map[string]any{"url": "https://mcp.example.com/mcp"}, true); err != nil {
			t.Fatal(err)
		}
		if e.Notes != "mine, keep it" {
			t.Errorf("Notes = %q, want the author's text", e.Notes)
		}
	})

	t.Run("a call that names notes keeps what it says", func(t *testing.T) {
		// Clearing notes is a request for an unannotated template, not a request
		// for the default: an absent key is what asks for the preset, and this one
		// is present.
		e := &Extension{Name: "x", Target: "internal", Notes: mcpNotes}
		if err := ApplyArgs(e, map[string]any{"url": "https://mcp.example.com/mcp", "notes": ""}, true); err != nil {
			t.Fatal(err)
		}
		if e.Notes != "" {
			t.Errorf("Notes = %q, want it left empty", e.Notes)
		}
	})
}

// startStreamableEchoEndpoint serves a real streamable-HTTP MCP server with one
// tool, the way the bridge tests do, and returns its URL. Create it before the
// reconciler whose extension will speak to it: the relay's listening GET stream
// stays open for the session's lifetime, and Close waits on it.
func startStreamableEchoEndpoint(t *testing.T) string {
	t.Helper()
	srv := mcpserver.NewMCPServer("exttest", "0.0.1")
	srv.AddTool(
		mcpgo.NewTool("echo", mcpgo.WithString("text", mcpgo.Required())),
		func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			text, _ := req.GetArguments()["text"].(string)
			return mcpgo.NewToolResultText("echo:" + text), nil
		},
	)
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv))
	// Registered in this order so they run the other way round (t.Cleanup is
	// LIFO): the opened streams are cut first, and only then is the server closed,
	// which otherwise waits on the very connection a live session holds open.
	t.Cleanup(ts.Close)
	t.Cleanup(ts.CloseClientConnections)
	return ts.URL + "/stream"
}
