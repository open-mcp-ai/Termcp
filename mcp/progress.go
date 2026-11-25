package mcp

import (
	"context"
	"encoding/json"
	"sync"
)

// PROGRESS: notifications/progress, and the _meta.progressToken that asks for them.
//
// The protocol has two halves and both were missing here. A client puts a token in
// `_meta.progressToken` on a tools/call; the server then sends
// `notifications/progress` frames while the call runs, and the final result last. The
// token was parsed into CallToolParams.Meta and dropped, and the POST answer was
// always application/json — so even a server that had something to say had no way to
// say it before the answer.
//
// THE SINK IS A CONTEXT VALUE, and that is what keeps the mcp package free of any
// opinion about what progress means. A handler that knows its own progress calls
// ReportProgress; a handler that does not simply never calls it, and the transport
// still answers. Nothing here guesses at percentages.
//
// DROPPING IS DELIBERATE. Progress is advisory: a client that is not reading must not
// be able to block the tool it is watching. A full queue drops the frame and returns
// false, and the call carries on.

type progressKey struct{}

// ProgressFrame is one notifications/progress payload.
type ProgressFrame struct {
	// ProgressToken is echoed from the request's _meta, VERBATIM. It is a string or a
	// number chosen by the client, and normalising it would make the client unable to
	// match its own request.
	ProgressToken json.RawMessage `json:"progressToken"`
	// Progress is a number that must INCREASE; the specification is explicit that it
	// may go up and must not go down.
	Progress float64 `json:"progress"`
	// Total is the value progress would reach at completion, when it is known.
	Total *float64 `json:"total,omitempty"`
	// Message is a human-readable line. It is the only part a caller can read without
	// knowing what the numbers mean, so it carries the meaning when there is one.
	Message string `json:"message,omitempty"`
}

// progressSink is what a transport installs so a handler can report.
type progressSink struct {
	mu    sync.Mutex
	token json.RawMessage
	send  func(ProgressFrame)
	// last is the highest progress reported, because the specification forbids a
	// decrease: a handler that reports out of order (two goroutines, one slow) would
	// otherwise produce a stream a client is entitled to treat as a protocol error.
	last float64
	sent bool
}

// ReportProgress sends one progress frame for the call this context belongs to.
//
// It reports whether the frame was sent. False means one of: the caller sent no
// progressToken, the transport cannot stream, or the client is not keeping up — all
// of which mean the same thing to a handler (carry on), so they share one answer
// rather than three a caller would have to handle.
func ReportProgress(ctx context.Context, progress float64, total *float64, message string) bool {
	sink, ok := ctx.Value(progressKey{}).(*progressSink)
	if !ok || sink == nil {
		return false
	}
	sink.mu.Lock()
	if sink.sent && progress < sink.last {
		progress = sink.last
	}
	sink.last = progress
	sink.sent = true
	token := sink.token
	send := sink.send
	sink.mu.Unlock()
	if send == nil {
		return false
	}
	send(ProgressFrame{ProgressToken: token, Progress: progress, Total: total, Message: message})
	return true
}

// HasProgressSink reports whether this call asked to be watched. A handler uses it to
// decide whether producing progress is worth the work.
func HasProgressSink(ctx context.Context) bool {
	sink, ok := ctx.Value(progressKey{}).(*progressSink)
	return ok && sink != nil
}
