package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/task"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

type v2Harness struct {
	t       *testing.T
	server  *Server
	manager *session.Manager
	account *auth.Managed
	vault   *sshconfig.Store
	url     string
	admin   *http.Client
	csrf    string
	token   string
	agentID string
	groupID string
}

func newV2Harness(t *testing.T, p auth.AgentPolicy) *v2Harness {
	t.Helper()
	dir := t.TempDir()
	vault, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vault.Close() })
	account, err := auth.OpenManaged(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	s, _, mgr, _ := newTestServerWithHistory(t)
	s.sshConfigs = vault
	mgr.SetSSHConfigResolver(vault.ResolveSessionReference)
	mux := http.NewServeMux()
	mux.Handle("/stream", s.StreamableHTTPHandler())
	mux.Handle("/sse", s.SSEHandler())
	mux.Handle("/message", s.MessageHandler())
	ts := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	h := &v2Harness{t: t, server: s, manager: mgr, account: account, vault: vault, url: ts.URL, admin: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	var status struct {
		CSRF string `json:"csrf"`
	}
	h.adminRequest("GET", "/auth/status", nil, &status)
	h.csrf = status.CSRF
	h.adminRequest("POST", "/auth/setup", map[string]any{"username": "test-admin", "password": "synthetic-v2-password"}, &status)
	h.csrf = status.CSRF
	var created struct {
		Token string `json:"token"`
		Agent struct {
			ID string `json:"id"`
		} `json:"principal"`
	}
	var group struct {
		Group struct {
			ID string `json:"id"`
		} `json:"group"`
	}
	h.adminRequest("POST", "/auth/groups", map[string]any{"name": "limited-policy", "policy": p}, &group)
	h.adminRequest("POST", "/auth/principals", map[string]any{"name": "limited", "group_id": group.Group.ID, "issue_token": true}, &created)
	h.token, h.agentID, h.groupID = created.Token, created.Agent.ID, group.Group.ID
	return h
}
func (h *v2Harness) adminRequest(method, path string, body, out any) {
	h.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r, _ := http.NewRequest(method, h.url+path, bytes.NewReader(raw))
	r.Header.Set("X-Termcp-CSRF", h.csrf)
	resp, err := h.admin.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var data any
		_ = json.NewDecoder(resp.Body).Decode(&data)
		h.t.Fatalf("admin %s: %d %v", path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatal(err)
		}
	}
}
func (h *v2Harness) tool(name string, args map[string]any) string {
	h.t.Helper()
	handler := h.account.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result, err := h.server.call(withRequestContext(r.Context(), r), name, args)
		if err != nil {
			h.t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	r := httptest.NewRequest("POST", h.url+"/stream", nil)
	r.Header.Set("Authorization", "Bearer "+h.token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		h.t.Fatalf("tool admission %d: %s", w.Code, w.Body)
	}
	var result mcpgo.CallToolResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		h.t.Fatal(err)
	}
	// mcp-go content has interface fields; this also covers structured results.
	return w.Body.String()
}
func (h *v2Harness) session(profile string) *session.Session {
	h.t.Helper()
	sess, err := h.manager.Create(session.Config{Command: testShell(), Mode: api.ModePipe, SSHConfig: profile, Namespace: ns.ForCaller("agent-" + h.agentID)})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = h.manager.Delete(sess.ID) })
	return sess
}
func (h *v2Harness) client(kind string) *client.Client {
	h.t.Helper()
	var cli *client.Client
	var err error
	if kind == "sse" {
		cli, err = client.NewSSEMCPClient(h.url+"/sse", transport.WithHeaders(map[string]string{"Authorization": "Bearer " + h.token}))
	} else {
		cli, err = client.NewStreamableHttpClient(h.url+"/stream", transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + h.token}))
	}
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	h.t.Cleanup(cancel)
	if err = cli.Start(ctx); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = cli.Close() })
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "permissions-test", Version: "1"}
	if _, err = cli.Initialize(ctx, init); err != nil {
		h.t.Fatal(err)
	}
	return cli
}
func v2Policy(ids ...string) auth.AgentPolicy {
	p := auth.AgentPolicy{SchemaVersion: 2, HostMode: "selected", Hosts: []string{sshconfig.InternalIdentity}, Actions: map[string]authorization.Grant{}}
	for _, id := range ids {
		p.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow}
	}
	return p
}
func v2Term(t *testing.T, cli *client.Client, cmd string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	params := map[string]any{"cmd": cmd, "timeout": 0}
	if len(args) > 0 {
		params["args"] = args
	}
	res, err := cli.CallTool(ctx, toolRequest("term", params))
	if err != nil {
		t.Fatal(err)
	}
	return toolResultText(res)
}

