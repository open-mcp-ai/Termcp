package webui

import (
	"sync"
	"sync/atomic"
)

// sessionListHub fans out coalesced signals to SSE subscribers when the session registry changes.
//
// The client table is a sync.Map and the id counter is atomic: each subscriber is
// an independent entry, so there is nothing for a lock to keep consistent.
type sessionListHub struct {
	nextID  atomic.Uint64
	clients sync.Map // id uint64 -> chan struct{}
}

func newSessionListHub() *sessionListHub {
	return &sessionListHub{}
}

func (h *sessionListHub) register() (id uint64, signal <-chan struct{}, unregister func()) {
	ch := make(chan struct{}, 1)
	id = h.nextID.Add(1)
	h.clients.Store(id, ch)
	return id, ch, func() {
		h.clients.Delete(id)
	}
}

func (h *sessionListHub) broadcast() {
	h.clients.Range(func(_, v any) bool {
		ch, ok := v.(chan struct{})
		if !ok {
			return true
		}
		// Buffered(1): if a wake is already pending, a second default would drop this notify
		// and the client might never pull ListAll() again — drain then enqueue one coalesced wake.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- struct{}{}:
		default:
		}
		return true
	})
}

func (h *Handler) sessionHub() *sessionListHub {
	if h.sessHub == nil {
		h.sessHub = newSessionListHub()
	}
	return h.sessHub
}
