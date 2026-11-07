package mcp

import (
	"strings"
	"testing"
)

// TestCloseChannelDoesNotTearDownSession pins issue P0-1 from the rc3 report:
// `close <handle> --force` used to terminate the entire session because `--force`
// was treated as a statement about what to close rather than how.
func TestCloseChannelDoesNotTearDownSession(t *testing.T) {
	s := newTestServer(t)

	// Session with two channels: a primary pty, and a secondary pipe.
	text := termText(t, s, map[string]any{"cmd": "connect internal --name s1"})
	h1 := handleFrom(t, text)
	sid := strings.SplitN(h1, ":", 2)[0]

	text2 := termText(t, s, map[string]any{"cmd": "open " + sid + " --name s2"})
	h2 := handleFrom(t, text2)

	// Write something to s2 so it has an output record.
	//
	// WAIT FOR THE WRITE TO LAND, rather than sleeping or reading immediately. `run`
	// returns as soon as the bytes are in the channel, and a shell that is still
	// starting (a heavy profile, a cold page cache, a loaded CI runner) echoes them
	// later — an immediate read then finds only the shell's banner and the test fails
	// over the scheduler, not over the behaviour it is pinning.
	termText(t, s, map[string]any{"cmd": "run " + h2 + " $1", "args": []any{"echo IN_S2"}, "timeout": 2})
	waitForContent(t, s, h2, "IN_S2")

	// 1. `close <channel> --force` must be REFUSED (400), not silently destroy s1.
	got := termText(t, s, map[string]any{"cmd": "close " + h2 + " --force"})
	if !strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("close channel with --force must be refused 400, got:\n%s", got)
	}
	if !strings.Contains(got, "is gone") {
		t.Logf("ACTUAL ERROR BODY:\n%s", got)
		t.Errorf("error should explain --force is gone")
	}

	// 2. s1 must still be fully alive after that refusal.
	after := termText(t, s, map[string]any{"cmd": "run " + h1 + " $1", "args": []any{"echo STILL_ALIVE"}, "timeout": 2})
	if !strings.Contains(after, "STILL_ALIVE") {
		t.Fatalf("s1 was destroyed by the refused close on s2:\n%s", after)
	}

	// 3. `close <channel> --grace 3` must also be refused.
	got = termText(t, s, map[string]any{"cmd": "close " + h2 + " --grace 3"})
	if !strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("close channel with --grace must be refused 400, got:\n%s", got)
	}

	// 4. Clean close of the channel: returns 204.
	got = termText(t, s, map[string]any{"cmd": "close " + h2})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("clean close should succeed 204, got:\n%s", got)
	}

	// 5. s1 is STILL alive.
	after2 := termText(t, s, map[string]any{"cmd": "run " + h1 + " $1", "args": []any{"echo STILL_ALIVE_2"}, "timeout": 2})
	if !strings.Contains(after2, "STILL_ALIVE_2") {
		t.Fatalf("s1 died when s2 was cleanly closed:\n%s", after2)
	}

	// 6. ARCHIVE: s2's output is still readable via read, NOT 404!
	//
	// ASK FOR THE LINE, NOT FOR A WINDOW THAT HAPPENS TO CONTAIN IT. `--tail 5`
	// assumed IN_S2 was still among the last five lines, but the shell paints its
	// banner AFTER running the command — how many lines follow IN_S2 depends on how
	// fast that shell started, so a loaded machine pushes it out of the window while
	// the archive itself is intact. `--grep` filters before the window, so the
	// assertion is about the archived bytes rather than about the banner's length.
	readBack := termText(t, s, map[string]any{"cmd": "read " + h2 + " --grep IN_S2"})
	if strings.HasPrefix(readBack, "TERM/1 404") {
		t.Fatalf("closed channel output was destroyed (got 404):\n%s", readBack)
	}
	if !strings.Contains(readBack, "IN_S2") {
		t.Errorf("closed channel output missing IN_S2:\n%s", readBack)
	}

	// Cleanup.
	termText(t, s, map[string]any{"cmd": "rm " + sid + " --force"})
}
