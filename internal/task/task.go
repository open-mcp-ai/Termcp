// Package task tracks the asynchronous file transfers a caller started and has
// not yet collected.
//
// WHY TRANSFERS ARE ASYNC AT ALL. A caller of this daemon is usually an agent
// behind a host that enforces a watchdog on every tool call — 30s and 60s are
// common. An SFTP transfer of a 30 MB binary takes longer than that on a slow
// link, so a synchronous write is not "slow": it is a call that WILL be killed
// partway, with a half-written file on the far side and no error anywhere (#158).
// Handing back a receipt in milliseconds and letting the caller ask about it is
// the only shape that survives a watchdog it does not control.
//
// The manager is deliberately in-memory and process-wide: a transfer lives in the
// goroutine that started it, and the daemon is one process. Finished tasks are
// kept for a while so a caller that lost the receipt can still ask.
package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Status is where a task is in its life.
type Status string

const (
	Running   Status = "running"
	Done      Status = "done"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Kind classifies the transfer, which decides which end is authoritative for the
// byte count.
type Kind string

const (
	Upload   Kind = "upload"
	Download Kind = "download"
	// Copy is one node to another: `read … | write …`. It is neither an upload nor a
	// download — the bytes never touch this process's caller and usually do not touch
	// its disk either — and calling it one of the two would make `get task` answer a
	// question about a direction nobody asked about.
	Copy Kind = "copy"
)

// ErrNotFound is returned for an unknown or expired task id.
var ErrNotFound = fmt.Errorf("no such task")

// retainFinished is how long a finished task stays answerable.
//
// Long enough for a caller to ask once more after its own turn ended (an agent
// that got a receipt, did something else, then came back), short enough that a
// long-lived daemon does not accumulate receipts for transfers nobody remembers.
const retainFinished = 10 * time.Minute

// View is one task as a caller sees it: a plain value, safe to keep or encode.
type View struct {
	ID         string    `json:"id"`
	Kind       Kind      `json:"kind"`
	SessionID  string    `json:"session_id"`
	Path       string    `json:"path"`
	LocalPath  string    `json:"local_path"`
	Status     Status    `json:"status"`
	BytesDone  int64     `json:"bytes_done"`
	BytesTotal int64     `json:"bytes_total"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Percent is the completion percentage, or -1 when the total is unknown (a
// stream whose length the server did not report).
func (v View) Percent() int {
	if v.BytesTotal <= 0 {
		return -1
	}
	p := int(v.BytesDone * 100 / v.BytesTotal)
	if p > 100 {
		return 100
	}
	return p
}

// Rate is the average bytes per second over the task's life.
func (v View) Rate() int64 {
	end := v.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	elapsed := end.Sub(v.StartedAt).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return int64(float64(v.BytesDone) / elapsed)
}

// Task is the mutable record behind a View.
type Task struct {
	id        string
	kind      Kind
	sessionID string
	path      string
	localPath string
	startedAt time.Time
	cancel    context.CancelFunc

	// done and total are atomic so the 32 KB copy loop never takes a lock (the
	// comment on Add documented that intention and previously did the opposite).
	done  atomic.Int64
	total atomic.Int64

	mu       sync.Mutex
	status   Status
	err      error
	endedAt  time.Time
	finished chan struct{}
}

// ID is the receipt a caller keeps.
func (t *Task) ID() string { return t.id }

// Add records n more bytes as transferred. Called from the copy loop, so it is
// lock-free: atomic add, no mutex per chunk (issue #223).
func (t *Task) Add(n int64) {
	t.done.Add(n)
}

// SetTotal records the size the transfer expects to move.
func (t *Task) SetTotal(n int64) {
	t.total.Store(n)
}

// finish moves the task to a terminal status exactly once.
func (t *Task) finish(status Status, err error) {
	t.mu.Lock()
	if t.status != Running {
		t.mu.Unlock()
		return
	}
	t.status = status
	t.err = err
	t.endedAt = time.Now()
	close(t.finished)
	t.mu.Unlock()
}

// view renders the current state as a value.
func (t *Task) view() View {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := View{
		ID: t.id, Kind: t.kind, SessionID: t.sessionID,
		Path: t.path, LocalPath: t.localPath,
		Status: t.status, BytesDone: t.done.Load(), BytesTotal: t.total.Load(),
		StartedAt: t.startedAt, EndedAt: t.endedAt,
	}
	if t.err != nil {
		v.Error = t.err.Error()
	}
	return v
}

// Manager owns every task this process started. Tasks are indexed by id in a
// sync.Map: callers only ever look up, insert, or list them, so there is no
// multi-key invariant for a mutex to protect.
type Manager struct {
	tasks sync.Map // id string -> *Task
}

// NewManager creates an empty manager.
func NewManager() *Manager {
	return &Manager{}
}

func randomID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A collision here would be a wrong receipt, so fall back to something that
		// is unique per process rather than to a constant.
		return fmt.Sprintf("task_%d_%d", os.Getpid(), time.Now().UnixNano())
	}
	return "task_" + hex.EncodeToString(b[:])
}

// Start registers a new running task and returns it.
//
// cancel is stored so the task can be stopped from another turn; a transfer that
// cannot be cancelled is a transfer that runs to completion while its caller is
// told it was cancelled.
func (m *Manager) Start(kind Kind, sessionID, path, localPath string, cancel context.CancelFunc) *Task {
	m.reap()
	for {
		id := randomID()
		t := &Task{
			id: id, kind: kind, sessionID: sessionID, path: path, localPath: localPath,
			startedAt: time.Now(), status: Running, cancel: cancel,
			finished: make(chan struct{}),
		}
		if _, loaded := m.tasks.LoadOrStore(id, t); !loaded {
			return t
		}
	}
}

// reap drops finished tasks that have been answerable long enough.
func (m *Manager) reap() {
	cutoff := time.Now().Add(-retainFinished)
	m.tasks.Range(func(k, v any) bool {
		t, ok := v.(*Task)
		if !ok {
			return true
		}
		t.mu.Lock()
		stale := t.status != Running && t.endedAt.Before(cutoff)
		t.mu.Unlock()
		if stale {
			m.tasks.Delete(k)
		}
		return true
	})
}

// task returns the record for id, or ErrNotFound.
func (m *Manager) task(id string) (*Task, error) {
	v, ok := m.tasks.Load(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	t, _ := v.(*Task)
	return t, nil
}

// Get returns one task's current state.
func (m *Manager) Get(id string) (View, error) {
	t, err := m.task(id)
	if err != nil {
		return View{}, err
	}
	return t.view(), nil
}

// List returns every known task, newest first.
func (m *Manager) List() []View {
	var all []*Task
	m.tasks.Range(func(_, v any) bool {
		if t, ok := v.(*Task); ok {
			all = append(all, t)
		}
		return true
	})

	out := make([]View, 0, len(all))
	for _, t := range all {
		out = append(out, t.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Cancel stops a running task. It reports whether there was anything to stop,
// so "already finished" and "no such task" cannot be confused by the caller.
func (m *Manager) Cancel(id string) (bool, error) {
	t, err := m.task(id)
	if err != nil {
		return false, err
	}
	t.mu.Lock()
	running := t.status == Running
	cancel := t.cancel
	t.mu.Unlock()
	if !running {
		return false, nil
	}
	// Mark first, then cancel: the transfer's own error path would otherwise report
	// "context canceled" as a failure, and a cancellation the caller asked for is not
	// a failure.
	t.finish(Cancelled, nil)
	if cancel != nil {
		cancel()
	}
	return true, nil
}

// Await waits for a task to finish, up to the caller's window.
//
// This is the same contract as a pty read: the window is a bound, not a promise.
// timeout <= 0 is a peek that answers with whatever is true now. A task that
// finishes inside the window returns its terminal state; one that does not returns
// its progress, which is the difference between "still working" and "no answer".
//
// The wait is a channel close, so a caller that asks about a finished task does not
// pay the window.
func (m *Manager) Await(ctx context.Context, id string, timeout time.Duration) (View, error) {
	t, err := m.task(id)
	if err != nil {
		return View{}, err
	}
	if timeout <= 0 {
		return t.view(), nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.finished:
	case <-timer.C:
	case <-ctx.Done():
		return View{}, ctx.Err()
	}
	return t.view(), nil
}

// Finish records the outcome of a transfer. It is called by the transfer itself,
// so a task cannot outlive its goroutine in the running state.
func (m *Manager) Finish(id string, err error) {
	t, gerr := m.task(id)
	if gerr != nil {
		return
	}
	if err != nil {
		t.finish(Failed, err)
		return
	}
	t.finish(Done, nil)
}

// Close stops every running task. Called when the daemon shuts down, so an
// interrupted transfer does not leave a .part file behind without a receipt.
func (m *Manager) Close() {
	var all []*Task
	m.tasks.Range(func(_, v any) bool {
		if t, ok := v.(*Task); ok {
			all = append(all, t)
		}
		return true
	})
	for _, t := range all {
		t.mu.Lock()
		cancel := t.cancel
		running := t.status == Running
		t.mu.Unlock()
		if running && cancel != nil {
			cancel()
		}
	}
}
