package webui

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// THE WATCH TABLE HAS ONE OWNER.
//
// Three unrelated goroutines touch it: the tab's readPump adds and removes
// watches, every output pump removes its own on the way out, and the viewer
// registry reads a count. A plain map is therefore not safe, and the obvious
// repair — a mutex — collides with the handoff: a replacement must wait for the
// old pump to be gone before it may acknowledge its own subscription, and a lock
// held across that wait is how one stuck socket becomes every operation on the
// tab hanging (issue #219).
//
// So the table is not shared at all. It lives inside watchOwner, and everyone
// else asks that goroutine to act on it. One owner means no lock; the handoff
// waits on the caller's own goroutine, so the owner is never blocked and never
// blocks anybody.
//
// The cost is a channel round trip per operation. For a table with one to five
// entries, written when a person switches tabs, that is nothing next to what it
// buys: the map is never concurrent, and every ordering rule lives in one file
// where it can be read top to bottom.

// watchOp is one request to the owner. apply runs with exclusive access to the
// table and must contain only map work: anything that can block belongs on the
// caller's goroutine, or the owner stops answering everyone.
type watchOp struct {
	apply func(t *watchTable)
	done  chan struct{}
}

// watchTable is the owner's private state. Nothing outside watchOwner reads or
// writes it.
type watchTable struct {
	entries map[string]*wsWatchEntry
}

// watchOwner is the only goroutine that touches the table.
func (c *uiWS) watchOwner() {
	defer close(c.watchDone)
	t := &watchTable{entries: make(map[string]*wsWatchEntry)}
	for {
		select {
		case <-c.ctx.Done():
			// The socket is going away: stop every pump and exit. Nothing waits for
			// them here, because the teardown must not depend on a stuck send.
			for _, ent := range t.entries {
				if ent != nil && ent.cancel != nil {
					ent.cancel()
				}
			}
			t.entries = make(map[string]*wsWatchEntry)
			return
		case op := <-c.watchOps:
			op.apply(t)
			close(op.done)
		}
	}
}

// watchRequest runs one map operation on the owner and waits for it. The answer
// is carried out through the closure, which is what keeps this a plain map
// instead of a typed message per operation.
func (c *uiWS) watchRequest(apply func(t *watchTable)) {
	op := watchOp{apply: apply, done: make(chan struct{})}
	select {
	case c.watchOps <- op:
	case <-c.watchDone:
		return
	case <-c.ctx.Done():
		return
	}
	select {
	case <-op.done:
	case <-c.watchDone:
	case <-c.ctx.Done():
	}
}

// watchControlEnqueueTimeout bounds how long a subscription control frame waits
// for room on the send queue before the socket is dropped.
const watchControlEnqueueTimeout = 2 * time.Second

// sendTerminalPayload puts one output frame on the wire.
//
// A full queue drops the frame rather than stalling the pump — terminal output may
// be dropped, and the frame's [start,end) range is what lets a client see the
// hole. `false` means the connection is gone and the pump should stop.
//
// THERE IS NO GENERATION CHECK HERE. A pump only runs while it is the current
// watch, because whoever replaced it waited for this pump to exit before
// acknowledging its own subscription: ordering is a consequence of goroutine
// lifetimes, not a rule every frame has to re-establish.
func (c *uiWS) sendTerminalPayload(payload []byte) bool {
	select {
	case c.send <- payload:
		return true
	case <-c.ctx.Done():
		return false
	default:
		return true
	}
}

// sendControlPayload is the same frame class with the opposite queue policy.
//
// A control frame carries the contract — where a subscription starts, where it
// ended, or that it failed — and losing one leaves the client working from a
// premise that is no longer true. So it waits, and a queue that will not accept
// it within the timeout takes the socket down instead: the client sees a
// disconnect and re-subscribes, which is recoverable, where a silently missing
// terminal_done is not.
func (c *uiWS) sendControlPayload(payload []byte) bool {
	timer := time.NewTimer(watchControlEnqueueTimeout)
	defer timer.Stop()
	select {
	case c.send <- payload:
		return true
	case <-c.ctx.Done():
		return false
	case <-timer.C:
		c.cancel()
		_ = c.conn.Close()
		return false
	}
}

