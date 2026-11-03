package extensions

import (
	"strings"
	"testing"
)

// TestResidentExtensionPinsDecision pins issue #227 (decisions 1A and 2A):
//
//  1. A resident extension without forwards is NOT lazy: it is brought up from daemon
//     boot, kept for as long as termcp runs, and restarted if it exits.
//  2. Declaring both resident = true and idle_timeout is refused at save time: a resident
//     extension has no idle collection window, so specifying one is an author-level
//     contradiction that must be rejected explicitly rather than silently resolving.
func TestResidentExtensionPinsDecision(t *testing.T) {
	// 1A: resident without forwards is not lazy
	ext := Extension{
		Name:     "background-mcp",
		Target:   "internal",
		Mode:     "pipe",
		Command:  "true",
		Resident: true,
	}
	if err := ext.Validate(); err != nil {
		t.Fatalf("valid resident extension failed validation: %v", err)
	}
	if ext.Lazy() {
		t.Fatalf("resident extension must not be lazy (Lazy() should return false)")
	}

	// 2A: resident + idle_timeout mutual exclusion
	extWithIdle := Extension{
		Name:        "bad-ext",
		Target:      "internal",
		Mode:        "pipe",
		Command:     "true",
		Resident:    true,
		IdleTimeout: "10m",
	}
	err := extWithIdle.Validate()
	if err == nil {
		t.Fatalf("expected validation error for resident + idle_timeout, got nil")
	}
	if !strings.Contains(err.Error(), "resident = true and idle_timeout are alternatives") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
