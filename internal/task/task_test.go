package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAReceiptAnswersBeforeTheTransferDoes is the property the whole package
// exists for: a caller gets an id in microseconds and learns the outcome later.
func TestAReceiptAnswersBeforeTheTransferDoes(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", func() {})
	tk.SetTotal(1000)

	go func() {
		<-release
		tk.Add(1000)
		m.Finish(tk.ID(), nil)
	}()

	got, err := m.Get(tk.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Running {
		t.Fatalf("a fresh task must read as running, got %q", got.Status)
	}
	if got.BytesTotal != 1000 {
		t.Fatalf("the receipt must carry the size, got %d", got.BytesTotal)
	}
	if got.Percent() != 0 {
		t.Fatalf("progress before any bytes is 0%%, got %d", got.Percent())
	}
	close(release)

	done, err := m.Await(context.Background(), tk.ID(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != Done || done.BytesDone != 1000 || done.Percent() != 100 {
		t.Fatalf("a finished task must read done at 100%%: %+v", done)
	}
}

// TestAwaitGivesUpAtTheWindow pins the pty contract on a task: the window is a
// bound, and what comes back when it expires is progress, not an error.
func TestAwaitGivesUpAtTheWindow(t *testing.T) {
	m := NewManager()
	tk := m.Start(Download, "s1", "/remote/big", "/local/big", func() {})
	tk.SetTotal(1 << 30)
	tk.Add(1 << 20)

	start := time.Now()
	got, err := m.Await(context.Background(), tk.ID(), 150*time.Millisecond)
	if err != nil {
		t.Fatalf("a window that expires is not a failure: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("Await returned before its window: %v", elapsed)
	}
	if got.Status != Running {
		t.Fatalf("status = %q, want running", got.Status)
	}
	if got.BytesDone != 1<<20 {
		t.Fatalf("the reply must carry the progress made so far, got %d", got.BytesDone)
	}
}

// TestAwaitOnAFinishedTaskDoesNotWait is the other half: a caller that comes back
// after the transfer ended must not pay the window for an answer already known.
func TestAwaitOnAFinishedTaskDoesNotWait(t *testing.T) {
	m := NewManager()
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", func() {})
	m.Finish(tk.ID(), nil)

	start := time.Now()
	got, err := m.Await(context.Background(), tk.ID(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a finished task made Await wait %v", elapsed)
	}
	if got.Status != Done {
		t.Fatalf("status = %q, want done", got.Status)
	}
}

// TestZeroTimeoutIsAPeek pins that timeout 0 asks rather than waits — the same
// rule the terminal's reads follow.
func TestZeroTimeoutIsAPeek(t *testing.T) {
	m := NewManager()
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", func() {})
	start := time.Now()
	if _, err := m.Await(context.Background(), tk.ID(), 0); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("timeout 0 waited %v", elapsed)
	}
}

// TestCancelStopsTheTransferAndIsNotAFailure pins the two facts a cancelled task
// must report: the status is cancelled (not failed, the caller asked for it), and
// the cancel function actually ran.
func TestCancelStopsTheTransferAndIsNotAFailure(t *testing.T) {
	m := NewManager()
	ctx, cancel := context.WithCancel(context.Background())
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", cancel)

	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopped)
		// The transfer's own error path reports the failure it sees; the manager must
		// not let that overwrite the cancellation.
		m.Finish(tk.ID(), context.Canceled)
	}()

	ok, err := m.Cancel(tk.ID())
	if err != nil || !ok {
		t.Fatalf("Cancel = %v, %v; want true, nil", ok, err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach the transfer's context")
	}

	got, _ := m.Get(tk.ID())
	if got.Status != Cancelled {
		t.Fatalf("status = %q, want cancelled (a cancellation the caller asked for is not a failure)", got.Status)
	}
	if got.Error != "" {
		t.Fatalf("a cancelled task must not carry an error: %q", got.Error)
	}
	// Cancelling twice is not an error, and neither is cancelling a finished task.
	if again, err := m.Cancel(tk.ID()); err != nil || again {
		t.Fatalf("second Cancel = %v, %v; want false, nil", again, err)
	}
}

// TestFailureCarriesTheReason pins that a transfer that broke says why, because
// "failed" alone sends the caller looking for a problem that is already known here.
func TestFailureCarriesTheReason(t *testing.T) {
	m := NewManager()
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", func() {})
	m.Finish(tk.ID(), errors.New("sftp: permission denied"))

	got, _ := m.Get(tk.ID())
	if got.Status != Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "permission denied") {
		t.Fatalf("the failure lost its reason: %q", got.Error)
	}
}

// TestFinishOnlyLandsOnce pins that a cancelled task cannot be flipped to failed by
// a later Finish — the race a cancel runs into when the copy returns an error just
// after the cancellation.
func TestFinishOnlyLandsOnce(t *testing.T) {
	m := NewManager()
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", func() {})
	m.Cancel(tk.ID())
	m.Finish(tk.ID(), errors.New("copy: context canceled"))

	got, _ := m.Get(tk.ID())
	if got.Status != Cancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
}

// TestUnknownTaskIsNotFound pins the distinction the caller needs: a typo and an
// expired receipt are the same answer, and it must not be "still running".
func TestUnknownTaskIsNotFound(t *testing.T) {
	m := NewManager()
	if _, err := m.Get("task_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := m.Await(context.Background(), "task_nope", time.Millisecond); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Await(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := m.Cancel("task_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(unknown) = %v, want ErrNotFound", err)
	}
}

// TestConcurrentProgressAndReadsAreConsistent is the check a race detector cannot
// make on its own: progress is written by the copy loop and read by whoever asks,
// and a torn read would report a byte count that never existed.
func TestConcurrentProgressAndReadsAreConsistent(t *testing.T) {
	m := NewManager()
	tk := m.Start(Upload, "s1", "/remote/x", "/local/x", func() {})
	tk.SetTotal(100000)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			tk.Add(100)
		}
		close(stop)
	}()
	// Readers run while the writer works; every snapshot must be monotonic and never
	// exceed the total.
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := int64(-1)
		for {
			select {
			case <-stop:
				return
			default:
			}
			v, _ := m.Get(tk.ID())
			if v.BytesDone < last {
				t.Errorf("progress went backwards: %d then %d", last, v.BytesDone)
				return
			}
			if v.BytesDone > 100000 {
				t.Errorf("progress exceeded the total: %d", v.BytesDone)
				return
			}
			last = v.BytesDone
		}
	}()
	wg.Wait()

	final, _ := m.Get(tk.ID())
	if final.BytesDone != 100000 {
		t.Fatalf("final progress = %d, want 100000", final.BytesDone)
	}
}