// sendWatchError reports a failed subscription to the client.
func (c *uiWS) sendWatchError(sid string, err error) {
	payload, marshalErr := json.Marshal(map[string]string{
		"type":  "watch_error",
		"id":    sid,
		"error": err.Error(),
	})
	if marshalErr == nil {
		c.sendControlPayload(payload)
	}
}

// endWatch is the pump retiring itself: it gives the reader back, removes its own
// entry if it is still the one installed, and closes its handoff signal.
func (c *uiWS) endWatch(sid string, shell *session.ChildShell, ent *wsWatchEntry) {
	shell.UnregisterReader(ent.rid)
	c.watchRequest(func(t *watchTable) {
		// Only the entry still installed is removed: a replacement already put its
		// own there, and that one is not this pump's to take away.
		if cur, ok := t.entries[sid]; ok && cur == ent {
			delete(t.entries, sid)
		}
	})
	// Closing exited is the handoff signal, and only the pump can give it: a
	// replacement waits on this before acknowledging its own subscription, which
	// is what makes "no stale frame behind the new watch_ready" a fact about
	// goroutine lifetimes rather than a check on every send.
	close(ent.exited)
}

func (c *uiWS) runWatch(ctx context.Context, shell *session.ChildShell, sid string, ent *wsWatchEntry) {
	defer c.endWatch(sid, shell, ent)

	/* Bytes are accumulated and shipped as ONE frame per burst, under the two
	   bounds described on terminalFlushBytes. `inBatch` marks a batch in hand,
	   `lastFlush` is what tells an idle echo from a stream. */
	var batch []byte
	var inBatch bool
	var batchStart int64
	var batchEnd int64
	var lastFlush time.Time
	flush := func() bool {
		if len(batch) == 0 {
			inBatch = false
			return true
		}
		// Terminal bytes travel as a plain JSON string; encoding/json escapes
		// whatever it must. base64 would only add a layer the client undoes.
		payload, err := json.Marshal(map[string]any{
			"type":  "terminal",
			"id":    sid,
			"d":     string(batch),
			"start": batchStart,
			"end":   batchEnd,
		})
		batch = batch[:0]
		inBatch = false
		batchStart = 0
		batchEnd = 0
		lastFlush = time.Now()
		if err == nil {
			return c.sendTerminalPayload(payload)
		}
		return true
	}

	for {
		if ctx.Err() != nil {
			return
		}
		/* Wait only as long as the batch in hand may still be held: once its
		   min-gap since the last flush has elapsed there is nothing left to wait
		   for, so the read returns at once and the flush below runs. A quiet pump
		   waits the idle window for a first byte. */
		timeout := terminalOutputIdleWait
		if inBatch {
			if left := terminalFlushMinGap - time.Since(lastFlush); left > 0 {
				timeout = left
			} else {
				timeout = 0
			}
		}
		// NO TRIM: the browser repaints the terminal from these bytes, and a terminal
		// emulator is byte-exact — a withheld byte would be a character that never
		// appeared. Cutting mid-sequence here is correct, and the next chunk finishes it.
		out, _, _, err := shell.ReadTerminalStream(ctx, ent.rid, timeout, false, 0, terminalOutputChunkBytes, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// EOF: buffer closed (shell exited but session kept alive for multiplexing).
			if err == io.EOF {
				// Whatever the batch still holds belongs to the shell's final output
				// and must precede the marker that ends it.
				if !flush() {
					return
				}
				payload, _ := json.Marshal(map[string]any{
					"type":   "terminal_done",
					"id":     sid,
					"cursor": shell.ReaderCursor(ent.rid),
				})
				c.sendControlPayload(payload)
				return
			}
			continue
		}
		if out != "" {
			end := shell.ReaderCursor(ent.rid)
			start := end - int64(len(out))
			if !inBatch {
				batchStart = start
			}
			batchEnd = end
			batch = append(batch, out...)
			inBatch = true
		}
		/* Ship when the batch cannot usefully grow any further:
		     - it reached the byte bound, or
		     - the reader has caught up (no backlog to add to this batch), and the
		       min-gap since the last flush has passed.

		   The backlog test is what makes this free for an interactive echo: one
		   chunk arrives, nothing follows it, so it ships on this same iteration
		   with no timer involved. The min-gap is what bounds a producer that
		   stays level with the reader — one that never leaves a backlog but
		   writes thousands of times a second (a shell echoing a loop line by
		   line), which no backlog test can see. Holding those for a few
		   milliseconds merges them into one frame, and a few milliseconds is far
		   below what a person can notice, while the frame rate it saves is what
		   the page cannot afford. */
		switch {
		case len(batch) >= terminalFlushBytes:
			if !flush() {
				return
			}
		case !shell.HasMoreOutput(ent.rid) && time.Since(lastFlush) >= terminalFlushMinGap:
			if !flush() {
				return
			}
		}
		info := shell.Info()
		if (info.Status != api.SessionRunning || shell.IsBufferClosed()) && !shell.HasMoreOutput(ent.rid) {
			if !flush() {
				return
			}
			payload, err := json.Marshal(map[string]any{
				"type":   "terminal_done",
				"id":     sid,
				"cursor": shell.ReaderCursor(ent.rid),
			})
			if err == nil {
				c.sendControlPayload(payload)
			}
			return
		}
	}
}