func TestAgentV2RealTransportsAndEquivalentFileBranches(t *testing.T) {
	for _, kind := range []string{"stream", "sse"} {
		t.Run(kind, func(t *testing.T) {
			p := v2Policy("file.realpath", "file.write", "shell.input", "shell.output", "shell.key", "session.list", "profile.get", "task.info")
			limit := int64(4)
			p.Actions["file.write"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}, MaxBytes: &limit}}
			repeat := 1
			p.Actions["shell.key"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Keys: []string{"tab"}, RepeatMax: &repeat}}
			h := newV2Harness(t, p)
			sess := h.session("internal")
			cli := h.client(kind)
			path := filepath.Join(t.TempDir(), "sample.txt")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, cmd := range []string{"get " + sess.ID + " file $1 --op realpath", "write " + sess.ID + " file $1 --op realpath"} {
				if out := v2Term(t, cli, cmd, path); strings.HasPrefix(out, "TERM/1 4") || strings.HasPrefix(out, "TERM/1 5") {
					t.Fatalf("permitted realpath refused: %s", out)
				}
			}
			if out := h.tool("file_fs", argsOf("session_id", sess.ID, "remote_path", path, "action", "realpath")); strings.Contains(out, "forbidden") {
				t.Fatal("specialized realpath refused", out)
			}
			for _, cmd := range []string{"get " + sess.ID + " file $1 --op truncate", "write " + sess.ID + " file $1 --op truncate --size 0", "write " + sess.ID + " file $1 --mode 0", "rm " + sess.ID + " file $1"} {
				if out := v2Term(t, cli, cmd, path); !strings.HasPrefix(out, "TERM/1 403") {
					t.Fatalf("ungranted file mutation passed: %s", out)
				}
			}
			if out := h.tool("file_fs", argsOf("session_id", sess.ID, "remote_path", path, "action", "truncate", "size", float64(0))); !strings.Contains(out, "forbidden") {
				t.Fatal("specialized truncate bypassed permission", out)
			}
			if out := v2Term(t, cli, "write "+sess.ID+" file $1 --data 12345", path); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("oversized write passed", out)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
				t.Fatal("denied operation changed file", err)
			}
			if out := v2Term(t, cli, "write "+sess.ID+" file $1 --data hex:00ff", path); strings.HasPrefix(out, "TERM/1 4") || strings.HasPrefix(out, "TERM/1 5") {
				t.Fatal("permitted sized write failed", out)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, []byte{0, 255}) {
				t.Fatal("write did not use decoded byte count", err)
			}
			marker := filepath.Join(t.TempDir(), "denied.txt")
			// The embedded newline would execute immediately if shell_input ran before
			// checking the missing Enter grant. Drain the same pipe as an operator below.
			payload := "echo forbidden > \"" + marker + "\"\n"
			if out := v2Term(t, cli, "run "+sess.ID+":1 $1", payload); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("run bypassed key constraints", out)
			}
			termText(t, h.server, map[string]any{"cmd": "run " + sess.ID + ":1 echo drained", "timeout": 1})
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("run typed before composite authorization", err)
			}
			if out := v2Term(t, cli, "ls"); !strings.Contains(out, sess.ID) || strings.Contains(out, sess.ID+":1") {
				t.Fatal("session listing leaked ungranted channel data", out)
			}
			if out := v2Term(t, cli, "reader "+sess.ID+":1"); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("output permission allowed reader registration", out)
			}
			aliases.set("v2_forbidden_alias", "rm "+sess.ID+" --force")
			t.Cleanup(func() { aliases.drop("v2_forbidden_alias") })
			if out := v2Term(t, cli, "v2_forbidden_alias"); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("alias bypassed actual operation permission", out)
			}
			if err := h.vault.Save("outside", []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"test\"\npassword = \"synthetic\"\n")); err != nil {
				t.Fatal(err)
			}
			other := h.session("outside")
			if out := h.tool("file_fs", argsOf("session_id", other.ID, "remote_path", path, "action", "realpath")); !strings.Contains(out, "forbidden") {
				t.Fatal("action grant bypassed host scope", out)
			}
			if out := v2Term(t, cli, "get host outside"); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("profile summary bypassed host scope", out)
			}
			// A task whose destination is allowed still needs the source's host. Even
			// with the live source gone, the captured refs must govern the receipt.
			job := h.server.tasks.Start(task.Copy, sess.ID, path, "display-only", func() {}, h.server.taskResource(other.ID, "read"), h.server.taskResource(sess.ID, "write"))
			t.Cleanup(func() { h.server.tasks.Finish(job.ID(), nil) })
			_ = h.manager.Delete(other.ID)
			if out := v2Term(t, cli, "get task "+job.ID()); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("task ignored source host", out)
			}
		})
	}
}

