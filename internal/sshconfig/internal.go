package sshconfig

import (
	"strings"
)

// IsInternalName reports whether name denotes the built-in loopback profile.
// The name is reserved rather than stored, so every layer that has to single it
// out — the store, the batch routes, the Web UI's export and delete — asks here
// instead of comparing the string itself.
func IsInternalName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "internal")
}

// InternalEntry returns the built-in loopback profile (not loaded from disk).
func InternalEntry() *Entry {
	ent, err := ParseAndValidate(InternalTemplate())
	if err != nil {
		// Template is compile-time constant; fallback if somehow invalid.
		return &Entry{Kind: KindInternal}
	}
	return ent
}
