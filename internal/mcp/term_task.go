package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/task"
)

// directiveGetTask answers `get task <id>`, waiting out the caller's window.
//
// THE WINDOW IS THE WAIT, exactly as it is for a pty read: the reply comes back the
// instant the transfer finishes, and one that does not finish inside the window
// answers with its progress rather than an error. That is the whole point of a
// receipt — the caller decides how long to wait, not a watchdog it cannot see.
func (s *Server) directiveGetTask(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	id := strings.TrimSpace(stmt.Key)
	if id == "" {
		id = strings.TrimSpace(stmt.Target)
	}
	if id == "" {
		return badRequest(stmt, "get task needs the id from the receipt: get task task_abc123")
	}
	timeout := time.Duration(win.TimeoutSeconds * float64(time.Second))
	v, err := s.tasks.Await(ctx, id, timeout)
	if err != nil {
		return notFoundSlot(stmt, CodeInvalidArgument, err.Error()+
			"\nA receipt is answerable for ten minutes after the transfer ends; `ls task` lists the live ones.")
	}
	code := 200
	if v.Status == task.Failed {
		code = 500
	}
	return envelope.Slot{Code: code, Headers: map[string]string{
		"Path": stmt.Path, "Task": v.ID, "Status": string(v.Status),
	}, Body: renderTask(v)}
}

// directiveLsTask answers `ls task`: every transfer this process knows about.
func (s *Server) directiveLsTask(stmt directive.Statement) envelope.Slot {
	list := s.tasks.List()
	if len(list) == 0 {
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path},
			Body: "no transfers. A pipeline (`read <source> file <path> | write <target> file <path>`) starts one and answers with a task id."}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s):\n", len(list))
	for _, v := range list {
		fmt.Fprintf(&b, "  %-16s %-9s %-9s %s\n", v.ID, v.Kind, v.Status, taskProgress(v))
	}
	b.WriteString("\n`get task <id>` waits for one; `close task <id>` cancels it.")
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// directiveCloseTask answers `close task <id>`: cancel the transfer, do not delete
// the receipt.
func (s *Server) directiveCloseTask(ctx context.Context, stmt directive.Statement) envelope.Slot {
	id := strings.TrimSpace(stmt.Key)
	if id == "" {
		id = strings.TrimSpace(stmt.Target)
	}
	if id == "" {
		return badRequest(stmt, "close task needs the id: close task task_abc123")
	}
	stopped, err := s.tasks.Cancel(id)
	if err != nil {
		return notFoundSlot(stmt, CodeInvalidArgument, err.Error()+"\n`ls task` lists the live ones.")
	}
	v, gerr := s.tasks.Get(id)
	if gerr != nil {
		return notFoundSlot(stmt, CodeInvalidArgument, gerr.Error())
	}
	if !stopped {
		// Already finished: the caller asked to stop something that is not running, and
		// the honest answer names the state it is in instead of pretending to cancel.
		return envelope.Slot{Code: 409, Headers: map[string]string{"Path": stmt.Path, "Task": id},
			Body: "nothing to cancel: " + id + " is already " + string(v.Status) + "\n" + renderTask(v)}
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path, "Task": id, "Status": "cancelled"},
		Body: "cancelled " + id + "; the partial file was removed at both ends\n" + renderTask(v)}
}

// renderTask is the one shape every task reply uses.
func renderTask(v task.View) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task:     %s\n", v.ID)
	fmt.Fprintf(&b, "Kind:     %s\n", v.Kind)
	fmt.Fprintf(&b, "Status:   %s\n", v.Status)
	fmt.Fprintf(&b, "Progress: %s\n", taskProgress(v))
	if rate := v.Rate(); rate > 0 {
		fmt.Fprintf(&b, "Rate:     %s/s\n", humanBytes(rate))
	}
	fmt.Fprintf(&b, "Path:     %s -> %s\n", v.LocalPath, v.Path)
	if v.Error != "" {
		fmt.Fprintf(&b, "Error:    %s\n", v.Error)
	}
	return b.String()
}

// taskProgress renders "done/total (pct)" without inventing a total.
func taskProgress(v task.View) string {
	if v.BytesTotal <= 0 {
		return humanBytes(v.BytesDone) + " (total unknown)"
	}
	return fmt.Sprintf("%s/%s (%d%%)", humanBytes(v.BytesDone), humanBytes(v.BytesTotal), v.Percent())
}
