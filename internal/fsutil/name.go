package fsutil

import (
	"fmt"
	"strings"
)

// AvailableName returns name, or the first free "name-N" after it.
//
// Two stores needed the same rule and had written it twice, byte for byte: a
// document and an ssh batch are both named by the user, both live in a set with
// case-insensitive uniqueness, and both have to stay inside the same length
// limit. Copying it was the drift waiting to happen — one of them would have
// grown a reserved-name list the other did not know about.
//
// The limit is the caller's, because it comes from different places: a document
// name is bounded by the filesystem's filename limit and a batch name by what
// the config file can round-trip. Folding is unconditional: both callers treat
// "Build" and "build" as the same name, so a candidate is free only if no used
// or reserved entry matches it case-insensitively.
func AvailableName(name string, used, reserved map[string]bool, maxLen int) string {
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := name
		if len(base)+len(suffix) > maxLen {
			base = base[:maxLen-len(suffix)]
		}
		candidate := base + suffix
		folded := strings.ToLower(candidate)
		if !used[folded] && !reserved[folded] {
			return candidate
		}
	}
}
