package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

type captureMCPAuditSink struct {
	mu     sync.Mutex
	events []auth.AuditEvent
}

func (s *captureMCPAuditSink) Record(_ context.Context, event auth.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *captureMCPAuditSink) snapshot() []auth.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]auth.AuditEvent(nil), s.events...)
}

func TestPolicyPreviewExplainsRunWithoutSideEffects(t *testing.T) {
	p := v2Policy("shell.input", "shell.output")
	h := newV2Harness(t, p)
	sess := h.session("internal")
	beforeSessions := len(h.manager.ListAll())
	beforeTasks := len(h.server.tasks.List())
	out := h.server.PreviewPolicy(context.Background(), p, auth.PolicyPreviewInput{
		Cmd:           "run " + sess.ID + ":1 $1",
		TermArguments: map[string]any{"args": []any{"echo should-not-run"}},
	})
	if out.Allowed || out.Executed || !strings.Contains(out.Reason, "shell.key") {
		t.Fatalf("preview did not explain the missing Enter grant: %+v", out)
	}
	if len(out.Requirements) != 3 || out.Requirements[0].Operation != "shell.input" ||
		out.Requirements[1].Operation != "shell.key" || out.Requirements[2].Operation != "shell.output" {
		t.Fatalf("preview requirements = %+v", out.Requirements)
	}
	if !out.Requirements[0].Allowed || out.Requirements[1].Allowed || !out.Requirements[2].Allowed {
		t.Fatalf("preview allowed states = %+v", out.Requirements)
	}
	if len(h.manager.ListAll()) != beforeSessions || len(h.server.tasks.List()) != beforeTasks {
		t.Fatal("preview created a session or task")
	}
}

func TestAgentPermissionDenialWritesStructuredAudit(t *testing.T) {
	h := newV2Harness(t, auth.EmptyAgentPolicy())
	sink := &captureMCPAuditSink{}
	h.account.SetAuditSink(sink)
	out := h.tool("session_list", nil)
	if !strings.Contains(out, "forbidden") {
		t.Fatalf("denial was not returned: %s", out)
	}
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %+v", events)
	}
	event := events[0]
	if event.Action != "mcp.tool.invoke" || event.TargetID != "session_list" ||
		event.Result != "deny" || event.ActorRole != auth.RoleAgent || event.Reason == "" {
		t.Fatalf("incomplete denial audit: %+v", event)
	}
	if strings.Contains(event.Reason, h.token) {
		t.Fatal("audit leaked the bearer token")
	}
}
