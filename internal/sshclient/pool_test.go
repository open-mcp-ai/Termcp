package sshclient

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestTransportPoolAcquireReleasesAndLingers verifies that a transport pool
// reuses a client while leases are held, stays warm during linger, and reclaims
// the entry after linger expiry.
func TestTransportPoolAcquireReleasesAndLingers(t *testing.T) {
	pool := NewTransportPool(50 * time.Millisecond)
	var dialCount atomic.Int64

	dialer := func(ctx context.Context) (*ssh.Client, []io.Closer, error) {
		dialCount.Add(1)
		// Return a client wrapper with nil conn for test logic
		return &ssh.Client{}, nil, nil
	}

	ctx := context.Background()
	c1, rel1, err := pool.Acquire(ctx, "hostA", dialer)
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if dialCount.Load() != 1 {
		t.Fatalf("dialCount = %d, want 1", dialCount.Load())
	}

	// Second acquire while lease 1 is held must reuse the existing client without dialing
	c2, rel2, err := pool.Acquire(ctx, "hostA", dialer)
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("c1 != c2, expected shared client")
	}
	if dialCount.Load() != 1 {
		t.Fatalf("dialCount after acquire 2 = %d, want 1", dialCount.Load())
	}

	rel1()
	rel2()

	// Immediately re-acquire within the 50ms linger window: must still reuse
	c3, rel3, err := pool.Acquire(ctx, "hostA", dialer)
	if err != nil {
		t.Fatalf("Acquire 3: %v", err)
	}
	if c1 != c3 {
		t.Fatalf("c3 != c1, expected client reuse during linger")
	}
	if dialCount.Load() != 1 {
		t.Fatalf("dialCount after acquire 3 = %d, want 1", dialCount.Load())
	}
	rel3()

	// Wait past the linger period: entry must expire
	time.Sleep(80 * time.Millisecond)

	// Fourth acquire must dial a new client
	c4, rel4, err := pool.Acquire(ctx, "hostA", dialer)
	if err != nil {
		t.Fatalf("Acquire 4: %v", err)
	}
	defer rel4()
	if c4 == c1 {
		t.Fatalf("c4 == c1, expected new client after linger teardown")
	}
	if dialCount.Load() != 2 {
		t.Fatalf("dialCount after expire = %d, want 2", dialCount.Load())
	}
}

// TestTransportPoolSingleFlight verifies that concurrent dials for the same key
// share one dial flight rather than dialing N parallel sockets.
func TestTransportPoolSingleFlight(t *testing.T) {
	pool := NewTransportPool(50 * time.Millisecond)
	var dialCount atomic.Int64

	dialer := func(ctx context.Context) (*ssh.Client, []io.Closer, error) {
		dialCount.Add(1)
		time.Sleep(30 * time.Millisecond)
		return &ssh.Client{}, nil, nil
	}

	const conc = 5
	clients := make([]*ssh.Client, conc)
	releases := make([]func(), conc)
	errs := make([]error, conc)

	start := make(chan struct{})
	done := make(chan struct{})

	for i := 0; i < conc; i++ {
		go func(idx int) {
			<-start
			c, rel, err := pool.Acquire(context.Background(), "hostB", dialer)
			clients[idx] = c
			releases[idx] = rel
			errs[idx] = err
			done <- struct{}{}
		}(i)
	}

	close(start)
	for i := 0; i < conc; i++ {
		<-done
	}

	for i := 0; i < conc; i++ {
		if errs[i] != nil {
			t.Fatalf("client %d failed: %v", i, errs[i])
		}
		if releases[i] != nil {
			releases[i]()
		}
	}

	if dialCount.Load() != 1 {
		t.Fatalf("dialCount = %d, want exactly 1 single-flight dial", dialCount.Load())
	}
	for i := 1; i < conc; i++ {
		if clients[i] != clients[0] {
			t.Fatalf("client[%d] != client[0], not shared", i)
		}
	}
}
