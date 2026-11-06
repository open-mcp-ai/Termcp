package fsutil

import "testing"

// TestAvailableNameIsTheSameRuleForEveryStore pins the rule that two stores had
// written twice before it lived here.
//
// The cases are the ones that made the two copies worth merging: the first free
// suffix (not the second), case-insensitive folding in BOTH maps (a stored name
// and a name this same batch is about to store are equally taken), and the
// length limit applied to the SUFFIXED name rather than to the base — a base at
// the limit still has to grow a "-2" without overflowing it.
func TestAvailableNameIsTheSameRuleForEveryStore(t *testing.T) {
	none := map[string]bool{}

	if got := AvailableName("build", none, none, 64); got != "build-2" {
		t.Errorf("a free name must not be suffixed at all; got %q", got)
	}

	used := map[string]bool{"build-2": true}
	if got := AvailableName("build", used, none, 64); got != "build-3" {
		t.Errorf("the suffix must be the first FREE one; got %q", got)
	}

	// Folding is the point: a store that treats Build and build as one name must
	// not hand out both.
	usedMixed := map[string]bool{"build-2": true}
	if got := AvailableName("build", usedMixed, none, 64); got != "build-3" {
		t.Errorf("a used name must be compared case-insensitively; got %q", got)
	}

	// Reserved is the same kind of taken: it is what the batch is about to write.
	reserved := map[string]bool{"build-2": true}
	if got := AvailableName("build", none, reserved, 64); got != "build-3" {
		t.Errorf("a reserved name must count as taken; got %q", got)
	}

	// The limit applies to the whole candidate, so a name already at the limit is
	// trimmed to make room for the suffix instead of exceeding it.
	long := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 63
	got := AvailableName(long, none, none, 64)
	if len(got) != 64 {
		t.Errorf("a suffixed name must fit the limit exactly; got %d bytes (%q)", len(got), got)
	}
	if got[len(got)-2:] != "-2" {
		t.Errorf("the suffix must survive the trim; got %q", got)
	}
}