// TestListIsNewestFirst pins the order a caller reads receipts in.
func TestListIsNewestFirst(t *testing.T) {
	m := NewManager()
	first := m.Start(Upload, "s1", "/a", "/a", func() {})
	time.Sleep(2 * time.Millisecond)
	second := m.Start(Upload, "s1", "/b", "/b", func() {})

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d tasks, want 2", len(list))
	}
	if list[0].ID != second.ID() {
		t.Fatalf("List is not newest-first: %s then %s", list[0].ID, list[1].ID)
	}
	_ = first
}

// TestTaskAddAndPollAreLockFreeAndRaceClean pins that Add runs concurrently with
// readers without data races or mutex contention (issue #223).
func TestTaskAddAndPollAreLockFreeAndRaceClean(t *testing.T) {
	task := &Task{startedAt: time.Now()}
	task.SetTotal(1000 * 32 * 1024)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 4 concurrent writers simulating chunks landing from multiple streams/pipes
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					task.Add(32 * 1024)
					time.Sleep(10 * time.Microsecond)
				}
			}
		}()
	}

	// 2 concurrent readers polling progress (the Web UI pattern)
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					v := task.view()
					if v.BytesDone < 0 {
						t.Errorf("negative bytes: %d", v.BytesDone)
					}
					time.Sleep(20 * time.Microsecond)
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if task.view().BytesDone <= 0 {
		t.Fatal("no bytes accumulated")
	}
}
