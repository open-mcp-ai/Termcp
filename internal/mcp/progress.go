package mcp

import (
	"context"
	"time"

	"github.com/open-mcp-ai/termcp/mcp"
)

// THE PRODUCER SIDE OF PROGRESS: something has to call ReportProgress.
//
// The transport can now carry a notifications/progress frame (mcp/progress.go), but a
// frame that nobody sends is a channel with no writer. This file is the writer, and it
// is deliberately ONE producer for the whole tool rather than one per directive.
//
// WHY A TICKER AND NOT A PERCENTAGE. A `term` call is a command line: it may read a
// channel for 20 seconds, connect over SSH, move a file, or all three. There is no
// single denominator to divide by, and inventing one — "50% of the read" — would be a
// number that is not about the caller's job. What is TRUE for every long call is
// elapsed time against the budget the caller itself set with `timeout`, and that is
// what the frame reports.
//
// IT STARTS ONLY WHEN ASKED. HasProgressSink is false for every call that sent no
// `_meta.progressToken`, which is all of them by default, so the ordinary path pays a
// boolean and no goroutine.
//
// THE FIRST FRAME IS NOT IMMEDIATE. Reporting at t=0 would tell the client "0 seconds"
// on every call, including the ones that finish in 4ms, and a client that then renders
// a progress bar for a call that already returned is worse than one that never did.

// progressInterval is how often a watched call reports. One second is the granularity a
// person reads; a shorter interval spends frames on numbers nobody can react to, and a
// longer one leaves a 30-second read silent long enough to look hung.
const progressInterval = time.Second

// withProgressReporting runs fn with a context that reports elapsed time while fn is
// working. It is a no-op wrapper when the call did not ask to be watched.
func withProgressReporting(ctx context.Context, budget time.Duration, fn func(context.Context) termReply) termReply {
	if !mcp.HasProgressSink(ctx) {
		return fn(ctx)
	}
	done := make(chan struct{})
	go func() {
		start := time.Now()
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				elapsed := time.Since(start).Seconds()
				// The total is the caller's own window, when it named one: a percentage a
				// client can draw is only honest if the denominator is the budget rather
				// than a guess about the work.
				var total *float64
				if budget > 0 {
					secs := budget.Seconds()
					total = &secs
				}
				mcp.ReportProgress(ctx, elapsed, total, progressMessage(elapsed, budget))
			}
		}
	}()
	reply := fn(ctx)
	close(done)
	return reply
}

// progressMessage is the sentence a client may show a person. It says what the call is
// doing (waiting) and for how long, because a bare number in a terminal-adjacent tool
// reads as "bytes" to anyone who has used this one.
func progressMessage(elapsed float64, budget time.Duration) string {
	if budget > 0 {
		return "waiting: " + time.Duration(elapsed*float64(time.Second)).Round(time.Second).String() +
			" of " + budget.Round(time.Second).String()
	}
	return "waiting: " + time.Duration(elapsed*float64(time.Second)).Round(time.Second).String()
}
