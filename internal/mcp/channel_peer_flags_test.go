package mcp

import (
	"strings"
	"testing"
)

// A CHANNEL READ DOES NOT TAKE PEER FLAGS: --since and --at belong to peer frame
// reads, where --at slices one frame's bytes. On a shell or pipe channel they name
// nothing at all.
//
// Accepting and ignoring them is the dangerous path (issue #159): the reply was a
// confident answer about a different question, and the channel's cursor had already
// moved past the bytes the caller asked to filter. Refusing must happen BEFORE the
// read, so a cursor nobody asked to advance stays where it was.
func TestChannelReadRefusesPeerOnlyFlags(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, `sh -c 'echo line1; echo line2; sleep 5'`)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, flag := range []string{"--since 1", "--at 1", "--bytes 10"} {
		name := strings.SplitN(flag, " ", 2)[0]
		t.Run(name, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": "read " + h + " " + flag})
			if !statusLineIs(got, "400") {
				t.Fatalf("a channel read with %s must answer 400 Bad Request, got:\n%s", flag, got)
			}
			if !strings.Contains(got, name) {
				t.Errorf("the refusal must name the flag it refused (%s):\n%s", name, got)
			}
		})
	}

	// THE CURSOR DID NOT MOVE: the two rejected calls consumed nothing, so a plain
	// read still returns the stream from its start. Without this the test would pass
	// even if the refusal ran after the bytes had been taken.
	plain := termText(t, s, map[string]any{"cmd": "read " + h})
	if !strings.Contains(plain, "line1") {
		t.Fatalf("a rejected flag consumed the channel's bytes; a plain read must still see the start:\n%s", plain)
	}
}
