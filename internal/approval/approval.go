// Package approval gates terminal input behind N-person approval.
//
// The model is one-way: every input reaches a shell as a *command line* that
// already passed approval. A caller writes text and may attach named keys
// (ctrl+c, enter, …); the queue holds the whole submission until Need distinct
// approvers accept it, and only then does the gate hand the bytes to the shell.
// Raw character streams are not representable here on purpose — they cannot be
// reviewed byte by byte, so the WebSocket input path is dropped instead of
// being squeezed through this queue.
//
// Nothing in this package fails open: an expired request, a rejected request, a
// queue that lost its session, and a session whose approval mode was turned off
// mid-flight all end with zero bytes written.
package approval

import (
	"errors"
	"fmt"
	"strings"
)

// State is the lifecycle state of one submitted command line.
type State string

const (
	// Pending is awaiting approvals. It is the only state that can transition
	// further; everything else is terminal.
	Pending State = "pending"
	// Approved was accepted by the reviewer.
	Approved State = "approved"
	// Rejected was refused by any single approver: one veto is enough, because
	// approval is a gate on dangerous input, not a vote on a proposal.
	Rejected State = "rejected"
	// Expired ran out of time before reaching the threshold. It is distinct from
	// Rejected so an audit can tell "someone said no" from "nobody looked".
	Expired State = "expired"
	// Cancelled was dropped by the server rather than by a decision: the session
	// died, the shell closed, or approval mode was turned off while it pending.
	Cancelled State = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool { return s != Pending }

// ErrTimeout is returned by Wait when a request expires.
var ErrTimeout = errors.New("approval timed out")

// LocalReviewer is what a decision records as its author.
//
// It is a constant, not a name typed in: under a single deployment token the
// server cannot verify who clicked, and a self-declared name would read as an
// identity in the audit trail while proving nothing. A deployment that needs
// real attribution issues a token per reviewer and records the authenticated
// subject here instead.
const LocalReviewer = "local"

// ErrNotFound is returned when an id is unknown to this queue.
var ErrNotFound = errors.New("approval request not found")

// ErrNotPending is returned when a decision arrives for a request that already
// reached a terminal state.
var ErrNotPending = errors.New("approval request is no longer pending")

// ErrAlreadyApproved is returned when the same approver approves twice. The
// threshold counts distinct people, so a repeat is not a second vote.
var ErrAlreadyApproved = errors.New("approver has already approved this request")

// Kind discriminates what a queued request will DO when approved.
//
// Review started as a gate on terminal input, where every request was a command
// line (text plus the keys that end it). File transfers and port forwards are
// the same kind of decision — an irreversible change to a host a human may not
// be watching — so they ride the same queue rather than growing a second one.
// The payload differs, hence this field: the reviewer sees a summary and the
// executor dispatches on the kind.
//
// A string, not an enum, so a new operation needs no schema change and an older
// reader renders an unknown kind instead of failing to parse the record.
type Kind string

const (
	// KindShellInput is a command line: Text plus the Keys that end it. The
	// original and still most common case.
	KindShellInput Kind = "shell_input"
	// KindFileWrite writes bytes to a remote path.
	KindFileWrite Kind = "file_write"
	// KindFileDelete removes a remote file or directory.
	KindFileDelete Kind = "file_delete"
	// KindFileRename moves a remote path.
	KindFileRename Kind = "file_rename"
	// KindFileMkdir creates a remote directory.
	KindFileMkdir Kind = "file_mkdir"
	// KindFilePerm changes ownership, mode, or timestamps.
	KindFilePerm Kind = "file_perm"
	// KindFileLink creates a symlink or hard link.
	KindFileLink Kind = "file_link"
	// KindFileTruncate changes a remote file's size.
	KindFileTruncate Kind = "file_truncate"
	// KindForwardOpen opens a port forward (local, remote, or dynamic).
	KindForwardOpen Kind = "forward_open"
	// KindForwardClose closes one.
	KindForwardClose Kind = "forward_close"
)

// Request is one submitted operation awaiting a decision, plus its outcome.
//
// The payload (Kind, Text, Keys, Payload) is immutable after Submit: only the
// bookkeeping fields move. Snapshots deep-copy the slices, so a caller cannot
// mutate a queued item through a returned value.
type Request struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	ShellID   string `json:"shell_id"`
	// Source records which entrance submitted this: "mcp" or "rest". It is a
	// string rather than an enum so a future entrance needs no schema change.
	Source string `json:"source"`

	// Kind says what approving this will do. Empty means KindShellInput: the
	// records written before file and forward operations were gated carry no
	// kind, and they are all command lines.
	Kind Kind `json:"kind,omitempty"`
	// Summary is the one-line description a reviewer reads. For a command line
	// it is derived from Text/Keys; for a file or forward operation it is the
	// operation spelled out ("write 1.2 KB to /etc/hosts"), because the payload is
	// not something a human can read back.
	Summary string `json:"summary,omitempty"`

	// Text is the literal text submitted; Keys are named keys the caller
	// attached (from the approval form, or a shell_key call). Together they are
	// the "one command line" this request stands for.
	Text string   `json:"text"`
	Keys []string `json:"keys,omitempty"`

	// Payload is the kind-specific operation to replay on approval, opaque to
	// this package: it stores and hands back what the executor that registered
	// the kind understands. It is what makes the queue general without teaching
	// it about SFTP or port forwarding.
	Payload []byte `json:"payload,omitempty"`

	CreatedAt  int64 `json:"created_at"` // Unix ms
	ExpiresAt  int64 `json:"expires_at,omitempty"`
	ResolvedAt int64 `json:"resolved_at,omitempty"`

	State State `json:"state"`
	// Approvals lists the approvers who accepted, in order. Its length must
	// reach Need; NeedsMore is the live remainder.
	Approvals []string `json:"approvals,omitempty"`
	Need      int      `json:"need"`
	// DecidedBy is the approver who rejected, when State is Rejected.
	DecidedBy string `json:"decided_by,omitempty"`
	// Reason carries the rejection reason or the cancellation cause.
	Reason string `json:"reason,omitempty"`

	// done is closed exactly once when the request reaches a terminal state.
	// It is the signal Wait blocks on, so an approval is observed immediately
	// instead of on the next poll tick.
	done chan struct{}
}

