package buffer

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuffer_WriteAndRead(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	b.Write([]byte("hello"))
	b.Write([]byte(" world"))

	data, err := b.Read(context.Background(), r, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(data))
	}

	// Second read with no new data should return empty immediately
	data, err = b.Read(context.Background(), r, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty, got %q", string(data))
	}
}

func TestBuffer_ReadWaitsForData(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	done := make(chan struct{})

	go func() {
		time.Sleep(100 * time.Millisecond)
		b.Write([]byte("delayed"))
		close(done)
	}()

	data, err := b.Read(context.Background(), r, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "delayed" {
		t.Fatalf("expected 'delayed', got %q", string(data))
	}
	<-done
}

func TestBuffer_ReadTimeout(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	start := time.Now()
	data, err := b.Read(context.Background(), r, 200*time.Millisecond, 0)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty, got %q", string(data))
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("returned too fast: %v", elapsed)
	}
}

func TestBuffer_LargeWriteRetainsAll(t *testing.T) {
	b := New(32)
	r, _ := b.NewReader()

	b.Write([]byte(strings.Repeat("a", 16)))
	b.Write([]byte(strings.Repeat("b", 16)))
	b.Write([]byte(strings.Repeat("c", 16)))

	data, err := b.Read(context.Background(), r, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if len(s) != 48 {
		t.Fatalf("expected 48 bytes, got %d", len(s))
	}
	if !strings.Contains(s, strings.Repeat("a", 16)) {
		t.Fatalf("expected 'a' chunk, got %q", s)
	}
	if !strings.Contains(s, strings.Repeat("b", 16)) {
		t.Fatalf("expected 'b' chunk, got %q", s)
	}
	if !strings.Contains(s, strings.Repeat("c", 16)) {
		t.Fatalf("expected 'c' chunk, got %q", s)
	}
}

func TestBuffer_CloseWakesReaders(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	done := make(chan struct{})

	go func() {
		data, err := b.Read(context.Background(), r, 10*time.Second, 0)
		if len(data) != 0 {
			t.Errorf("expected empty on close, got %q", string(data))
		}
		if err != io.EOF {
			t.Errorf("expected io.EOF, got %v", err)
		}
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	b.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the reader")
	}
}

func TestBuffer_WriteAfterClose(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	b.Write([]byte("before"))
	b.Close()

	err := b.Write([]byte("after"))
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}

	data, err := b.Read(context.Background(), r, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before" {
		t.Fatalf("expected 'before', got %q", string(data))
	}
}

func TestBuffer_ConcurrentReadWrite(t *testing.T) {
	b := New(1024 * 1024)
	r, _ := b.NewReader()
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Write([]byte("w"))
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		b.Read(context.Background(), r, 2*time.Second, 0)
	}()

	wg.Wait()
}

func TestBuffer_NewReaderSeededFrom(t *testing.T) {
	b := New(1024)
	src, _ := b.NewReader()
	b.Write([]byte("backlog"))
	dst, err := b.NewReaderSeededFrom(src)
	if err != nil {
		t.Fatal(err)
	}
	gotSrc, err := b.Read(context.Background(), src, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSrc) != "backlog" {
		t.Fatalf("src expected backlog intact, got %q", string(gotSrc))
	}
	gotDst, err := b.Read(context.Background(), dst, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDst) != "backlog" {
		t.Fatalf("dst expected seeded backlog, got %q", string(gotDst))
	}

	b.Write([]byte("more"))
	g1, _ := b.Read(context.Background(), src, time.Second, 0)
	g2, _ := b.Read(context.Background(), dst, time.Second, 0)
	if string(g1) != "more" || string(g2) != "more" {
		t.Fatalf("after write both readers: src=%q dst=%q", string(g1), string(g2))
	}
}

