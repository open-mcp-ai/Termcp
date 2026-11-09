package extensions

import (
	"fmt"
	"sync"
	"testing"
)

// THE CLAIM IS THE FILESYSTEM'S, NOT A LOCK'S. Two imports of the same document,
// racing, must both land: one keeps the name and the other is renamed, and neither
// overwrites the other. A store-wide lock used to provide this; os.Mkdir provides it
// without serialising unrelated saves.
func TestConcurrentImportsNeverOverwriteEachOther(t *testing.T) {
	s := NewStore(t.TempDir())

	const racers = 6
	var wg sync.WaitGroup
	results := make([]*BatchImportResult, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.ImportDocument([]byte(`[[extensions]]
name = "racer"
target = "internal"
command = "true"
`))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed: %v", i, err)
		}
	}

	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != racers {
		t.Fatalf("%d extensions stored for %d racing imports: a claim was lost or an import overwrote another", len(names), racers)
	}
	for _, name := range names {
		if _, err := s.Template(name); err != nil {
			t.Fatalf("stored name %q does not load: %v", name, err)
		}
	}
	// One import kept the original name; every other was renamed.
	renamed := 0
	for _, r := range results {
		renamed += len(r.Renamed)
	}
	if renamed != racers-1 {
		t.Fatalf("%d renames for %d racers, want %d: %v", renamed, racers, racers-1, fmt.Sprint(results))
	}
}
