package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
)

func policyReviewBody(t *testing.T, out string) map[string]any {
	t.Helper()
	var result mcpgo.CallToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return parseResult(t, &result)
}

func TestPolicyAskQueuesAndExecutesAfterApproval(t *testing.T) {
	limit := int64(32)
	p := v2Policy("file.write")
	p.Actions["file.write"] = authorization.Grant{
		Effect: authorization.EffectAsk,
		Constraints: &authorization.Constraints{
			Forms: []string{"inline"}, MaxBytes: &limit,
		},
	}
	h := newV2Harness(t, p)
	sess := h.session("internal")
	target := filepath.Join(t.TempDir(), "approval-write.txt")
	out := h.tool("file_write", argsOf(
		"session_id", sess.ID, "remote_path", target, "data", "approved content",
	))
	if !strings.Contains(out, "review_pending") {
		t.Fatalf("ask did not queue: %s", out)
	}
	body := policyReviewBody(t, out)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ask wrote before approval: %v", err)
	}
	pending := h.server.ListPolicyApprovals()
	if len(pending) != 1 || pending[0].State != approval.Pending || pending[0].Summary == "" {
		t.Fatalf("pending approvals = %+v", pending)
	}
	if body["approval_id"] != pending[0].ID || body["session_id"] != sess.ID {
		t.Fatalf("review correlation = %#v, pending = %+v", body, pending)
	}
	if _, err := h.server.DecidePolicyApproval(pending[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "approved content" {
		t.Fatalf("approved write = %q, %v", data, err)
	}
}

func TestApprovedPolicyAskIsReauthorizedAgainstCurrentCredential(t *testing.T) {
	p := v2Policy("session.list")
	p.Actions["session.list"] = authorization.Grant{Effect: authorization.EffectAsk}
	h := newV2Harness(t, p)
	if out := h.tool("session_list", nil); !strings.Contains(out, "review_pending") {
		t.Fatalf("read ask did not queue: %s", out)
	}
	pending := h.server.ListPolicyApprovals()
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %+v", pending)
	}
	h.adminRequest("PATCH", "/auth/principals/"+h.agentID, map[string]any{
		"enabled": false, "revision": 1,
	}, nil)
	if _, err := h.server.DecidePolicyApproval(pending[0].ID, true, ""); err == nil {
		t.Fatal("approval executed after its principal was disabled")
	}
}

func TestPolicyPreviewReportsAskWithoutQueueing(t *testing.T) {
	p := v2Policy("file.write")
	p.Actions["file.write"] = authorization.Grant{Effect: authorization.EffectAsk}
	h := newV2Harness(t, p)
	out := h.server.PreviewPolicy(t.Context(), p, auth.PolicyPreviewInput{
		Tool: "file_write", Arguments: map[string]any{},
	})
	if !out.Allowed || !out.ApprovalRequired {
		t.Fatalf("preview ask = %+v", out)
	}
	if len(h.server.ListPolicyApprovals()) != 0 {
		t.Fatal("preview queued a real approval")
	}
}

func TestDSLAskQueuesWholeStatementAndExecutesAfterApproval(t *testing.T) {
	p := v2Policy("shell.input", "shell.output", "shell.key")
	p.Actions["shell.key"] = authorization.Grant{Effect: authorization.EffectAsk}
	h := newV2Harness(t, p)
	sess := h.session("internal")
	client := h.client("stream")
	marker := filepath.Join(t.TempDir(), "approved-dsl.txt")
	out := v2Term(t, client, "run "+sess.ID+":1 $1", "printf approved > "+marker)
	if !strings.Contains(out, "review_pending") {
		t.Fatalf("DSL ask did not queue: %s", out)
	}
	parts := strings.SplitN(out, "\n\n", 2)
	if len(parts) != 2 {
		t.Fatalf("DSL review reply has no body: %q", out)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(parts[1]), &body); err != nil {
		t.Fatalf("decode DSL review reply: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("DSL ran before approval: %v", err)
	}
	pending := h.server.ListPolicyApprovals()
	if len(pending) != 1 || pending[0].Kind != approval.Kind("shell.key") {
		t.Fatalf("DSL pending approvals = %+v", pending)
	}
	if body["approval_id"] != pending[0].ID || body["session_id"] != sess.ID {
		t.Fatalf("DSL review correlation = %#v, pending = %+v", body, pending)
	}
	if _, err := h.server.DecidePolicyApproval(pending[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	var (
		data []byte
		err  error
	)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(marker)
		if err == nil && string(data) == "approved" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("approved DSL statement = %q, %v", data, err)
}