// NeedsMore is the number of additional distinct approvers required.
func (r Request) NeedsMore() int {
	if n := r.Need - len(r.Approvals); n > 0 {
		return n
	}
	return 0
}

// clone returns a copy safe to hand to callers: the slices are copied so a
// reader cannot append into the queue's backing array, and the signal channel
// is not exposed.
func (r *Request) clone() Request {
	c := *r
	c.Keys = append([]string(nil), r.Keys...)
	c.Approvals = append([]string(nil), r.Approvals...)
	// Payload is copied too: a shallow copy of a slice shares the backing array,
	// so a caller mutating what it got back would edit the queued operation.
	c.Payload = append([]byte(nil), r.Payload...)
	c.done = nil
	return c
}

// String renders the request as a one-line audit entry.
func (r Request) String() string {
	return fmt.Sprintf("%s %s/%s %s text=%q keys=%v approvals=%v/%d",
		r.State, r.SessionID, r.ShellID, r.Source, r.Text, r.Keys, r.Approvals, r.Need)
}

// ShellInputSummary renders a command line for a reviewer.
//
// It lives here rather than in the web UI because the summary travels with the
// queued request: an MCP caller, a REST script, and a browser tab must all see
// the same description of what they are deciding.
func ShellInputSummary(text string, keys []string) string {
	switch {
	case text != "" && len(keys) > 0:
		return text + " + " + strings.Join(keys, ",")
	case text != "":
		return text
	case len(keys) > 0:
		return "keys: " + strings.Join(keys, ",")
	default:
		return "(empty)"
	}
}