func (c *uiWS) addWatch(sid string) error {
	shell := c.getTerminalShell(sid)
	if shell == nil {
		return errWSSessionNotFound
	}
	rid, err := shell.RegisterReader()
	if err != nil {
		return err
	}
	cursor := shell.ReaderCursor(rid)
	ready, err := json.Marshal(map[string]any{
		"type":   "watch_ready",
		"id":     sid,
		"cursor": cursor,
	})
	if err != nil {
		shell.UnregisterReader(rid)
		return err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	ent := &wsWatchEntry{cancel: cancel, rid: rid, exited: make(chan struct{})}

	// Install it and learn what it replaced. The swap is the owner's work and
	// nothing else: it cannot block, so the table stays responsive while this
	// caller does the waiting below.
	var old *wsWatchEntry
	c.watchRequest(func(t *watchTable) {
		old = t.entries[sid]
		t.entries[sid] = ent
	})

	// THE HANDOFF, ON THIS GOROUTINE. The replaced pump is told to stop and this
	// waits for it to be gone before acknowledging the new subscription, so no
	// frame from the previous generation can reach the socket after watch_ready.
	// The wait is bounded by that pump's own sends, which
	// watchControlEnqueueTimeout caps; because it runs here, the owner keeps
	// serving every other session.
	if old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		select {
		case <-old.exited:
		case <-c.ctx.Done():
		}
	}

	// The acknowledgement is enqueued before the pump starts, so a client can treat
	// ready.cursor as the first byte any later frame can carry.
	if !c.sendControlPayload(ready) {
		c.watchRequest(func(t *watchTable) {
			if cur, ok := t.entries[sid]; ok && cur == ent {
				delete(t.entries, sid)
			}
		})
		cancel()
		close(ent.exited)
		shell.UnregisterReader(rid)
		return errWSControlUnavailable
	}
	go c.runWatch(ctx, shell, sid, ent)
	return nil
}

// removeWatch stops streaming one session.
//
// IT CANCELS AND LEAVES THE ENTRY IN PLACE. Deleting here would make the entry
// unreachable while its pump was still alive, and a `watch_add` arriving before
// that pump finished would install a new watch with nothing to hand off from —
// the exact ordering the handoff exists to prevent. Leaving it lets the next
// installation find it, cancel it (already done) and wait for it to exit; the
// pump itself removes the entry as it goes.
func (c *uiWS) removeWatch(sid string) {
	var ent *wsWatchEntry
	c.watchRequest(func(t *watchTable) {
		ent = t.entries[sid]
	})
	if ent != nil && ent.cancel != nil {
		ent.cancel()
	}
}

func (c *uiWS) removeAllWatches() {
	var list []*wsWatchEntry
	c.watchRequest(func(t *watchTable) {
		list = make([]*wsWatchEntry, 0, len(t.entries))
		for _, ent := range t.entries {
			if ent != nil {
				list = append(list, ent)
			}
		}
		t.entries = make(map[string]*wsWatchEntry)
	})
	for _, ent := range list {
		if ent.cancel != nil {
			ent.cancel()
		}
	}
}
