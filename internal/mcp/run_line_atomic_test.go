package mcp

import (
	"strings"
	"sync"
	"testing"
)

// A LINE IS ONE UNIT, EVEN UNDER CONCURRENCY (issue #212).
//
// `run` writes the text and then the enter that ends it, as two calls into the shell
// layer. Nothing kept a second `run` from landing between them, so two concurrent runs
// on one channel produced `{"id":1}{"id":2}\n\n` — neither two lines nor one valid
// line. A line-protocol peer reads that as corruption: the stdio MCP server in the
// report answered `invalid character '{' after object key:value pair` and exited, which
// is how a transport race became a dead extension.
//
// THE PEER HERE IS A LINE READER, because that is the shape that can tell the
// difference. `cat` cannot: it echoes whatever arrives, so a stitched stream looks
// exactly like a correct one.
func TestConcurrentRunsDoNotStitchLines(t *testing.T) {
	s := newTestServer(t)
	// Each line is parsed; a line that does not parse ends the process with a message
	// naming the corruption, which is what the test asserts on.
	script := writeScript(t, lineReaderBody)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	const rounds = 6
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			termText(t, s, map[string]any{
				"cmd":     "run " + h + " $1",
				"args":    []any{`{"jsonrpc":"2.0","id":` + string(rune('0'+n)) + `,"method":"ping"}`},
				"timeout": 3,
			})
		}(i)
	}
	wg.Wait()

	out := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 60", "timeout": 0})
	if strings.Contains(out, "corrupt") || strings.Contains(out, "exited") {
		t.Fatalf("concurrent runs stitched two lines into one:\n%s", out)
	}
	// Every frame must have been answered: the peer answers each parsed line, so a
	// count below rounds means a line was lost or merged.
	if got := strings.Count(out, `"ok":true`); got < rounds {
		t.Fatalf("the peer answered %d of %d frames; a merged line is a lost frame:\n%s", got, rounds, out)
	}
}

// lineReaderBody is a peer that reads lines, parses each as JSON, and answers — and
// reports corruption loudly instead of guessing, which is what makes a stitched stream
// visible at all.
//
// sh rather than a language runtime: the only thing the test needs is "did a line
// arrive whole", and sh is what the other pipe probes in this package already use.
const lineReaderBody = `while IFS= read -r line; do
  case "$line" in
    *'}{'*) echo "corrupt: two objects on one line"; exit 1 ;;
    *'"id"'*) echo '{"ok":true}' ;;
  esac
done`