func TestAgentV2SemanticPlansAndProtocolAllowlists(t *testing.T) {
	p := v2Policy("session.start", "shell.open", "file.realpath", "mcp.tool.invoke", "mcp.raw", "cdp.method.invoke", "jrpc.method.invoke")
	p.Actions["mcp.tool.invoke"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{ToolNames: []string{"status"}}}
	p.Actions["cdp.method.invoke"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Methods: []string{"Page.reload"}}}
	p.Actions["jrpc.method.invoke"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Methods: []string{"status"}}}
	for _, tc := range []struct {
		tool  string
		args  map[string]any
		allow bool
	}{
		{"session_start", argsOf("ssh_config", "internal"), true},
		{"session_start", argsOf("ssh_config", "internal", "command", "echo"), false},
		{"shell_open", argsOf("command", "echo"), false},
		{"file_fs", argsOf("action", "realpath"), true},
		{"file_fs", argsOf("action", "truncate"), false},
		{"file_fs", argsOf("action", "new-action"), false},
		{"unregistered_tool", nil, false},
	} {
		plan, err := capability.AgentToolPlan(tc.tool, tc.args)
		if err == nil {
			err = checkAgentPlan(p, plan)
		}
		if (err == nil) != tc.allow {
			t.Fatalf("classification %s %v: %v", tc.tool, tc.args, err)
		}
	}
	s := &Server{}
	for _, tc := range []struct {
		kind, payload string
		allow         bool
	}{
		{"mcp", `status {}`, true},
		{"mcp", `shell {}`, false},
		{"mcp", `{"method":"tools/call","params":{"name":"status"}}`, true},
		{"mcp", `{"method":"tools/call","params":{"name":"shell"}}`, false},
		{"mcp", `[{"method":"tools/call","params":{"name":"status"}},{"method":"tools/call","params":{"name":"shell"}}]`, false},
		{"cdp", `{"method":"Page.reload"}`, true},
		{"cdp", `{"method":"Runtime.evaluate"}`, false},
		{"cdp", `{"method":"Target.attachToTarget"}`, false},
		{"jrpc", `{"method":"status"}`, true},
		{"jrpc", `{"method":"execute"}`, false},
	} {
		stmt := directive.Statement{Op: "call", Kind: tc.kind, Payload: tc.payload}
		plan, err := s.agentPeerPlan(stmt)
		if err == nil {
			err = checkAgentPlan(p, plan)
		}
		if (err == nil) != tc.allow {
			t.Fatalf("%s %s: %v", tc.kind, tc.payload, err)
		}
	}
}

