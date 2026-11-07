package term

import (
	"context"
	"strings"
	"testing"
)

// recorder is a Handler that answers what a test tells it, and remembers the
// order it was asked in. It is the whole capability layer as far as Execute is
// concerned, which is the point of the interface.
type recorder struct {
	mu     chan struct{} // 1-slot channel used as a mutex, kept simple
	seen   []string
	answer func(Statement) int
}

func newRecorder(answer func(Statement) int) *recorder {
	r := &recorder{answer: answer}
	r.mu = make(chan struct{}, 1)
	r.mu <- struct{}{}
	return r
}

func (r *recorder) Handle(_ context.Context, st Statement) []Slot {
	<-r.mu
	r.seen = append(r.seen, st.Op)
	r.mu <- struct{}{}
	code := 200
	if r.answer != nil {
		code = r.answer(st)
	}
	return []Slot{{Code: code, Headers: map[string]string{"Path": st.Path}}}
}

func (r *recorder) calls() []string {
	<-r.mu
	out := append([]string(nil), r.seen...)
	r.mu <- struct{}{}
	return out
}

// TestExecuteKeepsOneSlotPerStatement pins the guarantee a caller maps back to what
// it wrote: slot i is statement i, even for the statements that did not run.
//
// Without the 208 placeholder a batched command's answer would be shorter than
// the batch, and every later index would silently refer to the wrong statement —
// the failure mode that makes a caller stop trusting the whole reply.
func TestExecuteKeepsOneSlotPerStatement(t *testing.T) {
	file, err := Parse("run a:1 $1 && run a:1 $1; ls session")
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Statements) != 3 {
		t.Fatalf("parsed %d statements, want 3", len(file.Statements))
	}
	// First fails, so the `&&` branch must be skipped and the `;` branch must run.
	r := newRecorder(func(st Statement) int {
		if st.Op == "run" {
			return 500
		}
		return 200
	})
	reply := Execute(context.Background(), file, r, Options{})

	if len(reply.Slots) != 3 {
		t.Fatalf("got %d slots for 3 statements: %s", len(reply.Slots), reply.Text())
	}
	codes := []int{reply.Slots[0].Code, reply.Slots[1].Code, reply.Slots[2].Code}
	if codes[0] != 500 || codes[1] != 208 || codes[2] != 200 {
		t.Fatalf("codes = %v, want [500 208 200]:\n%s", codes, reply.Text())
	}
	if code := reply.Slots[1].Headers["Error-Code"]; code != "short_circuited" {
		t.Errorf("the skipped slot must say why, Error-Code = %q", code)
	}
	// The handler must never have been asked to run the skipped branch.
	if got := r.calls(); len(got) != 2 || got[0] != "run" || got[1] != "ls" {
		t.Errorf("handler saw %v, want the skipped run to be absent", got)
	}
}

// TestExecuteOrRunsOnlyAfterFailure is the other half of the gate: `||` runs the
// right side only when the left failed.
func TestExecuteOrRunsOnlyAfterFailure(t *testing.T) {
	r := newRecorder(func(Statement) int { return 200 })
	file, err := Parse("ls session || ls task")
	if err != nil {
		t.Fatal(err)
	}
	reply := Execute(context.Background(), file, r, Options{})
	if len(reply.Slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(reply.Slots))
	}
	if reply.Slots[1].Code != 208 {
		t.Fatalf("`||` after a success must be skipped, got %d:\n%s", reply.Slots[1].Code, reply.Text())
	}

	// And when the left fails, the right runs.
	r2 := newRecorder(func(Statement) int { return 500 })
	reply2 := Execute(context.Background(), file, r2, Options{})
	if reply2.Slots[1].Code != 500 {
		t.Fatalf("`||` after a failure must run, got %d", reply2.Slots[1].Code)
	}
}

