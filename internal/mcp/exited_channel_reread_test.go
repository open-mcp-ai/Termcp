package mcp

import (
	"regexp"
	"strings"
	"testing"
)

// TestEveryReadOfAnExitedChannelRepeatsTheSameFacts pins issue #208.4.
//
// The complaint was that the second read of a finished channel changed from 200 to
// 204, and a caller looking only at the status line concluded the TERMINAL STATE had
// changed. It had not: 200 delivered the bytes, 204 says there are no new ones, and
// every fact about the ended program — Status, Exit-Code, Error-Code — repeats
// identically. That consistency is the contract, and it is what makes the terminal
// readable any number of times without a cursor dance.
func TestEveryReadOfAnExitedChannelRepeatsTheSameFacts(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "connect internal --argv sh -c 'echo OUT; exit 7'"})
	h := handleFrom(t, text)
	waitForShellToExit(t, s, shellIDFromHandle(t, s, h))

	facts := regexp.MustCompile(`(?m)^(Status|Exit-Code|Error-Code): .*$`)
	var seen []string
	for i := 0; i < 3; i++ {
		got := termText(t, s, map[string]any{"cmd": "read " + h})
		lines := facts.FindAllString(got, -1)
		joined := strings.Join(lines, "\n")
		if i == 0 {
			seen = lines
			if !strings.Contains(got, "OUT") {
				t.Fatalf("the first read did not deliver the bytes:\n%s", got)
			}
			continue
		}
		if joined != strings.Join(seen, "\n") {
			t.Errorf("read #%d changed the terminal facts:\nfirst:\n%s\nthis:\n%s", i+1, strings.Join(seen, "\n"), joined)
		}
		for _, want := range []string{"Exit-Code: 7", "Status: exited", "Error-Code: child_exit"} {
			if !strings.Contains(joined, want) {
				t.Errorf("read #%d lost %q:\n%s", i+1, want, got)
			}
		}
	}
}
