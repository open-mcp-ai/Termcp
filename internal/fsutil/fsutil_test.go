package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// TestWriteFileAtomicLeavesNoTemporaryFile pins the two things a caller relies
// on and cannot see from the outside: the target has the new bytes, and nothing
// named ".tmp-*" is left behind in its directory.
//
// The temporary file is the failure mode worth testing, because it is invisible
// until someone lists the data directory: a write that fails after CreateTemp
// and before the rename leaves a file that no later run ever cleans up.
func TestWriteFileAtomicLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")

	for i, data := range []string{"first", "second", "third"} {
		if err := WriteFileAtomic(target, []byte(data)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if string(got) != data {
			t.Fatalf("write %d: got %q, want %q", i, got, data)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Errorf("a temporary file survived the write: %s", e.Name())
		}
	}
}

// TestWriteFileAtomicIsSixHundred pins the mode rather than the content: the
// file can hold a private path or a command line, and os.CreateTemp makes it
// 0600, so the only way to lose that is to chmod it back.
func TestWriteFileAtomicIsSixHundred(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits to assert on")
	}
	target := filepath.Join(t.TempDir(), "secret")
	if err := WriteFileAtomic(target, []byte("x")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

// TestRetryStopsOnAnErrorItCannotFix pins that Retry is not "try 20 times
// regardless". It used to be: every error was retried, so a permission problem
// cost half a second of sleeping before being reported unchanged. A permanent
// error must come back on the first attempt.
func TestRetryStopsOnAnErrorItCannotFix(t *testing.T) {
	want := os.ErrPermission
	attempts := 0
	err := Retry(func() error {
		attempts++
		return want
	})
	if err != want {
		t.Fatalf("Retry returned %v, want it to pass the error through", err)
	}
	if attempts != 1 {
		t.Fatalf("Retry attempted %d times on a permanent error, want 1", attempts)
	}
}

// TestConcurrentWritesAlwaysLeaveOneWholeFile is the reason the rename is
// atomic: readers and writers of the same store run on different goroutines, and
// a reader must never see a half-written file. Every observation is one of the
// complete payloads, never a prefix and never a mix.
func TestConcurrentWritesAlwaysLeaveOneWholeFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	payloads := []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	var bad error
	var mu sync.Mutex

	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := WriteFileAtomic(target, []byte(payloads[i%len(payloads)])); err != nil {
				mu.Lock()
				bad = err
				mu.Unlock()
				return
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				got, err := os.ReadFile(target)
				if err != nil {
					continue // not created yet, or replaced between open and read
				}
				switch string(got) {
				case payloads[0], payloads[1], payloads[2]:
				default:
					mu.Lock()
					bad = &tornRead{string(got)}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	// THE WRITER MUST BE JOINED BEFORE `bad` IS READ. `close(stop)` only ASKS it to
	// stop; without this wait the writer can still store into `bad` while the check
	// below loads it, which is the race the detector reported on macos — and it is
	// real, not a false positive: the read is of a variable the goroutine owns.
	<-writerDone
	if bad != nil {
		t.Fatalf("a reader saw a file that was never written whole: %v", bad)
	}
}

type tornRead struct{ got string }

func (e *tornRead) Error() string {
	return "partial content " + e.got
}
