package mcp

import (
	"strings"
	"testing"
)

// A sub-MCP's channel is the same channel the caller reads, so EVERYTHING a sub-MCP
// emits arrives as ordinary channel data. That is the whole rule, and it is
// deliberately the absence of a filter:
//
//   - the caller is a protocol participant. It receives a request, decides, and
//     writes the answer back — the same three steps it already performs for any
//     prompt, because the reply travels on the channel it is already holding;
//   - a method it does not implement is answered by IT, with method-not-found. It
//     knows what it implements, and its answer is better than a guess made here;
//   - `tasks/*`, `roots/list` and `elicitation/create` are requests BETWEEN the
//     caller and the sub-MCP. termcp moves the frame and does not read it.
//
// This replaced two earlier versions that got the direction wrong. The first
// recognised one method and let the rest fall through by accident; the second
// refused everything it did not recognise. Both treated a frame as termcp's to
// judge, and a refusal is a question termcp has no standing to answer.

// TestASubMCPRequestReachesTheCaller is the rule stated positively: a request from a
// sub-MCP is the caller's data, whatever method it names.
func TestASubMCPRequestReachesTheCaller(t *testing.T) {
	for _, method := range []string{
		"elicitation/create",
		"roots/list",
		"sampling/createMessage",
		"tasks/get",
		"vendor/thing",
	} {
		t.Run(method, func(t *testing.T) {
			s := newTestServer(t)
			script := writeScript(t, `
printf '{"jsonrpc":"2.0","id":42,"method":"`+method+`","params":{}}\n'
read answer
printf 'GOT:%s\n' "$answer"
`)
			h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"}))
			defer termText(t, s, map[string]any{"cmd": "close " + h})

			got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 3})

			// The request itself is the caller's data.
			if !strings.Contains(got, method) {
				t.Fatalf("the request did not reach the caller:\n%s", got)
			}
			// And termcp did not answer it on the caller's behalf: no error frame was
			// written back, so the sub-MCP is still waiting for a real answer.
			if strings.Contains(got, "-32601") || strings.Contains(got, "method not found") {
				t.Errorf("termcp answered a question the caller should have seen:\n%s", got)
			}
		})
	}
}

// TestATaskFrameIsOrdinaryContent is the case that makes the rule matter. A task
// handle is not a protocol event termcp participates in: the sub-MCP answers the
// caller's tools/call directly and the handle rides inside that answer. termcp moves
// the frame and must not rewrite it — the caller parses those bytes.
func TestATaskFrameIsOrdinaryContent(t *testing.T) {
	s := newTestServer(t)

	script := writeScript(t, `
printf '{"jsonrpc":"2.0","id":1,"result":{"task":{"taskId":"t-9","status":"working","pollInterval":500}}}\n'
read answer
printf 'GOT:%s\n' "$answer"
`)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 3})
	if !strings.Contains(got, `"taskId":"t-9"`) {
		t.Fatalf("the task handle did not reach the caller intact:\n%s", got)
	}
	if strings.Contains(got, "[this frame") {
		t.Errorf("termcp annotated a frame the caller owns:\n%s", got)
	}
}

// TestANotificationIsNotAnswered: a notification has no id and nobody is waiting.
// Answering one would put an unsolicited frame on the channel, which the caller
// would read as the answer to its own call.
func TestANotificationIsNotAnswered(t *testing.T) {
	s := newTestServer(t)

	script := writeScript(t, `
printf '{"jsonrpc":"2.0","method":"notifications/initialized"}\n'
printf 'STILL_ALIVE\n'
read _
`)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	first := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 3})
	if strings.Contains(first, `"error"`) {
		t.Errorf("a notification was answered with an error:\n%s", first)
	}
	if !strings.Contains(first, "notifications/initialized") {
		t.Errorf("the notification did not pass through as ordinary traffic:\n%s", first)
	}

	// The channel still works: nothing was swallowed by an answer nobody asked for.
	second := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 3})
	if !strings.Contains(second, "STILL_ALIVE") {
		t.Errorf("the frame after the notification is missing:\n%s", second)
	}
}

// TestASubMCPResponseIsNotAnswered: a response answers something we asked. Treating
// it as a request would answer an answer, which is how a JSON-RPC conversation
// desynchronises.
func TestASubMCPResponseIsNotAnswered(t *testing.T) {
	s := newTestServer(t)

	script := writeScript(t, `
printf '{"jsonrpc":"2.0","id":3,"result":{"ok":true}}\n'
printf 'AFTER\n'
read _
`)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 3})
	if strings.Contains(got, `"error"`) {
		t.Errorf("a response was answered with an error:\n%s", got)
	}
	if !strings.Contains(got, `"ok"`) {
		t.Errorf("the response did not pass through as ordinary traffic:\n%s", got)
	}
}
