package extensions

// A WATCHED EXTENSION IS NOT IDLE (SetWatched).
//
// The idle window answers "does anybody still need this process" with "nobody has asked
// recently", and asking means a request. An event-only peer breaks that equation: its
// normal state is silence and the party that needs it never sends a request — it waits to
// be told something. Without the predicate, a subscriber could watch a healthy peer and
// have it reaped between two events, with nothing said to the subscriber.

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestAWatchedExtensionSurvivesItsIdleWindow(t *testing.T) {
	r := startReconciler(t)
	lazyExt(t, r, "watched", "80ms")

	var watching atomic.Bool
	watching.Store(true)
	r.SetWatched(func(name string) bool { return name == "watched" && watching.Load() })

	if _, err := r.Acquire("watched", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	// SEVERAL WINDOWS' WORTH OF SILENCE, driven by Sync rather than by sleeping on the
	// timer alone: the walk is one of the two roads the rule has to hold on.
	until := time.Now().Add(5 * 80 * time.Millisecond)
	for time.Now().Before(until) {
		r.Sync(t.Context(), "")
		if cur, _ := r.Status("watched"); cur.Instances == 0 {
			t.Fatalf("a watched extension was reaped at its window: %+v", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// AND THE WINDOW STILL WORKS once nobody watches: the predicate suspends the rule, it
	// does not replace it.
	watching.Store(false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.Sync(t.Context(), "")
		if cur, _ := r.Status("watched"); cur.Instances == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an unwatched idle instance was never closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
