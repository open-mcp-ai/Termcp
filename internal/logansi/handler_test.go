package logansi

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// THE INVARIANT THE TOKEN PROTECTS: one record is one line. Concurrent writers must
// not interleave inside a line, or a log becomes unreadable exactly when several
// goroutines report at once.
func TestConcurrentRecordsDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	h := NewTextHandler(&buf, Options{MinLevel: 0})

	var wg sync.WaitGroup
	const writers, records = 8, 50
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < records; i++ {
				// One unique marker per record: a line holding two markers is two
				// records woven together, and a line holding none lost its own.
				n := g*records + i
				rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", uintptr(n))
				rec.AddAttrs(slog.String("marker", fmt.Sprintf("m%d", n)))
				_ = h.Handle(context.Background(), rec)
			}
		}(g)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != writers*records {
		t.Fatalf("got %d lines for %d records: records were merged", len(lines), writers*records)
	}
	for _, line := range lines {
		if n := strings.Count(line, "marker=m"); n != 1 {
			t.Fatalf("a line carries %d records: %q", n, line)
		}
	}
}
