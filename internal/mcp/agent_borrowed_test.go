package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestAgentBorrowedRunWithAllHosts(t *testing.T) {
	p := v2Policy("shell.input", "shell.key", "shell.output", "session.list", "shell.list")
	p.HostMode, p.Hosts = "all", nil
	h := newV2Harness(t, p)
	// An operator's existing borrowed container must not be reused by an Agent.
	operator := h.server.runWithArgs("run use:internal $1", "echo operator-borrowed")
	if len(operator.slots) != 1 || operator.slots[0].Code != 200 || !strings.Contains(operator.slots[0].Body, "operator-borrowed") {
		t.Fatal(operator.slots)
	}
	cli := h.client("stream")
	for _, target := range []string{"use:internal", "internal", "`use internal`"} {
		out := v2Term(t, cli, "run "+target+" $1", "echo agent-borrowed-ok")
		_, body, _ := strings.Cut(out, "\n\n")
		if strings.HasPrefix(out, "TERM/1 4") || strings.HasPrefix(out, "TERM/1 5") || !strings.Contains(body, "agent-borrowed-ok") {
			t.Fatalf("all-host Agent could not run on %s: %s", target, out)
		}
	}
	caller := ns.ForCaller("agent-" + h.agentID)
	owned := 0
	for _, info := range h.manager.ListAll() {
		if info.Borrowed && info.Namespace == caller {
			owned++
		}
	}
	if owned != 1 {
		t.Fatalf("Agent has %d borrowed containers, want one separate from the operator", owned)
	}
	var created struct {
		Token     string `json:"token"`
		Principal struct {
			ID string `json:"id"`
		} `json:"principal"`
	}
	h.adminRequest("POST", "/auth/principals", map[string]any{"name": "other-borrowed-agent", "group_id": h.groupID, "issue_token": true}, &created)
	other := *h
	other.token, other.agentID = created.Token, created.Principal.ID
	otherClient := other.client("stream")
	out := v2Term(t, otherClient, "run use:internal $1", "echo other-agent-ok")
	_, body, _ := strings.Cut(out, "\n\n")
	if !strings.Contains(body, "other-agent-ok") {
		t.Fatal(out)
	}
	otherNS := ns.ForCaller("agent-" + other.agentID)
	otherOwned := 0
	for _, info := range h.manager.ListAll() {
		if info.Borrowed && info.Namespace == otherNS {
			otherOwned++
		}
	}
	if otherOwned != 1 {
		t.Fatalf("second Agent has %d containers, want its own", otherOwned)
	}
}

func TestAgentBorrowedHostScopeAndBinding(t *testing.T) {
	s := newTestServer(t)
	s.sshConfigs = sshconfig.NewStore(t.TempDir())
	p := v2Policy("shell.input", "file.read")
	path := filepath.Join(t.TempDir(), "synthetic.txt")
	for _, tool := range []string{"shell_input", "file_read"} {
		key := "shell_id"
		if tool == "file_read" {
			key = "session_id"
		}
		request := toolRequest(tool, argsOf(key, "use:internal", "remote_path", path))
		if _, bad, _ := s.agentToolCheckV2(context.Background(), p, tool, request); bad != nil {
			t.Fatal(toolResultText(bad))
		}
		denied := p
		denied.HostMode, denied.Hosts = "none", nil
		if _, bad, _ := s.agentToolCheckV2(context.Background(), denied, tool, request); bad == nil {
			t.Fatal("borrowed host bypassed host scope", tool)
		}
	}
	profile := "borrowed-binding"
	first := []byte("kind = \"remote\"\nhost = \"127.0.0.1\"\nport = 1\nuser = \"synthetic\"\npassword = \"synthetic-password\"\n")
	if err := s.sshConfigs.Save(profile, first); err != nil {
		t.Fatal(err)
	}
	p.HostMode, p.Hosts = "all", nil
	ctx, bad, _ := s.agentToolCheckV2(context.Background(), p, "shell_input", toolRequest("shell_input", argsOf("shell_id", "use:"+profile)))
	if bad != nil {
		t.Fatal(toolResultText(bad))
	}
	changed := []byte(strings.Replace(string(first), "port = 1", "port = 2", 1))
	if err := s.sshConfigs.Save(profile, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureBorrowedContainer(ctx, profile); !errors.Is(err, sshconfig.ErrAuthorizationBindingChanged) {
		t.Fatalf("run did not reject changed host binding: %v", err)
	}
	if _, _, err := s.acquireSSHTransport(ctx, "use:"+profile); !errors.Is(err, sshconfig.ErrAuthorizationBindingChanged) {
		t.Fatalf("file operation did not reject changed host binding: %v", err)
	}
	if len(s.sessMgr.ListAll()) != 0 {
		t.Fatal("changed binding created a session")
	}
	if err := s.sshConfigs.Save(profile, first); err != nil {
		t.Fatal(err)
	}
	ctx, bad, _ = s.agentToolCheckV2(context.Background(), p, "shell_input", toolRequest("shell_input", argsOf("shell_id", "use:"+profile)))
	if bad != nil {
		t.Fatal(toolResultText(bad))
	}
	bindings, _ := agentBindingsFromContext(ctx)
	expectedID := bindings.profiles[profile].Identity
	if err := s.sshConfigs.Rename(profile, "renamed-borrowed"); err != nil {
		t.Fatal(err)
	}
	stopBeforeDial := errors.New("test stops before dialing")
	resolved := false
	s.sessMgr.SetSSHConfigResolver(func(name, identity string) (string, string, error) {
		resolved = true
		if name != "renamed-borrowed" || identity != expectedID {
			t.Errorf("rename lost authorized identity: name=%q identity=%q", name, identity)
		}
		return "", "", stopBeforeDial
	})
	if _, err := s.ensureBorrowedContainer(ctx, profile); !errors.Is(err, stopBeforeDial) || !resolved {
		t.Fatalf("renamed binding did not reach identity validation: %v", err)
	}
}

func TestAgentBorrowedRunRequiresCompletePermissionsBeforeExecution(t *testing.T) {
	for _, missing := range []string{"shell.input", "shell.key", "shell.output"} {
		t.Run(missing, func(t *testing.T) {
			p := v2Policy("shell.input", "shell.key", "shell.output")
			delete(p.Actions, missing)
			h := newV2Harness(t, p)
			out := v2Term(t, h.client("stream"), "run use:internal $1", "echo must-not-run")
			if !strings.HasPrefix(out, "TERM/1 403") {
				t.Fatal("incomplete permission ran command", out)
			}
			if len(h.manager.ListAll()) != 0 {
				t.Fatal("denied operation created a borrowed container")
			}
		})
	}
}
