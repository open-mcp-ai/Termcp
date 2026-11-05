package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// AuditEvent is the stable, secret-free record of an access-management or
// authorization decision.
type AuditEvent struct {
	Action           string `json:"action"`
	ActorID          string `json:"actor_id"`
	ActorName        string `json:"actor_name,omitempty"`
	ActorRole        Role   `json:"actor_role"`
	TargetType       string `json:"target_type,omitempty"`
	TargetID         string `json:"target_id,omitempty"`
	RequestMethod    string `json:"request_method,omitempty"`
	RequestPath      string `json:"request_path,omitempty"`
	BeforeRevision   uint64 `json:"before_revision,omitempty"`
	AfterRevision    uint64 `json:"after_revision,omitempty"`
	AffectedSubjects int    `json:"affected_subjects,omitempty"`
	Result           string `json:"result"`
	Reason           string `json:"reason,omitempty"`
}

// AuditSink receives structured events synchronously. Implementations must not
// write request bodies or credential material.
type AuditSink interface {
	Record(context.Context, AuditEvent)
}

type SlogAuditSink struct{}

func (SlogAuditSink) Record(ctx context.Context, event AuditEvent) {
	attrs := []any{
		"action", event.Action,
		"actor_id", event.ActorID,
		"actor_role", event.ActorRole,
		"result", event.Result,
	}
	if event.ActorName != "" {
		attrs = append(attrs, "actor_name", event.ActorName)
	}
	if event.TargetType != "" {
		attrs = append(attrs, "target_type", event.TargetType)
	}
	if event.TargetID != "" {
		attrs = append(attrs, "target_id", event.TargetID)
	}
	if event.RequestMethod != "" {
		attrs = append(attrs, "request_method", event.RequestMethod)
	}
	if event.RequestPath != "" {
		attrs = append(attrs, "request_path", event.RequestPath)
	}
	if event.BeforeRevision != 0 {
		attrs = append(attrs, "before_revision", event.BeforeRevision)
	}
	if event.AfterRevision != 0 {
		attrs = append(attrs, "after_revision", event.AfterRevision)
	}
	if event.AffectedSubjects != 0 {
		attrs = append(attrs, "affected_subjects", event.AffectedSubjects)
	}
	if event.Reason != "" {
		attrs = append(attrs, "reason", event.Reason)
	}
	if event.Result == "deny" || event.Result == "error" {
		slog.WarnContext(ctx, "permission audit", attrs...)
		return
	}
	slog.InfoContext(ctx, "permission audit", attrs...)
}

type auditSinkKey struct{}
type auditDraftKey struct{}
type auditDisabled struct{}

type auditDraft struct {
	action           string
	targetType       string
	targetID         string
	beforeRevision   uint64
	afterRevision    uint64
	affectedSubjects int
}

func withAuditSink(ctx context.Context, sink AuditSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, auditSinkKey{}, sink)
}

// WithoutAudit suppresses decision audit records for non-executing simulations
// such as /auth/access/preview.
func WithoutAudit(ctx context.Context) context.Context {
	return context.WithValue(ctx, auditSinkKey{}, auditDisabled{})
}

// RecordAudit is used by authorization adapters outside package auth. It only
// records when the request came through Managed.Wrap and therefore carries a
// trusted, verified Actor.
func RecordAudit(ctx context.Context, event AuditEvent) {
	sink, _ := ctx.Value(auditSinkKey{}).(AuditSink)
	if sink == nil {
		return
	}
	if actor, ok := ActorFromContext(ctx); ok {
		if event.ActorID == "" {
			event.ActorID = actor.ID
		}
		if event.ActorName == "" {
			event.ActorName = actor.Username
		}
		if event.ActorRole == "" {
			event.ActorRole = actor.Role
		}
	}
	if event.Result == "" {
		event.Result = "success"
	}
	sink.Record(ctx, event)
}

func (m *Managed) SetAuditSink(sink AuditSink) {
	m.mu.Lock()
	m.auditSink = sink
	m.mu.Unlock()
}

