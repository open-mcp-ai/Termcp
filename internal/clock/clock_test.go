package clock

import (
	"testing"
	"time"
)

// The package exists so the timestamp unit is chosen in exactly one place. These
// tests pin the unit itself: if someone changes Now to seconds, they fail here
// rather than silently mismatching every manifest already on disk.
func TestNowIsUnixMilliseconds(t *testing.T) {
	got := Now()
	// A Unix-millisecond stamp for any plausible "now" is ~1.7e12 (13 digits);
	// seconds would be ~1.7e9 (10 digits). Range-check instead of comparing to a
	// literal so the test does not need updating as time passes.
	if got < 1_000_000_000_000 || got > 9_999_999_999_999 {
		t.Fatalf("Now() = %d, want a 13-digit Unix-millisecond stamp", got)
	}
	// It must agree with the stdlib's own millisecond conversion.
	if diff := got - time.Now().UnixMilli(); diff > 1000 || diff < -1000 {
		t.Fatalf("Now() = %d, disagrees with time.Now().UnixMilli()", got)
	}
}

func TestMillisAndTimeRoundTrip(t *testing.T) {
	orig := time.Date(2026, 9, 22, 7, 30, 15, 123_000_000, time.UTC)
	ms := Millis(orig)
	if ms%1000 != 123 {
		t.Fatalf("Millis(%v) = %d, want sub-second precision preserved", orig, ms)
	}
	if back := Time(ms); !back.Equal(orig) {
		t.Fatalf("Time(Millis(%v)) = %v, round trip lost precision", orig, back)
	}
}

// Since must interpret its argument as milliseconds. Passing seconds would make
// the result decades off, which is the exact class of bug this package prevents.
func TestSinceInterpretsMilliseconds(t *testing.T) {
	got := Since(Now() - 1500)
	if got < 1*time.Second || got > 3*time.Second {
		t.Fatalf("Since(now-1500ms) = %v, want ~1.5s (argument is milliseconds)", got)
	}
}
