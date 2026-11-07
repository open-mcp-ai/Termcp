package uid

import (
	"regexp"
	"strings"
	"testing"
)

// The two properties a caller depends on. Both are cheap to state and expensive to
// discover in production: an id that repeats corrupts a directory name, and a UUID
// that is not shaped like one is refused by anything that validates it.

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDIsVersionFour(t *testing.T) {
	for i := 0; i < 100; i++ {
		got := UUID()
		if !uuidShape.MatchString(got) {
			t.Fatalf("UUID() = %q, which is not a RFC 4122 v4 id", got)
		}
	}
}

func TestHexHasTwiceTheBytes(t *testing.T) {
	for _, n := range []int{1, 6, 16, 32} {
		got := Hex(n)
		if len(got) != 2*n {
			t.Errorf("Hex(%d) = %q, length %d, want %d", n, got, len(got), 2*n)
		}
		if strings.Trim(got, "0123456789abcdef") != "" {
			t.Errorf("Hex(%d) = %q, which is not lowercase hex", n, got)
		}
	}
}

// TestIdsDoNotRepeat is the property that matters and the one a formatting bug
// would not catch: 10000 draws of a 6-byte id must all differ. The collision
// probability is ~1e-11, so a repeat means the source is not random.
func TestIdsDoNotRepeat(t *testing.T) {
	const draws = 10000
	seen := make(map[string]struct{}, draws)
	for i := 0; i < draws; i++ {
		got := Hex(6)
		if _, dup := seen[got]; dup {
			t.Fatalf("Hex(6) repeated %q after %d draws", got, i)
		}
		seen[got] = struct{}{}
	}
}

// TestTheFallbackIsNotAConstant: if crypto/rand ever fails, the fallback must
// still be unique per call. A constant would turn a loud error into two sessions
// sharing one directory.
func TestTheFallbackIsNotAConstant(t *testing.T) {
	first := fallback()
	second := fallback()
	if first == second {
		t.Fatalf("fallback() returned %q twice; a repeat is a silent collision", first)
	}
	if first == "" {
		t.Fatal("fallback() returned nothing")
	}
}
