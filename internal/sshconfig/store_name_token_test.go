package sshconfig

import (
	"strings"
	"sync"
	"testing"
)

// UNIQUENESS IS CASE-INSENSITIVE, so `prod` and `PROD` are the same profile and the
// second writer must lose. That used to be enforced by one mutex over the whole store;
// it is now the per-name token, keyed by the lowercased name, which is why the two
// names still share a lock while `prod` and `staging` do not.
//
// Repeated rounds because the failure it guards against is a check-then-write race:
// one round can pass by luck, fifty cannot.
func TestSavesDifferingOnlyByCaseCannotBothWin(t *testing.T) {
	s := NewStore(t.TempDir())
	payload := []byte("kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n")

	for round := 0; round < 50; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, name := range []string{"prod", "PROD"} {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				errs[i] = s.Save(name, payload)
			}(i, name)
		}
		wg.Wait()

		won, lost := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case strings.Contains(err.Error(), "already exists"):
				lost++
			default:
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("round %d: %d saves won and %d lost; two names differing only by case must not both be created", round, won, lost)
		}
		names, err := s.List()
		if err != nil {
			t.Fatalf("round %d: List: %v", round, err)
		}
		if len(names) != 2 { // "internal" plus the one that won
			t.Fatalf("round %d: List = %v, want internal plus one profile", round, names)
		}
		// Clean up so the next round starts from an empty store.
		for _, n := range names {
			if !IsInternalName(n) {
				if err := s.Delete(n); err != nil {
					t.Fatalf("round %d: Delete(%s): %v", round, n, err)
				}
			}
		}
	}
}
