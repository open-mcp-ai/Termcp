package mcp

// Progress: the channel and the token (issue #327).
//
// THE PROTOCOL HALF IS SMALL AND THE CHANNEL IS THE WORK. `_meta.progressToken` was
// parsed and thrown away, and a POST always answered application/json — so even a
// tool that wanted to report progress had nowhere to put a frame: it could not ride
// the response (the response was one JSON document, written at the end) and it could
// not be a notification, because a notification goes to the GET stream and the client
// was waiting on its POST.
//
// So a POST may now answer with text/event-stream. When the client asks for one AND
// sends a progressToken, the request runs while the response is open and the tool's
// progress frames are written into it, followed by the result. That is the per-request
// sink: one call, one stream, in order.

import (
	"context"
	"encoding/json"
	"sync"
)

// progressSink collects frames a tool emits while one request is running.
//
// A channel rather than a callback because the writer is a tool goroutine and the
// reader is the HTTP handler: a callback would make the tool's pace the network's
// pace, and a slow client would then slow the work it is watching.
type progressSink struct {
	mu     sync.Mutex
	frames [][]byte
	closed bool
}

// emit queues one frame. Frames after close are dropped: the response is gone, and a
// tool that keeps working (a long read, a command still running) must not be turned
// into a leak by a client that hung up.
func (p *progressSink) emit(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.frames = append(p.frames, frame)
}

// drain hands over everything queued and marks the sink closed.
func (p *progressSink) drain() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.frames
	p.frames = nil
	p.closed = true
	return out
}

// peek takes what is queued without closing.
func (p *progressSink) peek() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.frames
	p.frames = nil
	return out
}

type progressKey struct{}

// WithProgressSink puts a sink on a request's context.
//
// The TOKEN travels with it, because the frame's params must echo the client's own
// spelling of it: a client that sent `"tok-1"` and reads back `1` has no way to know
// the frame is about its request.
func WithProgressSink(ctx context.Context, sink *progressSink, token ProgressToken) context.Context {
	return context.WithValue(ctx, progressKey{}, progressContext{sink: sink, token: token})
}

type progressContext struct {
	sink  *progressSink
	token ProgressToken
}

// ReportProgress emits one notifications/progress frame for the request being served.
//
// A NO-OP WHEN NOBODY ASKED, which is the common case and must stay free: a tool
// calls this unconditionally and pays one context lookup when the client sent no
// progressToken. That is the only way the callers stay readable — the alternative is
// every tool asking "did the client want progress" and getting it wrong somewhere.
func ReportProgress(ctx context.Context, progress float64, total *float64, message string) {
	pc, ok := ctx.Value(progressKey{}).(progressContext)
	if !ok || pc.sink == nil {
		return
	}
	frame, err := json.Marshal(NewProgressNotification(pc.token, progress, total, message))
	if err != nil {
		return
	}
	pc.sink.emit(frame)
}

// HasProgress reports whether the caller asked to be kept informed.
func HasProgress(ctx context.Context) bool {
	pc, ok := ctx.Value(progressKey{}).(progressContext)
	return ok && pc.sink != nil
}