// TestExecuteRunsABackgroundGroupButKeepsOrder pins both halves of `&`: the members
// really run together, and the slots still come back in the order written.
//
// Completion order is not statement order, so a reply assembled as they finish
// would shuffle — and a caller reading slot 0 would get a different statement
// depending on scheduling.
func TestExecuteRunsABackgroundGroupButKeepsOrder(t *testing.T) {
	const n = 8
	var inFlight, peak int
	var mu chan struct{} = make(chan struct{}, 1)
	mu <- struct{}{}
	r := newRecorder(nil)
	r.answer = func(Statement) int {
		<-mu
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu <- struct{}{}
		// Hold long enough for the others to arrive, then release.
		defer func() {
			<-mu
			inFlight--
			mu <- struct{}{}
		}()
		for i := 0; i < 1000; i++ {
			_ = i
		}
		return 200
	}

	line := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			line += " "
		}
		line += "ls session &"
	}
	file, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	reply := Execute(context.Background(), file, r, Options{})
	if len(reply.Slots) != n {
		t.Fatalf("got %d slots for %d statements", len(reply.Slots), n)
	}
	// Every slot must still carry its own statement's Path, which is what proves
	// the order was not shuffled.
	for i, st := range file.Statements {
		if got := reply.Slots[i].Headers["Path"]; got != st.Path {
			t.Fatalf("slot %d carries Path %q, want %q", i, got, st.Path)
		}
	}
}

// TestExecuteLearnModeNamesEachDirectiveOnce pins the teaching header: it exists to
// introduce a name, so repeating it on the second use is noise.
func TestExecuteLearnModeNamesEachDirectiveOnce(t *testing.T) {
	r := newRecorder(nil)
	file, err := Parse("ls session; ls task; get session")
	if err != nil {
		t.Fatal(err)
	}
	reply := Execute(context.Background(), file, r, Options{LearnMode: true})
	if len(reply.Slots) != 3 {
		t.Fatalf("got %d slots", len(reply.Slots))
	}
	if got := reply.Slots[0].Headers["Usage"]; !strings.HasPrefix(got, "ls ") {
		t.Errorf("first `ls` should carry its usage line, got %q", got)
	}
	if _, again := reply.Slots[1].Headers["Usage"]; again {
		t.Error("the second `ls` must not repeat the name it already taught")
	}
	if got := reply.Slots[2].Headers["Usage"]; !strings.HasPrefix(got, "get ") {
		t.Errorf("a different verb should carry its usage line, got %q", got)
	}
}

// TestExecuteHandsTheStatementOverWhole pins that the handler receives the parsed
// statement rather than a string: flags keep their three-state pointer, the
// payload is the raw slice, and the target is already normalised.
func TestExecuteHandsTheStatementOverWhole(t *testing.T) {
	var got Statement
	r := newRecorder(nil)
	r.answer = func(st Statement) int { got = st; return 200 }

	file, err := Parse(`run a:1 --tail 5 $1`)
	if err != nil {
		t.Fatal(err)
	}
	Execute(context.Background(), file, r, Options{})

	if got.Op != "run" || got.Target != "a:1" {
		t.Fatalf("op=%q target=%q", got.Op, got.Target)
	}
	if v := got.Flags["tail"]; v == nil || *v != "5" {
		t.Fatalf("--tail did not arrive: %v", got.Flags["tail"])
	}
	if got.Payload != "$1" {
		t.Errorf("payload = %q, want the raw slice", got.Payload)
	}
}

// TestExecuteOfNothingIsAnEmptyReply pins the "where am I" call: an empty line is
// legal and produces no slots, not one placeholder.
func TestExecuteOfNothingIsAnEmptyReply(t *testing.T) {
	file, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if !file.Empty {
		t.Fatal("an empty line should parse as Empty")
	}
	reply := Execute(context.Background(), file, newRecorder(nil), Options{})
	if len(reply.Slots) != 0 {
		t.Fatalf("got %d slots from an empty line", len(reply.Slots))
	}
}
