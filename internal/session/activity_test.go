package session

import (
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// activityLog records the input events a Manager fans out.
type activityLog struct {
	mu     sync.Mutex
	events []activityEvent
}

type activityEvent struct {
	shellID string
	src     InputSource
	submit  bool
}

func (a *activityLog) add(shellID string, src InputSource, submit bool) {
	a.mu.Lock()
	a.events = append(a.events, activityEvent{shellID, src, submit})
	a.mu.Unlock()
}

func (a *activityLog) snapshot() []activityEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]activityEvent(nil), a.events...)
}

func (a *activityLog) waitFor(t *testing.T, n int) []activityEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := a.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d activity events, got %d", n, len(a.snapshot()))
	return nil
}

// TestManager_ActivityReportsInputSource pins the signal the channel chip needs:
// who sent input, not just that input happened.
//
// It matters most for the source the browser cannot see. A tab knows its own
// keystrokes, but input an agent sends over MCP never touches the page, so
// without this event a channel driven by an agent would show nothing at all.
// The test therefore drives both paths and asserts they are distinguishable.
func TestManager_ActivityReportsInputSource(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	log := &activityLog{}
	mgr.AddActivityListener(log.add)

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}

	// A person typing through the API.
	if err := cs.SendTerminalBytes([]byte("echo human\n"), false); err != nil {
		t.Fatal(err)
	}
	events := log.waitFor(t, 1)
	if events[0].shellID != cs.ID {
		t.Errorf("activity reported shell %q, want %q", events[0].shellID, cs.ID)
	}
	if events[0].src != InputFromAPI {
		t.Errorf("a human send reported source %v, want InputFromAPI", events[0].src)
	}
	if !events[0].submit {
		t.Error("a send ending in a newline reported submit=false, want true")
	}

	// An agent sending through MCP.
	if err := cs.SendTerminalBytesFrom([]byte("echo agent\n"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	events = log.waitFor(t, 2)
	if events[1].src != InputFromAI {
		t.Errorf("an agent send reported source %v, want InputFromAI", events[1].src)
	}

	// A named key is the other input path (MCP's shell_key) and must report too:
	// an agent that presses enter sends no text, and a chip that only watched
	// text would miss it.
	if err := cs.PressKeyFrom("enter", 1, InputFromAI); err != nil {
		t.Fatal(err)
	}
	events = log.waitFor(t, 3)
	if events[2].src != InputFromAI {
		t.Errorf("a named key reported source %v, want InputFromAI", events[2].src)
	}
	if !events[2].submit {
		t.Error("pressing enter reported submit=false, want true")
	}

	// Text that does not end a line is not a submit. The flag has to be false
	// here or the status would flicker away on every keystroke of a command the
	// agent is still typing.
	if err := cs.SendTerminalBytesFrom([]byte("partial"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	events = log.waitFor(t, 4)
	if events[3].submit {
		t.Error("text without a line ending reported submit=true, want false")
	}

	// More than one listener must be served: the web UI push and any future
	// consumer are independent, and a single slot would silently drop one (the
	// bug the approval listener list exists to prevent).
	second := &activityLog{}
	mgr.AddActivityListener(second.add)
	if err := cs.SendTerminalBytes([]byte("x"), false); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, 5)
	second.waitFor(t, 1)
}

// TestSubmitsLine pins the rule that closes a line, which is the whole reason
// the flag exists: the terminal's byte stream cannot delimit lines (a redraw
// emits a line break without ending anything), so the input side decides and
// this function is that decision.
//
// Every case here is one the chip reads wrong if it flips: an interior newline
// from a paste would report a half-typed command as submitted, and a missed
// ctrl+c would leave the chip on "typing" for the rest of the session — the
// escape hatch is exactly what a person reaches for when a command misbehaves.
func TestSubmitsLine(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"empty input ends nothing", nil, false},
		{"plain text is still being typed", []byte("echo hi"), false},
		{"a carriage return submits", []byte("echo hi\r"), true},
		{"a line feed submits", []byte("echo hi\n"), true},
		{"a CRLF submit (pipe enter) submits", []byte("echo hi\r\n"), true},
		// A paste of several lines is one write; only its last byte ends the line
		// being built, and treating the interior newline as a submit would close
		// the line early.
		{"an interior newline is not a submit", []byte("one\ntwo"), false},
		// xterm wraps a paste in bracketed-paste delimiters when the remote
		// program enables that mode. The final byte is then '~', even if the
		// pasted text itself ends in a newline.
		{"bracketed paste hides a final newline from the last-byte rule", []byte("\x1b[200~one\ntwo\n\x1b[201~"), false},
		// Control characters that abandon the line count as ending it, so the chip
		// cannot get stuck on "typing" after an interrupt.
		{"ctrl+c abandons the line", []byte{0x03}, true},
		{"ctrl+d abandons the line", []byte{0x04}, true},
		{"ctrl+z abandons the line", []byte{0x1a}, true},
		// ...but only when they are the last byte: a ctrl+c in the middle of a
		// paste is followed by more typing.
		{"an interior ctrl+c is not the end", []byte{0x03, 'x'}, false},
		// A cursor key is not a submit: an agent arrowing through history is still
		// composing the line.
		{"an escape sequence is not a submit", []byte{0x1b, '[', 'A'}, false},
		{"backspace is not a submit", []byte{0x7f}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := submitsLine(tc.payload); got != tc.want {
				t.Errorf("submitsLine(%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// TestManager_ActivityReportsControlKeysAsSubmit drives the rule through the
// real input path: the named-key branch has to report the same submit flag the
// text branch does, because a control key is the one input that carries no text.
func TestManager_ActivityReportsControlKeysAsSubmit(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	log := &activityLog{}
	mgr.AddActivityListener(log.add)

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}

	// ctrl+c is how a person abandons a command; the chip must leave "typing".
	if err := cs.PressKeyFrom("ctrl+c", 1, InputFromAPI); err != nil {
		t.Fatal(err)
	}
	events := log.waitFor(t, 1)
	if !events[0].submit {
		t.Error("ctrl+c reported submit=false; a chip would stay on typing after an interrupt")
	}

	// An arrow key is navigation, not submission: arrowing through history leaves
	// the line being edited.
	if err := cs.PressKeyFrom("up", 1, InputFromAI); err != nil {
		t.Fatal(err)
	}
	events = log.waitFor(t, 2)
	if events[1].submit {
		t.Error("an arrow key reported submit=true, want false")
	}
}

// TestManager_ActivitySkipsNothingWhenLoggingFails pins the ordering: the event
// is emitted before the log append, so a session without a message manager (or a
// failing one) still reports activity. The chip is live feedback and must not be
// hostage to persistence.
func TestManager_ActivitySurvivesMissingMessageManager(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	// No message manager: the log append has nowhere to go.
	mgr := NewManager(nil, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	log := &activityLog{}
	mgr.AddActivityListener(log.add)

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	if err := cs.SendTerminalBytes([]byte("echo hi\n"), false); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, 1)
}
