// Package uid mints the opaque identifiers this project hands out: session
// ids, shell ids, approval request ids, task receipts and temporary filename
// suffixes.
//
// WHY THIS EXISTS. Four packages had each written the same six lines — a
// crypto/rand read, a hex encode, and a fallback for the read error — and two more
// called into a UUID library to get a string with the same properties. The shapes
// differ (a task receipt is prefixed, a session id is URL-short), so the shared
// part is the RANDOMNESS, not the formatting: one place that reads bytes and says
// what to do when it cannot.
//
// The fallback matters more than it looks. crypto/rand does not fail on any
// platform this runs on, but if it ever did, returning a constant would hand two
// callers the same id and turn a loud error into a silent collision — a wrong
// receipt, or two sessions sharing a directory. The fallback is therefore
// unique-per-process, not fixed.
package uid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Hex returns n random bytes as a lowercase hex string (2n characters).
//
// n is a byte count because that is what the callers think in: a session id is
// short enough to read out of a URL, a task receipt is long enough not to repeat.
func Hex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fallback()
	}
	return hex.EncodeToString(b)
}

// UUID returns a RFC 4122 version 4 identifier in the canonical
// 8-4-4-4-12 form.
//
// The formatting is part of the contract, not decoration: a request id is shown
// in the Web UI and pasted into audit notes, and the dashes are what makes a
// 36-character string reviewable by eye.
func UUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fallback()
	}
	// Version 4 and the RFC 4122 variant, so a checker that reads the bits sees a
	// well-formed v4 id rather than a random blob wearing dashes.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// fallbackCounter makes the fallback unique even when two calls land in the same
// nanosecond. A clock alone is not enough: the resolution of time.Now is coarse
// enough on Windows that a loop minting ids hits the same value twice, which is
// exactly the collision this fallback exists to avoid.
var fallbackCounter atomic.Uint64

// fallback is what a caller gets when the OS cannot produce random bytes.
//
// It is unique per process and per call rather than constant: an id that collides
// is worse than an id that is ugly.
func fallback() string {
	return fmt.Sprintf("r%d-%d-%dx", os.Getpid(), time.Now().UnixNano(), fallbackCounter.Add(1))
}
