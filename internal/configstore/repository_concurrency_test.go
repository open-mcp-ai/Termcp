package configstore

import (
	"sync"
	"testing"
	"time"
)

// The lock this repository used to hold made every reader wait for whichever
// writer was in the middle of a disk write. That is the property this file
// pins: a held writer token must not delay a reader at all.
//
// The deadline is a hang detector, not a budget: a passing run returns on the
// first observation.

func withRepository(t *testing.T) *Repository {
	t.Helper()
	t.Setenv(EnvKEKVariable, environmentKEK(0x5a))
	repo, err := InitializeRepository(t.TempDir(), NewEnvProvider(""))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Save("alpha", []byte("host = \"example\"\n")); err != nil {
		t.Fatalf("save: %v", err)
	}
	return repo
}

func TestReadersDoNotWaitForAWriter(t *testing.T) {
	repo := withRepository(t)
	release := repo.acquireWrite()
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	// Release even when the assertion below fails, or the cleanup Close would
	// wait for a token this test forgot to return.
	defer releaseOnce()

	type result struct {
		what string
		err  error
	}
	done := make(chan result, 3)
	go func() { _, err := repo.List(); done <- result{"List", err} }()
	go func() { _, err := repo.Read("alpha"); done <- result{"Read", err} }()
	go func() { _ = repo.Status(); done <- result{"Status", nil} }()

	for i := 0; i < 3; i++ {
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("%s while a writer holds the token = %v, want success", got.what, got.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a reader parked behind the writer token")
		}
	}
	releaseOnce()
}

func TestWriteTokenAdmitsOneWriterAtATime(t *testing.T) {
	repo := withRepository(t)

	release := repo.acquireWrite()
	entered := make(chan struct{})
	go func() {
		defer close(entered)
		release2 := repo.acquireWrite()
		release2()
	}()

	select {
	case <-entered:
		release()
		t.Fatal("a second writer entered while the token was held")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting writer never acquired the released token")
	}
}

// Close revokes the keyring, so a reader that raced with it must fail closed
// rather than decrypt with a key that has been withdrawn.
func TestReadersFailClosedAfterClose(t *testing.T) {
	repo := withRepository(t)
	if err := repo.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := repo.Read("alpha"); err == nil {
		t.Fatal("Read succeeded on a closed repository")
	}
	if names, err := repo.List(); err == nil && len(names) != 0 {
		t.Fatalf("List on a closed repository = %v, want an error or nothing", names)
	}
}