func TestBuffer_MultiReaderIndependence(t *testing.T) {
	b := New(1024)
	r1, _ := b.NewReader()
	r2, _ := b.NewReader()

	b.Write([]byte("hello"))

	data1, err := b.Read(context.Background(), r1, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := b.Read(context.Background(), r2, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}

	if string(data1) != "hello" {
		t.Fatalf("reader 1 expected 'hello', got %q", string(data1))
	}
	if string(data2) != "hello" {
		t.Fatalf("reader 2 expected 'hello', got %q", string(data2))
	}
}

func TestBuffer_MultiReaderSequentialWrite(t *testing.T) {
	b := New(1024)
	r1, _ := b.NewReader()
	r2, _ := b.NewReader()

	b.Write([]byte("chunk1"))
	// r1 reads chunk1
	data1, _ := b.Read(context.Background(), r1, time.Second, 0)
	if string(data1) != "chunk1" {
		t.Fatalf("r1 expected 'chunk1', got %q", string(data1))
	}

	b.Write([]byte("chunk2"))
	// r1 reads only chunk2
	data1, _ = b.Read(context.Background(), r1, time.Second, 0)
	if string(data1) != "chunk2" {
		t.Fatalf("r1 expected 'chunk2', got %q", string(data1))
	}
	// r2 reads both chunk1 and chunk2
	data2, _ := b.Read(context.Background(), r2, time.Second, 0)
	s2 := string(data2)
	if !strings.Contains(s2, "chunk1") || !strings.Contains(s2, "chunk2") {
		t.Fatalf("r2 expected both chunks, got %q", s2)
	}
}

func TestBuffer_Unregister(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	b.Write([]byte("before"))
	b.Unregister(r)

	// Read from unregistered reader should return error
	_, err := b.Read(context.Background(), r, 0, 0)
	if err != ErrReader {
		t.Fatalf("expected ErrReader, got %v", err)
	}
}

func TestBuffer_UnregisterWakesReader(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	done := make(chan error, 1)
	go func() {
		_, err := b.Read(context.Background(), r, 10*time.Second, 0)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	b.Unregister(r)

	select {
	case err := <-done:
		if err != ErrReader {
			t.Fatalf("expected ErrReader after unregister, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unregister did not wake the blocked reader")
	}
}

func TestBuffer_HasMore(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	if b.HasMore(r) {
		t.Fatal("expected no data initially")
	}

	b.Write([]byte("data"))
	if !b.HasMore(r) {
		t.Fatal("expected data after write")
	}

	b.Read(context.Background(), r, 0, 0)
	if b.HasMore(r) {
		t.Fatal("expected no data after read")
	}
}

func TestBuffer_ReadLimitedMaxLinesPreservesUnreadData(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	if err := b.Write([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}

	data, err := b.ReadLimited(context.Background(), r, 0, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("expected first two lines, got %q", string(data))
	}
	if !b.HasMore(r) {
		t.Fatal("expected unread data after max_lines read")
	}

	data, err = b.ReadLimited(context.Background(), r, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "three\nfour\n" {
		t.Fatalf("expected remaining lines, got %q", string(data))
	}
	if b.HasMore(r) {
		t.Fatal("expected no unread data after draining")
	}
}

func TestBuffer_InvalidReader(t *testing.T) {
	b := New(1024)
	_, err := b.Read(context.Background(), 999, 0, 0)
	if err != ErrReader {
		t.Fatalf("expected ErrReader, got %v", err)
	}
}

func TestBuffer_ReadCancelledContext(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	data, err := b.Read(ctx, r, 10*time.Second, 0)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("Read should return quickly on ctx cancel, took %v", elapsed)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty data on cancelled ctx, got %q", string(data))
	}
	if err != nil {
		t.Fatalf("expected nil error on cancelled ctx, got %v", err)
	}
}

func TestBuffer_ReadCancelledContextWithData(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	ctx := context.Background()

	// Write data first, then read with valid ctx; should get data immediately
	b.Write([]byte("hello"))
	data, err := b.Read(ctx, r, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(data))
	}
}

func TestBuffer_NewReaderOnClosed(t *testing.T) {
	b := New(1024)
	b.Close()
	_, err := b.NewReader()
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestBuffer_ReadTimeoutReliability(t *testing.T) {
	// Read must return after timeout even when concurrent Writes cause
	// the reader to loop between Wait calls while the AfterFunc fires.
	b := New(1024)
	r, _ := b.NewReader()

	for i := 0; i < 100; i++ {
		done := make(chan struct{})
		go func() {
			data, err := b.Read(context.Background(), r, 50*time.Millisecond, 0)
			if err != nil && err != io.EOF {
				t.Errorf("unexpected error: %v", err)
			}
			_ = data
			close(done)
		}()

		// Concurrent writes create contention: reader wakes, loops,
		// competes for lock; widens the race window for AfterFunc.
		go func() {
			for j := 0; j < 50; j++ {
				b.Write([]byte("x"))
				time.Sleep(time.Millisecond)
			}
		}()

		select {
		case <-done:
			// good: Read returned within timeout
		case <-time.After(2 * time.Second):
			t.Fatalf("Read blocked forever on iteration %d (AfterFunc race)", i)
		}
	}
}

func TestBuffer_StressConcurrentReadCloseUnregister(t *testing.T) {
	b := New(1024)
	const rounds = 50
	for i := 0; i < rounds; i++ {
		r, _ := b.NewReader()
		var wg sync.WaitGroup
		wg.Add(3)

		go func() {
			defer wg.Done()
			b.Read(context.Background(), r, 100*time.Millisecond, 0)
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				b.Write([]byte("stress"))
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(30 * time.Millisecond)
			b.Unregister(r)
		}()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("deadlock in round %d", i)
		}

		// Reset buffer for next round
		b.Close()
		b = New(1024)
	}
}

func TestBuffer_ByteRange(t *testing.T) {
	b := New(1024)
	b.Write([]byte("abcd"))
	if b.Len() != 4 {
		t.Fatalf("Len: got %d", b.Len())
	}
	p, total := b.ByteRange(1, 2)
	if total != 4 || string(p) != "bc" {
		t.Fatalf("ByteRange(1,2): got %q total=%d", p, total)
	}
	p2, _ := b.ByteRange(0, 100)
	if string(p2) != "abcd" {
		t.Fatalf("ByteRange full: %q", p2)
	}
	p3, _ := b.ByteRange(10, 1)
	if p3 != nil {
		t.Fatalf("expected nil past end")
	}
}

// A reader that is registered but never drained pins the compaction watermark at
// its cursor (maybeCompactLocked uses min(readPos)), so the master buffer grows
// without bound. Unregister must release that pin — this is what session-scope
// cleanup relies on for readers an agent forgot to release.
func TestBuffer_UndrainedReaderPinsCompactionUntilUnregistered(t *testing.T) {
	b := New(0)
	b.compactThreshold = 64
	b.compactMinAdvance = 32

	churn := make([]byte, 32)
	for i := range churn {
		churn[i] = 'x'
	}

	// Active reader that keeps draining, so only the idle reader can pin growth.
	drainer, err := b.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	idle, err := b.NewReader()
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 200
	for i := 0; i < rounds; i++ {
		if err := b.Write(churn); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Read(context.Background(), drainer, 0, 0); err != nil {
			t.Fatal(err)
		}
	}

	pinned := b.Len()
	if pinned < int64(rounds*len(churn)/2) {
		t.Fatalf("expected idle reader to pin growth, master len = %d", pinned)
	}

	b.Unregister(idle)

	// One more write lets the next compaction reclaim the consumed prefix.
	if err := b.Write(churn); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(context.Background(), drainer, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Compaction reclaims memory, and the reported stream length keeps growing:
	// dropping a prefix is an internal optimisation, so it must not change what an
	// offset means or make the stream look shorter than the bytes written. The
	// retained window shrinks while the stream length does not.
	retained := b.Len() - b.BaseOffset()
	if retained >= pinned {
		t.Fatalf("expected compaction after Unregister to reclaim memory: retained=%d before=%d", retained, pinned)
	}
	if after := b.Len(); after <= pinned {
		t.Fatalf("stream length must keep growing across compaction: before=%d after=%d", pinned, after)
	}
}

// ByteRange numbers its argument in the ABSOLUTE stream (baseOffset + position),
// which is the numbering every caller uses: an offset from shell_output, from a
// log file, or from a previous read all mean the same thing. Compaction drops a
// prefix of master and absorbs it into baseOffset, so after compaction the
// absolute total is much larger than the retained slice. These two tests cover
// the two ways that mismatch used to escape: a panic, and a window of NUL bytes.

// TestBuffer_ByteRangeClampsAgainstRetainedBytes is the panic case. A caller
// asks for a window derived from the absolute total - which is exactly what
// shell_output does for offset=0 with max_bytes=0 ("no limit", a documented
// value) - and the retained slice is far shorter. Clamping the window against
// the absolute total instead of the retained length asked the slice expression
// for more bytes than it had.
func TestBuffer_ByteRangeClampsAgainstRetainedBytes(t *testing.T) {
	b := New(0)
	b.compactThreshold = 64
	b.compactMinAdvance = 32

	r, err := b.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("0123456789", 100)) // 1000 bytes
	if err := b.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(context.Background(), r, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(chunk[:100]); err != nil {
		t.Fatal(err)
	}

	base := b.BaseOffset()
	if base == 0 {
		t.Fatal("compaction did not happen; the test no longer exercises a dropped prefix")
	}
	total := b.Len()
	if total <= int64(len(b.master)) {
		t.Fatalf("expected the absolute total (%d) to exceed the retained bytes (%d)", total, len(b.master))
	}

	// The documented shape: offset 0 with no byte limit.
	out, got := b.ByteRange(0, int(total))

	if got != total {
		t.Errorf("total = %d, want %d: the absolute stream length must not change", got, total)
	}
	// start 0 is before the earliest retained byte, so it clamps forward to base:
	// the caller gets the retained suffix and can tell it was shortened by
	// comparing its request against BaseOffset.
	if len(out) != len(b.master) {
		t.Errorf("returned %d bytes, want the %d retained", len(out), len(b.master))
	}
	if want := strings.Repeat("0123456789", 10); string(out) != want {
		t.Errorf("returned %q, want the retained suffix %q", out, want)
	}
}

// TestBuffer_ByteRangeNeverReturnsUnwrittenBytes is the silent case. When the
// requested window happens to fit in the slice's CAPACITY but not its LENGTH,
// a Go slice expression succeeds and the copy returns zeroed bytes: no panic,
// just a screen of NULs that the caller believes is terminal output. The
// returned window must never contain a byte that was not written.
func TestBuffer_ByteRangeNeverReturnsUnwrittenBytes(t *testing.T) {
	b := New(0)
	b.compactThreshold = 64
	b.compactMinAdvance = 32

	r, err := b.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	// Only 'A's are ever written, so any other byte in a result is fabricated.
	chunk := []byte(strings.Repeat("A", 1000))
	if err := b.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(context.Background(), r, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(chunk[:100]); err != nil {
		t.Fatal(err)
	}
	if b.BaseOffset() == 0 {
		t.Fatal("compaction did not happen")
	}

	// Ask for the whole absolute stream, which starts before the retained bytes.
	out, _ := b.ByteRange(0, int(b.Len()))
	for i, c := range out {
		if c != 'A' {
			t.Fatalf("byte %d of the window is %q; only 'A' was ever written", i, c)
		}
	}
}

// TestBuffer_ByteRangeWindowInsideRetainedData pins that the clamp still returns
// exactly the requested window when it lies entirely within the retained bytes,
// so the fix cannot be "return less than asked" for the normal case.
func TestBuffer_ByteRangeWindowInsideRetainedData(t *testing.T) {
	b := New(0)
	b.compactThreshold = 64
	b.compactMinAdvance = 32

	r, err := b.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Write([]byte(strings.Repeat("Z", 200))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(context.Background(), r, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Write([]byte("abcdefghij")); err != nil {
		t.Fatal(err)
	}
	base := b.BaseOffset()
	if base == 0 {
		t.Fatal("compaction did not happen")
	}

	// A window wholly inside the retained region, addressed absolutely.
	out, total := b.ByteRange(base+2, 4)
	if total != b.Len() {
		t.Errorf("total = %d, want %d", total, b.Len())
	}
	if string(out) != "cdef" {
		t.Errorf("got %q, want cdef (absolute offset %d, 4 bytes)", out, base+2)
	}
}
