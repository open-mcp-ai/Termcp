package webui

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

const watchControlEnqueueTimeout = 2 * time.Second

// sendWatchTerminalPayload serializes a terminal frame with watch replacement.
// The watch mutex covers both the generation check and the enqueue, so once
// addWatch replaces an entry no stale pump can put a frame behind the new
// watch_ready acknowledgement.
func (c *uiWS) sendWatchTerminalPayload(sid string, rid int, payload []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.watch[sid]
	if entry == nil || entry.rid != rid {
		return false
	}
	select {
	case c.send <- payload:
	case <-c.ctx.Done():
	default:
	}
	return true
}

func (c *uiWS) sendWatchControlPayload(sid string, rid int, payload []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.watch[sid]
	if entry == nil || entry.rid != rid {
		return false
	}
	return c.sendControlPayload(payload)
}

// sendControlPayload never silently drops a subscription control frame. If the
// connection cannot accept it promptly, the socket is closed so the client
// observes a disconnect instead of believing that a subscription is established.
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

func (c *uiWS) endWatch(sid string, shell *session.ChildShell, rid int) {
	shell.UnregisterReader(rid)
	c.mu.Lock()
	if ent, ok := c.watch[sid]; ok && ent != nil && ent.rid == rid {
		delete(c.watch, sid)
	}
	c.mu.Unlock()
}

func (c *uiWS) runWatch(ctx context.Context, shell *session.ChildShell, sid string, rid int) {
	defer c.endWatch(sid, shell, rid)

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
			return c.sendWatchTerminalPayload(sid, rid, payload)
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
		out, _, _, err := shell.ReadTerminalStream(ctx, rid, timeout, false, 0, terminalOutputChunkBytes, nil)
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
					"cursor": shell.ReaderCursor(rid),
				})
				c.sendWatchControlPayload(sid, rid, payload)
				return
			}
			continue
		}
		if out != "" {
			end := shell.ReaderCursor(rid)
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
		case !shell.HasMoreOutput(rid) && time.Since(lastFlush) >= terminalFlushMinGap:
			if !flush() {
				return
			}
		}
		info := shell.Info()
		if (info.Status != api.SessionRunning || shell.IsBufferClosed()) && !shell.HasMoreOutput(rid) {
			if !flush() {
				return
			}
			payload, err := json.Marshal(map[string]any{
				"type":   "terminal_done",
				"id":     sid,
				"cursor": shell.ReaderCursor(rid),
			})
			if err == nil {
				c.sendWatchControlPayload(sid, rid, payload)
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
	c.mu.Lock()
	if old, ok := c.watch[sid]; ok && old != nil && old.cancel != nil {
		old.cancel()
	}
	c.watch[sid] = &wsWatchEntry{cancel: cancel, rid: rid}
	if !c.sendControlPayload(ready) {
		delete(c.watch, sid)
		c.mu.Unlock()
		cancel()
		shell.UnregisterReader(rid)
		return errWSControlUnavailable
	}
	c.mu.Unlock()
	// The acknowledgement is enqueued before the output pump starts, so a
	// client can treat ready.cursor as the first possible byte of later frames.
	go c.runWatch(ctx, shell, sid, rid)
	return nil
}

func (c *uiWS) removeWatch(sid string) {
	c.mu.Lock()
	ent := c.watch[sid]
	delete(c.watch, sid)
	c.mu.Unlock()
	if ent != nil && ent.cancel != nil {
		ent.cancel()
	}
}

func (c *uiWS) removeAllWatches() {
	c.mu.Lock()
	var list []*wsWatchEntry
	for _, ent := range c.watch {
		list = append(list, ent)
	}
	c.watch = make(map[string]*wsWatchEntry)
	c.mu.Unlock()
	for _, ent := range list {
		if ent != nil && ent.cancel != nil {
			ent.cancel()
		}
	}
}
