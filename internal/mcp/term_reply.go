package mcp

import (
	"fmt"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// The total byte cap has already been applied by Term, because it is a property
// of the reply rather than of one slot.
//
// In the structured-only mode the text content is omitted rather than sent empty:
// a JSON client reads structuredContent and one empty string is noise, while a
// client that renders text has already asked for something the caller declined to
// pay for. The tool result stays valid either way — content is allowed to be
// empty, and every field the caller needs is in structuredContent.
func toolResult(reply envelope.Reply) *mcpgo.CallToolResult {
	result := mcpgo.NewToolResultText(reply.Text())
	if reply.NoText {
		// A structured-only caller gets NO text content. `content` is allowed to be
		// empty in the tool-result schema, and leaving the rendering in would bill the
		// duplicate the caller explicitly declined.
		result.Content = nil
	}
	result.IsError = reply.IsError()
	/* The JSON twin is built only when it was asked for. Structured() renders every
	   slot a second time, and on a large reply that render is the cost the default
	   exists to avoid — cheaper to skip than to build and drop. */
	if reply.StructuredContent {
		structured := reply.Structured()
		if len(structured) == 1 {
			result.StructuredContent = structured[0]
		} else {
			// structuredContent must be a JSON object. A batch is several slots, so
			// it is wrapped rather than sent as a bare array, which clients reject
			// against the object-shaped result schema.
			result.StructuredContent = map[string]any{"slots": structured}
		}
	}
	return result
}

// applyTotalCap truncates the tail of the reply to the caller's total budget.
//
// A slot that crosses the cap is cut to exactly what fits and reports 206 with a
// Content-Range describing what it DID return; every slot after it is reduced to
// its headers. That is the same "fold, never drop" rule the per-slot windows use:
// the bytes are still in the log, and the range says where to continue.
func (t termReply) applyTotalCap(slots []envelope.Slot) []envelope.Slot {
	total := 0
	for i, slot := range slots {
		if total >= t.win.TotalBytesLimit {
			// AN EMPTY 206 IS NOT AN ANSWER. The statement RAN — this runs after
			// execution — and its output was withheld because the reply had already
			// spent the caller's total. The old reply was a slot with headers, code
			// 206 and no body, which is what "this statement had nothing to say" looks
			// like; the caller had no way to tell that a statement it asked for had
			// produced output it never saw.
			slot.Body = ""
			if slot.Code < 400 {
				slot.Code = 206
			}
			if slot.Headers == nil {
				slot.Headers = map[string]string{}
			}
			slot.Headers["Dropped"] = fmt.Sprintf(
				"this statement ran and its output was withheld: the reply had spent total_bytes_limit (%d). Run it alone, or raise total_bytes_limit",
				t.win.TotalBytesLimit)
			slots[i] = slot
			continue
		}
		remaining := t.win.TotalBytesLimit - total
		if len(slot.Body) > remaining {
			// Cut, don't just truncate: Content-Range has to describe the bytes the
			// body still carries, or the caller's next read starts past the gap.
			slot = slot.Cut(remaining)
			if slot.Code < 400 {
				slot.Code = 206
			}
		}
		total += len(slot.Body)
		slots[i] = slot
	}
	return slots
}

// termSlotResult wraps one slot as a tool result.
func termSlotResult(slot envelope.Slot) *mcpgo.CallToolResult {
	return toolResult(envelope.Reply{Slots: []envelope.Slot{slot}})
}