func (m *Managed) audited(w http.ResponseWriter, r *http.Request, action string, next func(http.ResponseWriter, *http.Request)) {
	draft := &auditDraft{action: action, targetType: auditTargetType(r.URL.Path), targetID: auditTargetID(r.URL.Path)}
	r = r.WithContext(context.WithValue(r.Context(), auditDraftKey{}, draft))
	recorder := &auditResponseWriter{ResponseWriter: w}
	next(recorder, r)
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	event := AuditEvent{
		Action:           draft.action,
		TargetType:       draft.targetType,
		TargetID:         draft.targetID,
		RequestMethod:    r.Method,
		RequestPath:      r.URL.Path,
		BeforeRevision:   draft.beforeRevision,
		AfterRevision:    draft.afterRevision,
		AffectedSubjects: draft.affectedSubjects,
	}
	switch {
	case status >= 200 && status < 300:
		event.Result = "success"
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		event.Result = "deny"
	default:
		event.Result = "error"
	}
	if status >= 400 {
		event.Reason = strings.TrimSpace(string(recorder.body))
	}
	RecordAudit(r.Context(), event)
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(w.body) < 512 {
		remaining := 512 - len(w.body)
		if remaining > len(data) {
			remaining = len(data)
		}
		w.body = append(w.body, data[:remaining]...)
	}
	return w.ResponseWriter.Write(data)
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func auditDetail(ctx context.Context, mutate func(*auditDraft)) {
	if draft, ok := ctx.Value(auditDraftKey{}).(*auditDraft); ok {
		mutate(draft)
	}
}

func auditTargetType(path string) string {
	switch {
	case strings.HasPrefix(path, "/auth/group-presets"):
		return "permission_group_preset"
	case strings.HasPrefix(path, "/auth/groups"):
		return "permission_group"
	case strings.HasPrefix(path, "/auth/principals"):
		return "principal"
	case strings.HasPrefix(path, "/auth/tokens"):
		return "credential"
	case path == "/auth/permissions":
		return "role_policy"
	case path == "/auth/default-group":
		return "default_group"
	default:
		return "authentication"
	}
}

func auditTargetID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "auth" {
		return parts[2]
	}
	return ""
}

func auditAction(r *http.Request) string {
	switch {
	case r.URL.Path == "/auth/groups" && r.Method == http.MethodPost:
		return "group.create"
	case r.URL.Path == "/auth/groups" && r.Method == http.MethodGet:
		return "group.list"
	case strings.HasSuffix(r.URL.Path, "/clone"):
		return "group.clone"
	case strings.HasPrefix(r.URL.Path, "/auth/groups/") && strings.HasSuffix(r.URL.Path, "/delete-preview"):
		return "group.delete_preview"
	case strings.HasPrefix(r.URL.Path, "/auth/groups/") && r.Method == http.MethodPatch:
		return "group.update"
	case strings.HasPrefix(r.URL.Path, "/auth/groups/") && r.Method == http.MethodDelete:
		return "group.delete"
	case r.URL.Path == "/auth/default-group":
		return "group.default_update"
	case r.URL.Path == "/auth/principals" && r.Method == http.MethodPost:
		return "principal.create"
	case r.URL.Path == "/auth/principals":
		return "principal.list"
	case strings.HasPrefix(r.URL.Path, "/auth/principals/") && strings.HasSuffix(r.URL.Path, "/delete-preview"):
		return "principal.delete_preview"
	case strings.HasPrefix(r.URL.Path, "/auth/principals/") && r.Method == http.MethodDelete:
		return "principal.delete"
	case strings.HasSuffix(r.URL.Path, "/group"):
		return "principal.group_bind"
	case strings.HasPrefix(r.URL.Path, "/auth/principals/"):
		return "principal.update"
	case r.URL.Path == "/auth/tokens" && r.Method == http.MethodPost:
		return "credential.issue"
	case r.URL.Path == "/auth/tokens":
		return "credential.list"
	case strings.HasPrefix(r.URL.Path, "/auth/tokens/"):
		return "credential.revoke"
	case r.URL.Path == "/auth/permissions":
		return "role_policy.update"
	case r.URL.Path == "/auth/password":
		return "authentication.password_change"
	default:
		return "access.manage"
	}
}
