package session

import (
	"fmt"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// RecordBorrowedCommand adds one finished command to a borrowed container's record.
//
// WHAT THIS IS FOR (issue #255). `run use:<host> $1` runs a command on a pooled
// transport and keeps nothing: no window, no channel, no row anywhere. That was the
// point of the short-circuit (#237), and it is also why an agent's work on a host is
// invisible while it happens. Recording the command against a per-host container turns
// "some connection is busy somewhere" into a session a person can open, read and close
// - without giving the command a channel it would have to be driven through.
//
// IT IS A RECORD, NOT A CHANNEL. The entry has no buffer, no live execSession and no
// entry in s.shells, so nothing can type into it or read from it later: the output
// captured by the caller is stored as the entry's transcript-free metadata and nothing
// else. A session that could be driven through a finished command's entry would be a
// second write path around the approval gate.
//
// THE INDEX IS THE POINT OF ORDERING. Each call takes the next channel number, so the
// sequence a person sees in the window is the sequence the commands ran in - the same
// numbering termcp://#<session>:N uses everywhere.
func (s *Session) RecordBorrowedCommand(command string, exitCode int, startedAt int64) error {
	if s == nil {
		return fmt.Errorf("no session to record into")
	}
	s.shellStateMu.Lock()
	s.nextShellIndex++
	index := s.nextShellIndex
	s.shellStateMu.Unlock()

	code := exitCode
	entry := api.Session{
		ID:      fmt.Sprintf("%s-use-%d", s.ID, index),
		Name:    commandName(command),
		Index:   index,
		Command: command,
		Mode:    api.ModePipe,
		Status:  api.SessionExited,
		ExitCode: &code,
		// CreatedAt is when the command STARTED, so the row's own time is the moment
		// the work happened rather than the moment it was written down.
		CreatedAt: startedAt,
		UpdatedAt: time.Now().UnixMilli(),
	}
	s.shellHistory.Store(entry.ID, entry)
	s.mu.Lock()
	s.UpdatedAt = entry.UpdatedAt
	s.mu.Unlock()
	s.notifyChildChange()
	return nil
}

// commandName is the short label a recorded command carries: its first line, cut to
// something a tab can show. A command is arbitrary text - newlines, redirects, a whole
// script - and a label is not the place for it.
func commandName(command string) string {
	const max = 60
	out := ""
	for _, r := range command {
		if r == '\n' || r == '\r' {
			break
		}
		out += string(r)
		if len(out) >= max {
			return out[:max] + "…"
		}
	}
	if out == "" {
		return "(empty command)"
	}
	return out
}
