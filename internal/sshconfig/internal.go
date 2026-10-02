package sshconfig

import (
	"strings"
)

func isInternalName(name string) bool {
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
