package mcp

import (
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// ptyQuiet is how long a pty's stream must stand still before a wait calls the
// command done, when this channel has not been measured yet.
//
// MEASURED, NOT GUESSED. One `echo` on a PowerShell with an oh-my-posh profile
// arrives as five chunks spread over 1.1s: the echo of the typed line at 120ms,
// a first repaint at 510ms, the command's own output at 940ms. The gaps between
// chunks reach 430ms, so a shorter grace ends the wait on the echo alone and the
// reply looks like a command that printed nothing. 500ms covers the widest gap
// this shell showed and still ends a fast command in about a quarter of the 2s
// window a caller typically passes.
//
// IT IS THE STARTING VALUE, not the only one: a pty channel measures its own
// gaps and may come down from here. See ptyQuietTracker.
const ptyQuiet = 500 * time.Millisecond

// The bounds of the learned threshold.
//
// LEARNING MAY ONLY SHORTEN THE WAIT. The ceiling is ptyQuiet itself: a channel
// whose chunks really are 430ms apart keeps today's 500ms, and one whose chunks
// arrive 20ms apart gets 100ms instead. Letting the learned value also LENGTHEN
// the wait was tried and measured: a shell's start-up repaint — one wide burst
// that happens once, when the session opens — was remembered, and the two
// commands after it waited 1.09s where the constant gives 0.68s. An optimisation
// that makes the common case slower is not one, so the learner is only trusted in
// the direction the constant is known to be wrong.
//
// ptyQuietMin is that shortening's floor. It is deliberately above ptyPoll, the
// interval the gaps are sampled at: a shell's start-up echo can arrive in bursts
// 5-50ms apart, and a floor at the sampling resolution would mistake the pause
// between two lines of a looping command for the end of its output. 250ms was
// chosen against the commands that surfaced this — a PowerShell loop printing
// every 200ms must still be reported Still-Running after a 1s window.
//
// There is no safe floor for an arbitrary pty command: a terminal gives no
// completion signal, so the threshold is always a bet that the stream is quiet
// because the command is done rather than between two lines. This is the bet the
// old 500ms constant already made, tightened where the shell has shown it does not
// need the full 500ms, and never loosened elsewhere.
const (
	ptyQuietMin = 250 * time.Millisecond

	// ptyQuietFactor is how many times the widest recent gap to wait. Two, not
	// one: a gap is measured AFTER it happened, so the next one may be slightly
	// longer, and a threshold equal to the observed maximum would fire on the first
	// slow chunk. The margin costs half a gap on a fast channel and saves the reply
	// on a slow one.
	ptyQuietFactor = 2
)

// ptyQuietHistory is how many recent gaps one channel remembers.
//
// A handful, not the whole life of the channel: the number decides when the NEXT
// wait stops, and the only thing that justifies waiting is evidence that this
// shell still talks at those intervals. Sized for roughly two commands' worth of
// chunks, so one quiet command cannot erase what the noisy one beside it proved,
// and a start-up burst ages out within a couple of commands.
const ptyQuietHistory = 8

// ptyQuietTracker learns how long each pty channel's output pauses between
// chunks, so the quiet threshold is this shell's rhythm rather than the widest
// rhythm anyone has ever reported.
//
// WHY PER CHANNEL AND NOT GLOBAL: the 500ms constant is a compromise between a
// shell that repaints over a second and a shell that prints one chunk. Sized for
// the former, it charges every `run` on the latter for a stranger's slowness —
// measured on this machine, `echo x` has all its bytes on the stream by 111ms and
// the wait held the reply to 611ms to be sure nothing else was coming. A global
// threshold cannot be right for both; a measured one is right for each, and its
// floor keeps the measurement from being applied where it has not yet seen a
// command's real interval.
//
// WHAT IT LEARNS FROM: the gaps between chunk arrivals during a wait, of the last
// few chunks only. A command that prints once shows no gap and teaches nothing.
//
// SAFETY: this only decides when to STOP WAITING. Returning early is never lossy —
// the bytes stay in the stream and `read` returns them, the same contract a
// terminal has — while returning late is only slow. That asymmetry is why a
// learned value is allowed to be optimistic.
type ptyQuietTracker struct {
	mu sync.Mutex
	// recent is a ring of the last ptyQuietHistory gaps between two chunks of one
	// command, in arrival order.
	recent [ptyQuietHistory]time.Duration
	next   int
	filled int
}

// threshold is the quiet window this channel should use: twice its widest recent
// gap, floored at ptyQuietMin and never above the ptyQuiet it started from.
func (t *ptyQuietTracker) threshold() time.Duration {
	if t == nil {
		return ptyQuiet
	}
	t.mu.Lock()
	var widest time.Duration
	for i := 0; i < t.filled; i++ {
		if g := t.recent[i]; g > widest {
			widest = g
		}
	}
	t.mu.Unlock()
	if widest <= 0 {
		return ptyQuiet
	}
	d := widest * ptyQuietFactor
	if d < ptyQuietMin {
		return ptyQuietMin
	}
	if d > ptyQuiet {
		return ptyQuiet
	}
	return d
}

// observe records one gap between two chunks of the same command.
//
// The ring holds shortening candidates only, which is why there is no ceiling to
// reject: a wide gap clamps back to ptyQuiet in threshold and is therefore
// harmless, while filling the history with it would push out the narrow gaps that
// carry the information. A pause too wide to be a repaint teaches nothing either
// way, so it is dropped rather than crowding out what does.
func (t *ptyQuietTracker) observe(gap time.Duration) {
	if t == nil || gap <= 0 || gap*ptyQuietFactor > ptyQuiet {
		return
	}
	t.mu.Lock()
	t.recent[t.next] = gap
	t.next = (t.next + 1) % ptyQuietHistory
	if t.filled < ptyQuietHistory {
		t.filled++
	}
	t.mu.Unlock()
}

// seed fills the history with one value, for a caller that has to state what a
// channel does without waiting for it to happen. Held to the same bound: seeding
// a wide gap says nothing about this shell's rhythm.
func (t *ptyQuietTracker) seed(gap time.Duration) {
	if t == nil || gap <= 0 || gap*ptyQuietFactor > ptyQuiet {
		return
	}
	t.mu.Lock()
	for i := range t.recent {
		t.recent[i] = gap
	}
	t.next = 0
	t.filled = ptyQuietHistory
	t.mu.Unlock()
}

// newPtyQuietTracker creates one learner map and wires its lifecycle at the
// server's construction/composition point. A session can contain many channels,
// so one DEAD callback drops each of its handle keys without reaching into session
// internals — the hook has no session locks held while it calls us.
func newPtyQuietTracker(m *session.Manager) *quietTrackers {
	q := &quietTrackers{m: make(map[string]*ptyQuietTracker)}
	if m != nil {
		m.AddOnDeadHook(q.dropSession)
	}
	return q
}

type quietTrackers struct {
	mu sync.Mutex
	m  map[string]*ptyQuietTracker
}

// quietTracker returns the learner for one handle, creating it on first use.
func (q *quietTrackers) quietTracker(handle string) *ptyQuietTracker {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t := q.m[handle]; t != nil {
		return t
	}
	t := &ptyQuietTracker{}
	q.m[handle] = t
	return t
}

// drop forgets one closed channel's threshold.
func (q *quietTrackers) drop(handle string) {
	q.mu.Lock()
	delete(q.m, handle)
	q.mu.Unlock()
}

// peek returns a channel's learner without creating one, for the lifecycle check
// that a closed channel really did leave nothing behind.
func (q *quietTrackers) peek(handle string) *ptyQuietTracker {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.m[handle]
}

// dropSession forgets all thresholds owned by a terminated session. Handle ids
// are opaque to this package; the session prefix is the part before the last ':'.
func (q *quietTrackers) dropSession(sessionID string) {
	prefix := sessionID + ":"
	q.mu.Lock()
	for handle := range q.m {
		if strings.HasPrefix(handle, prefix) {
			delete(q.m, handle)
		}
	}
	q.mu.Unlock()
}

// quietTracker returns the learner for one handle, creating it on first use.
//
// A Server built without New (a test harness) has no learner map, and gets a
// throwaway: the wait uses the floor, and nothing is remembered. That is the
// correct reading of "this server has no lifecycle to clean up after" rather than
// a crash on a field nobody set.
func (s *Server) quietTracker(handle string) *ptyQuietTracker {
	if s.ptyQuiet == nil {
		return &ptyQuietTracker{}
	}
	return s.ptyQuiet.quietTracker(handle)
}

// dropQuietTracker forgets a closed handle's learned threshold.
func (s *Server) dropQuietTracker(handle string) {
	if s.ptyQuiet == nil {
		return
	}
	s.ptyQuiet.drop(handle)
}
