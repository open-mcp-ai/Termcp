package mcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/open-mcp-ai/termcp/internal/approval"
)

// SetApprovalNotifier connects policy-level approvals to the Web UI's existing
// approval card and notification channel.
func (s *Server) SetApprovalNotifier(fn func(string, approval.Request)) {
	s.policyApprovalMu.Lock()
	s.approvalNotify = fn
	s.policyApprovalMu.Unlock()
}

// SetApprovalContextProvider reconstructs the original verified Agent identity
// and current policy when a human approves a queued operation.
func (s *Server) SetApprovalContextProvider(fn func(context.Context, string, string) (context.Context, error)) {
	s.policyApprovalMu.Lock()
	s.approvalContext = fn
	s.policyApprovalMu.Unlock()
}

func (s *Server) policyApprovalQueue(sessionID string) *approval.Queue {
	s.policyApprovalMu.Lock()
	defer s.policyApprovalMu.Unlock()
	if s.policyApprovals == nil {
		s.policyApprovals = map[string]*approval.Queue{}
	}
	if q := s.policyApprovals[sessionID]; q != nil {
		return q
	}
	q := approval.NewQueue(sessionID, approval.Options{
		Need: 1,
		OnChange: func(req approval.Request) {
			s.policyApprovalMu.Lock()
			notify := s.approvalNotify
			s.policyApprovalMu.Unlock()
			if notify != nil {
				notify(sessionID, req)
			}
		},
	})
	s.policyApprovals[sessionID] = q
	return q
}

func (s *Server) submitPolicyApproval(ctx context.Context, sessionID string, payload []byte, kind approval.Kind, summary string) (string, error) {
	if sessionID == "" {
		sessionID = "policy"
	}
	return s.policyApprovalQueue(sessionID).Submit(approval.Submission{
		Kind: kind, Source: callerSource(ctx), Summary: summary, Payload: payload,
	})
}

// policyApprovalCorrelation describes the queued request for the caller. The id
// returned by Submit is enough to find the request, while reading its snapshot
// supplies the session and optional expiry the common review reply carries.
func (s *Server) policyApprovalCorrelation(sessionID, id string) reviewPendingFields {
	corr := reviewPendingFields{ApprovalID: id, SessionID: sessionID}
	if id == "" {
		return corr
	}
	if q := s.policyApprovalFor(id); q != nil {
		if req, err := q.Get(id); err == nil {
			corr.SessionID = req.SessionID
			corr.ExpiresAt = req.ExpiresAt
		}
	}
	return corr
}

// ListPolicyApprovals exposes policy asks through the existing WebUI approval
// worklist without requiring session review mode to be enabled.
func (s *Server) ListPolicyApprovals() []approval.Request {
	s.policyApprovalMu.Lock()
	queues := make([]*approval.Queue, 0, len(s.policyApprovals))
	for _, q := range s.policyApprovals {
		queues = append(queues, q)
	}
	s.policyApprovalMu.Unlock()
	out := []approval.Request{}
	for _, q := range queues {
		out = append(out, q.List()...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *Server) policyApprovalFor(id string) *approval.Queue {
	s.policyApprovalMu.Lock()
	defer s.policyApprovalMu.Unlock()
	for _, q := range s.policyApprovals {
		if _, err := q.Get(id); err == nil {
			return q
		}
	}
	return nil
}

// DecidePolicyApproval resolves one ask. Approval executes immediately through
// the common tool admission path; rejection performs no side effect.
func (s *Server) DecidePolicyApproval(id string, approve bool, reason string) (approval.Request, error) {
	q := s.policyApprovalFor(id)
	if q == nil {
		return approval.Request{}, approval.ErrNotFound
	}
	var (
		req approval.Request
		err error
	)
	if approve {
		req, err = q.Approve(id)
	} else {
		req, err = q.Reject(id, reason)
	}
	if err != nil {
		return req, err
	}
	if approve {
		if err := s.ExecuteApprovedOperation(req); err != nil {
			return req, fmt.Errorf("approved but execution failed: %w", err)
		}
	}
	return req, nil
}

func (s *Server) closePolicyApprovals() {
	s.policyApprovalMu.Lock()
	queues := make([]*approval.Queue, 0, len(s.policyApprovals))
	for _, q := range s.policyApprovals {
		queues = append(queues, q)
	}
	s.policyApprovals = map[string]*approval.Queue{}
	s.policyApprovalMu.Unlock()
	for _, q := range queues {
		q.Close("server stopped")
	}
}
