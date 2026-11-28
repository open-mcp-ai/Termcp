package approval

import (
	"fmt"
	"testing"
	"time"
)

// The queue publishes its whole state as an immutable snapshot, so every
// transition copies both indexes. That is O(n) per transition and n only ever
// grows, because terminal requests are retained for the audit view (issue #217).
// These benchmarks measure what that costs, so the decision to bound the
// retention (or to move to per-key updates) is made on a number.
//
//	go test ./internal/approval -bench Snapshot -benchmem
func BenchmarkSnapshotCostPerSubmit(b *testing.B) {
	for _, n := range []int{10, 100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("backlog=%d", n), func(b *testing.B) {
			q := benchQueue(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := q.Submit(Submission{ShellID: "sh", Text: "ls", Summary: "run ls"}); err != nil {
					b.Fatalf("Submit: %v", err)
				}
			}
		})
	}
}

// benchQueue returns a queue already holding n decided requests, which is the
// state a long session reaches.
func benchQueue(b *testing.B, n int) *Queue {
	b.Helper()
	q := NewQueue("sess-bench", Options{Timeout: time.Hour})
	for i := 0; i < n; i++ {
		id, err := q.Submit(Submission{ShellID: "sh", Text: "ls", Summary: "run ls"})
		if err != nil {
			b.Fatalf("seed Submit: %v", err)
		}
		if _, err := q.Approve(id); err != nil {
			b.Fatalf("seed Approve: %v", err)
		}
	}
	// Drain any notify work the seeds queued.
	_, _ = q.Get("nothing")
	return q
}

// BenchmarkListOnALongBacklog measures the reader side: List copies the whole
// map out of one snapshot, which is O(n) too but happens per call rather than
// per transition.
func BenchmarkListOnALongBacklog(b *testing.B) {
	for _, n := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("backlog=%d", n), func(b *testing.B) {
			q := benchQueue(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := q.List(); len(got) != n {
					b.Fatalf("List returned %d of %d", len(got), n)
				}
			}
		})
	}
}
