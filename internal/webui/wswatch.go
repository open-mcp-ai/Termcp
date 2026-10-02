package webui

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func (c *uiWS) sendTerminalPayload(payload []byte) {
	select {
	case c.send <- payload:
	case <-c.ctx.Done():
	default:
	}
}

func (c *uiWS) sendTerminalDonePayload(payload []byte) {
	select {
	case c.send <- payload:
	case <-c.ctx.Done():
	default:
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
	for {
		if ctx.Err() != nil {
			return
		}
		out, err := shell.ReadTerminalStream(ctx, rid, 250*time.Millisecond, false, 0, terminalOutputChunkBytes)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// EOF: buffer closed (shell exited but session kept alive for multiplexing).
			if err == io.EOF {
				payload, _ := json.Marshal(map[string]string{"type": "terminal_done", "id": sid})
				c.sendTerminalDonePayload(payload)
				return
			}
			continue
		}
		if out != "" {
			// Terminal bytes travel as a plain JSON string; encoding/json escapes
			// whatever it must. base64 would only add a layer the client undoes.
			payload, err := json.Marshal(map[string]string{"type": "terminal", "id": sid, "d": out})
			if err != nil {
				continue
			}
			c.sendTerminalPayload(payload)
		}
		info := shell.Info()
		if (info.Status != api.SessionRunning || shell.IsBufferClosed()) && !shell.HasMoreOutput(rid) {
			payload, err := json.Marshal(map[string]string{"type": "terminal_done", "id": sid})
			if err == nil {
				c.sendTerminalDonePayload(payload)
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
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if old, ok := c.watch[sid]; ok && old != nil && old.cancel != nil {
		old.cancel()
	}
	c.watch[sid] = &wsWatchEntry{cancel: cancel, rid: rid}
	c.mu.Unlock()
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
