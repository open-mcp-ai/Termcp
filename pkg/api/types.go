package api

// SessionStatus represents the current state of a session.
type SessionStatus string

const (
	SessionRunning SessionStatus = "running"
	SessionExited  SessionStatus = "exited"
	SessionError   SessionStatus = "error"
)

// SessionMode represents the execution mode for one shell channel.
type SessionMode string

const (
	ModePTY  SessionMode = "pty"
	ModePipe SessionMode = "pipe"
)

// Session holds metadata for an interactive process session.
//
// Timestamps are Unix milliseconds, the one time representation this project
// stores and serves: log.jsonl marks, manifests, and every JSON API response
// use it. Formatting for display is the client's job (see the Web UI's
// fmtTime), so a value never has to be parsed back out of a string.
type Session struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Mode is a per-shell property: each shell channel picks its own ("pty" for
	// an interactive terminal, "pipe" for a line-oriented run-to-exit command).
	// It is empty on session records — a session is a connection container and
	// owns no mode. The session_start mode is merely the mode of the first shell.
	Mode      SessionMode   `json:"mode,omitempty"`
	Status    SessionStatus `json:"status"` // running | exited | error
	ExitCode  *int          `json:"exit_code"`
	PID       int           `json:"pid"`
	CreatedAt int64         `json:"created_at"` // Unix ms
	UpdatedAt int64         `json:"updated_at"` // Unix ms
	Rows      int           `json:"rows"`
	Cols      int           `json:"cols"`
	// SSHEndpoint is a coarse hint for clients: "internal" (built-in loopback SSH) or "remote" (no host/user/port exposed).
	SSHEndpoint string `json:"ssh_endpoint,omitempty"`
	// ApprovalMode reports whether this session gates input behind N-person
	// approval. It is a session-level policy, not a per-shell one: turning it on
	// covers every shell channel in the session. The Web UI reads it to show that
	// typing is refused and to offer the approval composer instead.
	ApprovalMode bool `json:"approval_mode,omitempty"`
	// ApprovalNeed is the number of distinct approvers required while
	// ApprovalMode is on.
	ApprovalNeed int `json:"approval_need,omitempty"`

	// Shells is a per-shell metadata snapshot, populated only when the session is
	// persisted/restored so a DEAD session can still render its tabs after a
	// restart. Never set on a live running session's Info().
	Shells []Session `json:"shells,omitempty"`
}

// LogStatus classifies a span of a shell's log.bin.
//
// The set is open: adding a status only means introducing a new value, the
// on-disk schema does not change.
type LogStatus string

const (
	// LogOutput is bytes produced by the shell.
	LogOutput LogStatus = "o"
	// LogAIInput is bytes entered by an AI agent through MCP.
	LogAIInput LogStatus = "a"
	// LogAPIInput is bytes entered through the HTTP/WebSocket API (the human at
	// the browser).
	LogAPIInput LogStatus = "i"
	// LogApprovalRequest marks the point where an input was queued for approval
	// instead of being written. The bytes do not exist yet at this offset: the
	// span says "a decision was requested here", and the bytes that eventually
	// appear carry their own input status once an approver releases them.
	LogApprovalRequest LogStatus = "q"
	// LogApprovalGranted marks the point where an approved input was written.
	// A reader replaying the log can therefore tell reviewed input from input
	// that never passed a gate.
	LogApprovalGranted LogStatus = "A"
)

// LogMark is one line of log.jsonl: a transition to `Status` at byte `Offset`
// in the shell's log.bin.
//
// Marks carry no payload — log.bin is the only copy of the bytes. A mark only
// records where a span starts and what produced it. The span runs from its own
// Offset to the next mark's Offset (the last one runs to the end of the file),
// so there is no end field to keep in sync.
//
// Keys are single characters because every session appends these lines for its
// whole life; the file is an index, not a document.
type LogMark struct {
	Status LogStatus `json:"s"`
	Time   int64     `json:"t"` // Unix milliseconds; the moment the span started
	Offset int64     `json:"i"` // byte offset in log.bin where the span starts
}
