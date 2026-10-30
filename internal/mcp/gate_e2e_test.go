package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/approval"
)

// The three operations a review gate must cover, checked through the MCP tools
// an agent actually calls. A hole in any one of them is the bug this whole
// feature exists to prevent: review that covers the terminal but lets a file
// write or a port forward through reads as protection without being any.
func TestGatedToolsAllHoldUnderReview(t *testing.T) {
	s := newTestServer(t)
	sessionID, _ := startGatedSession(t, s, 1, time.Minute)

	cases := []struct {
		name string
		tool string
		args map[string]any
		kind approval.Kind
	}{
		{"file write", "file_write", map[string]any{
			"session_id": sessionID, "remote_path": "/tmp/gated-write.txt", "data": "x",
		}, approval.KindFileWrite},
		{"file delete", "file_delete", map[string]any{
			"session_id": sessionID, "remote_path": "/tmp/gated-del.txt",
		}, approval.KindFileDelete},
		{"file mkdir", "file_mkdir", map[string]any{
			"session_id": sessionID, "remote_path": "/tmp/gated-dir",
		}, approval.KindFileMkdir},
		{"file perm", "file_perm", map[string]any{
			"session_id": sessionID, "remote_path": "/tmp/x", "action": "chmod", "mode": float64(493),
		}, approval.KindFilePerm},
		{"truncate", "file_fs", map[string]any{
			"session_id": sessionID, "remote_path": "/tmp/x", "action": "truncate", "size": float64(0),
		}, approval.KindFileTruncate},
		{"forward", "forward", map[string]any{
			"session_id": sessionID, "action": "local", "local_port": float64(0),
			"remote_host": "localhost", "remote_port": float64(22),
		}, approval.KindForwardOpen},
	}

	approvalIDs := make([]string, 0, len(cases))
	for _, tc := range cases {
		res := dispatch(t, s, tc.tool, tc.args)
		body := parseResult(t, res)
		if body["review_pending"] != true {
			t.Errorf("%s was not held: %v", tc.name, body)
			continue
		}
		if _, has := body["pending_id"]; has {
			t.Errorf("%s handed back a pending_id", tc.name)
		}
		approvalID, _ := body["approval_id"].(string)
		if approvalID == "" {
			t.Errorf("%s did not return an approval_id: %v", tc.name, body)
		} else {
			approvalIDs = append(approvalIDs, approvalID)
		}
		if body["session_id"] != sessionID {
			t.Errorf("%s session_id = %v, want %q", tc.name, body["session_id"], sessionID)
		}
	}

	q := s.sessMgr.Get(sessionID).ApprovalQueue()
	reqs := q.List()
	if len(reqs) != len(cases) {
		t.Fatalf("queued %d requests, want %d", len(reqs), len(cases))
	}
	queuedIDs := make(map[string]bool, len(reqs))
	for _, request := range reqs {
		queuedIDs[request.ID] = true
	}
	for _, approvalID := range approvalIDs {
		if !queuedIDs[approvalID] {
			t.Errorf("returned approval_id %q is not in the queue", approvalID)
		}
	}
	// Every request must say what it will do and carry a replayable payload:
	// a request a reviewer cannot read, or an executor cannot replay, is a gate
	// that stops work without protecting anything.
	for _, r := range reqs {
		if strings.TrimSpace(r.Summary) == "" {
			t.Errorf("%s request has no summary", r.Kind)
		}
		if len(r.Payload) == 0 {
			t.Errorf("%s request has no payload to replay", r.Kind)
		}
		if r.Source != "mcp" {
			t.Errorf("%s request source = %q, want mcp", r.Kind, r.Source)
		}
	}

	// Reads must not be gated: approving a directory listing trains a reviewer to
	// click without reading.
	before := len(q.List())
	// Called through the same entry an agent uses: the reads are absent from the
	// review gate, which is the point.
	if _, err := s.call(context.Background(), "file_read", map[string]any{"session_id": sessionID, "remote_path": "/etc/hostname"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.call(context.Background(), "file_stat", map[string]any{"session_id": sessionID, "remote_path": "/etc/hostname"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.call(context.Background(), "file_getwd", map[string]any{"session_id": sessionID}); err != nil {
		t.Fatal(err)
	}
	if after := len(q.List()); after != before {
		t.Errorf("a read-only operation was queued for review: %d -> %d", before, after)
	}
	// forward list is a read too.
	_ = dispatch(t, s, "forward", map[string]any{"session_id": sessionID, "action": "list"})
	if after := len(q.List()); after != before {
		t.Errorf("listing forwards was queued for review: %d -> %d", before, after)
	}
}

// dispatch calls the tool the way the MCP server would, admission included.
func dispatch(t *testing.T, s *Server, tool string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	res, err := s.call(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}
