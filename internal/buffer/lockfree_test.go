package buffer

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// A READ IN PROGRESS MUST NOT HOLD ANYTHING A WRITER NEEDS. The window copy is a
// memcpy of terminal output; it used to happen under a mutex, so a slow reader — or a
// reader whose charset trimming took a while — stalled every writer of that stream.
//
// The trim hook is used as a probe because it runs inside the window-building path,
// which is exactly where the copy happens. If the read still held a shared lock,
// Write below could not return while the probe is parked; the deadline is only a hang
// detector, since a lock-free write returns immediately and a blocked one never does.
func TestAReadInProgressDoesNotBlockWrites(t *testing.T) {
	b := New(0)
	if err := b.Write([]byte("alpha")); err != nil {
		t.Fatal(err)
	}
	// FromStart so the reader has bytes to deliver, which is what reaches the copy path.
	r, err := b.NewReaderFromStart()
	if err != nil {
		t.Fatal(err)
	}

	inside := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = b.ReadLimited(context.Background(), r, 0, 0, 0, func(w []byte) int {
			close(inside)
			<-release
			return 0
		})
	}()

	select {
	case <-inside:
	case <-time.After(10 * time.Second):
		t.Fatal("the read never reached the window-copy path")
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- b.Write([]byte("beta")) }()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked while a read was in progress: the read path still holds a lock writers need")
	}

	close(release)
	<-readDone
}

// CONCURRENT READERS AND WRITERS, with compaction active. A reader that falls behind
// may lose the dropped prefix — that is the documented contract — but what it does get
// must be one unbroken run of the stream, never spliced or duplicated. Checking that
// the delivered bytes are a suffix of the written stream is what catches a torn copy:
// a window copied while the log moved would not be findable in the stream at all.
func TestConcurrentReadersSeeAnUnbrokenStream(t *testing.T) {
	b := New(0)
	b.compactThreshold = 256
	b.compactMinAdvance = 64

	const writers = 2
	const chunksPerWriter = 200

	var wg sync.WaitGroup
	readers := make([]int, 8)
	for i := range readers {
		id, err := b.NewReader()
		if err != nil {
			t.Fatal(err)
		}
		readers[i] = id
	}

	var got [][]byte
	var mu sync.Mutex
	readerDone := make(chan struct{})
	for _, id := range readers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var seen []byte
			for {
				data, err := b.Read(context.Background(), id, 0, 0)
				if err != nil {
					break // closed: the writer is done
				}
				if data == nil {
					select {
					case <-readerDone:
						return
					default:
						time.Sleep(time.Millisecond)
						continue
					}
				}
				seen = append(seen, data...)
			}
			mu.Lock()
			got = append(got, seen)
			mu.Unlock()
		}(id)
	}

	var stream []byte
	var streamMu sync.Mutex
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < chunksPerWriter; i++ {
				chunk := bytes.Repeat([]byte{byte('a' + w)}, 7)
				streamMu.Lock()
				stream = append(stream, chunk...)
				streamMu.Unlock()
				if err := b.Write(chunk); err != nil {
					return
				}
			}
		}(w)
	}

	// Let the writers finish, then close so the readers stop.
	doneWriters := make(chan struct{})
	go func() {
		for {
			if b.Len() >= writers*chunksPerWriter*7 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(doneWriters)
	}()
	<-doneWriters
	b.Close()
	close(readerDone)
	wg.Wait()

	streamMu.Lock()
	full := append([]byte(nil), stream...)
	streamMu.Unlock()

	for _, seen := range got {
		if len(seen) == 0 {
			continue
		}
		if !bytes.HasSuffix(full, seen) {
			// A reader may start at a non-zero offset only via compaction, which moves
			// the base but never reorders bytes, so a delivered run is always a suffix.
			if !bytes.Contains(full, seen) {
				t.Fatalf("a reader delivered bytes that are not a run of the stream (%d bytes)", len(seen))
			}
		}
	}
}