func TestAgentV2ConnectRequiresAllOperationsBeforeCreating(t *testing.T) {
	for _, ids := range [][]string{{"session.start"}, {"session.start", "shell.open"}} {
		t.Run(fmt.Sprint(len(ids)), func(t *testing.T) {
			h := newV2Harness(t, v2Policy(ids...))
			args := argsOf("ssh_config", "internal", "mode", "pipe", "command", testShell())
			if out := h.tool("session_start", args); !strings.Contains(out, "forbidden") {
				t.Fatal("explicit program bypassed composite permissions", out)
			}
			if list := h.manager.ListAll(); len(list) != 0 {
				t.Fatal("refused connection created a session")
			}
		})
	}
}

func TestAgentV2ProtocolSendsCheckActualToolAndStdioPlan(t *testing.T) {
	for _, kind := range []string{"stream", "sse"} {
		t.Run(kind, func(t *testing.T) {
			p := v2Policy("mcp.tool.invoke", "mcp.raw", "shell.input", "shell.output", "shell.key")
			p.Actions["mcp.tool.invoke"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{ToolNames: []string{"status"}}}
			n := 1
			p.Actions["shell.key"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Keys: []string{"tab"}, RepeatMax: &n}}
			h := newV2Harness(t, p)
			sess := h.session("internal")
			cli := h.client(kind)
			calls := make(chan string, 20)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					w.WriteHeader(405)
					return
				}
				var frame map[string]any
				if err := json.NewDecoder(r.Body).Decode(&frame); err != nil {
					http.Error(w, "invalid frame", 400)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				result := map[string]any{}
				switch frame["method"] {
				case "initialize":
					result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "permissions-peer", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
				case "tools/call":
					params, _ := frame["params"].(map[string]any)
					name, _ := params["name"].(string)
					calls <- name
					result = map[string]any{"content": []map[string]any{{"type": "text", "text": "status-ok"}}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": frame["id"], "result": result})
			}))
			t.Cleanup(peer.Close)
			addr := handleFrom(t, termText(t, h.server, map[string]any{"cmd": "open " + sess.ID + " mcp " + peer.URL}))
			t.Cleanup(func() { closePeersBySession(sess.ID) })
			for _, payload := range []string{`forbidden {}`, `'{"method":"tools/call","params":{"name":"forbidden"}}'`, `--send '{"method":"tools/call","params":{"name":"forbidden"}}'`, `'[{"method":"tools/call","params":{"name":"status"}},{"method":"tools/call","params":{"name":"forbidden"}}]'`} {
				if out := v2Term(t, cli, "call "+addr+" "+payload); !strings.HasPrefix(out, "TERM/1 403") {
					t.Fatal("protocol encoding bypassed tool allowlist", out)
				}
			}
			select {
			case called := <-calls:
				t.Fatal("refused frame reached the peer", called)
			default:
			}
			for _, payload := range []string{`status {}`, `'{"method":"tools/call","params":{"name":"status"}}'`} {
				if out := v2Term(t, cli, "call "+addr+" "+payload); !strings.Contains(out, "status-ok") {
					t.Fatal("allowed tool failed", out)
				}
				select {
				case called := <-calls:
					if called != "status" {
						t.Fatal("sent a different tool", called)
					}
				default:
					t.Fatal("allowed invocation did not reach peer")
				}
			}
			// Stdio has the same protocol permission, plus a composite terminal plan.
			// Missing Enter must refuse before even a partial JSON line is written.
			if out := v2Term(t, cli, "call "+sess.ID+`:1 '{"method":"tools/call","params":{"name":"status"}}'`); !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("stdio sent before checking Enter", out)
			}
			out := termText(t, h.server, map[string]any{"cmd": "run " + sess.ID + ":1 echo clean-pipe", "timeout": 1})
			if !strings.Contains(out, "clean-pipe") || strings.Contains(out, "tools/call") {
				t.Fatal("stdio authorization left a partial line", out)
			}
		})
	}
}
