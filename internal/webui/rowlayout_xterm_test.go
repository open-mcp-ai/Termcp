package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The row model is a reimplementation of a terminal, so the only honest way to
// check it is against a real one. xterm.js is already in this repository (it is
// what the Web UI draws the terminal with), it runs under node, and it produces
// the exact rows the rail is indexed against — so the corpus below is replayed
// through both and the two are compared byte for byte.
//
// What is compared is the thing the rail rests on: which row each byte ended up
// on. A mark is a byte offset, the rail asks which row holds it, and a model that
// is right about the text but a row off puts the cell beside the wrong line. The
// final row count and cursor row are compared too, which is what catches a model
// that tracks the cursor correctly but keeps rows the terminal has trimmed, or
// trims where the terminal does not.
//
// node is not required to build or run the rest of the suite, so the test skips
// when it is missing (CI images without node). The model is then still covered by
// the explicit expectations in shellrail_test.go — those say what the answer is,
// this says the answer agrees with xterm.

// xtermCorpus is the terminal output the two are compared on: shapes a shell
// actually writes, plus the control sequences that decide where the text goes.
//
// Each case is the list of writes the SSH pipe could deliver, because the model
// carries parser state across reads and a bug there is invisible when a case is
// handed over whole.
func xtermCorpus() map[string][]string {
	cases := map[string][]string{
		// A transcript: prompt, command, its echo, output, next prompt.
		"prompt": {"user@host:~$ ", "echo hello\r\n", "hello\r\n", "user@host:~$ "},

		// Wrapping: a line longer than the screen, twice.
		"wrap": {"$ printf 'X%.0s' $(seq 1 200)\r\n" + strings.Repeat("X", 200) + "\r\n$ "},

		// Wide characters, which take two columns and wrap two characters early if
		// they are counted as one.
		"cjk": {"$ echo ", strings.Repeat("汉字测试", 20), "\r\n$ "},

		// A progress bar repainting one row through carriage returns.
		"progress": {"$ download\r", "\r[  0%]", "\r[ 50%]", "\r[100%]", "\r\ndone\r\n$ "},

		// Colour, which the model parses and does not apply.
		"sgr": {"$ ls --color\r\n\x1b[01;34mdir\x1b[0m  \x1b[01;32m.sh\x1b[0m\r\n$ "},

		// The shell repainting the line as it is typed (completion, broadened paste).
		"repaint": {"$ ec", "\r\x1b[K$ echo abcdef", "\r\nabcdef\r\n$ "},

		// `clear`: erase saved lines, then home and erase the screen. This is the
		// sequence that renumbers a whole session's rows.
		"clear": {"$ clear\r\n\x1b[3J\x1b[H\x1b[2J", "$ echo after\r\nafter\r\n$ "},

		// More than one screen of output, then a clear: the rows above the screen
		// are trimmed and every surviving row number slides down.
		"clear_scrolled": func() []string {
			out := make([]string, 0, 32)
			for i := 1; i <= 30; i++ {
				out = append(out, fmt.Sprintf("LINE_%02d\r\n", i))
			}
			out = append(out, "\x1b[3J\x1b[H\x1b[2J", "prompt$ ")
			return out
		}(),

		// Enough output to pass the retention limit on a small screen, where the
		// oldest row falls off and every row number moves down.
		"trim": func() []string {
			out := []string{}
			for i := 0; i < 400; i++ {
				out = append(out, fmt.Sprintf("L%03d\r\n", i))
			}
			out = append(out, "$ ")
			return out
		}(),

		// Erase to end of line, the shell's inline prompt.
		"erase_line": {"$ echo verylongname11111", "\r\x1b[K$ ls\r\nfile\r\n$ "},

		// Backspace over a partially typed line.
		"backspace": {"$ read pw\r\n", "ab\b\b\b\bsecret\r\n", "$ "},

		// Tabs, which advance to the next stop and wrap past the right edge.
		"tabs": {"$ ls\t\tfoo\r\n", "a\tb\tc\r\n$ "},

		// Cursor addressing without the alternate screen.
		"addressed": {"$ printf abc\r\n", "\x1b[1;1H\x1b[K$ printf XYZ\r\n", "XYZ\r\n$ "},

		// Blank lines: a line break and nothing else is still a row.
		"blanks": {"$ cat\r\n\r\n\r\n$ "},

		// A full-screen program: its bytes are not the transcript's, and the rows
		// that come back are the ones that were there before it started.
		"alt_screen": {"before\r\n", "\x1b[?1049h\x1b[H\x1b[2J", strings.Repeat("~ full screen row\r\n", 20), "\x1b[?1049l", "after\r\n$ "},

		// A sequence and a rune split across reads, including one naming a row.
		"split_reads": {"$ ", "\x1b[", "2;5", "H", "中", "文", "\r\n$ "},
	}

	// A deterministic spread of sessions, so the corpus is not only the shapes
	// someone thought to name: prompts, commands, wrapping output, wide
	// characters, clears and full-screen programs, in pseudo-random order, written
	// in chunks of one to several bytes.
	lines := []string{"ls -la", "echo hello world", "grep -rn TODO src | wc -l", "make build", "git status --short", "cat README.md", "printf '%s\\n' a b c"}
	prompts := []string{"user@host:~$ ", "root@box:/etc# ", "(venv) dev@ci:~/app$ "}
	wide := []string{"文件", "目录", "测试", "系统", "配置", "日志", "错误", "完成"}
	sgr := []string{"\x1b[0m", "\x1b[1m", "\x1b[31m", "\x1b[32m", "\x1b[36m", "\x1b[39m", "\x1b[49m", "\x1b[7m", "\x1b[27m"}
	next := xtermRand(1)
	for c := 0; c < 60; c++ {
		var writes []string
		var b strings.Builder
		flush := func() {
			if b.Len() > 0 {
				writes = append(writes, b.String())
				b.Reset()
			}
		}
		p := prompts[next(len(prompts))]
		for s := 0; s < 1+next(4); s++ {
			cmd := lines[next(len(lines))]
			b.WriteString(p)
			// Sometimes the line is repainted as it is typed, which is the case the
			// model has to read as a replacement rather than an append.
			if next(3) == 0 {
				part := cmd[:1+next(len(cmd)-1)]
				b.WriteString(part)
				flush()
				b.WriteString("\r\x1b[K" + p + cmd)
			} else {
				b.WriteString(cmd)
			}
			b.WriteString("\r\n")
			flush()
			switch next(10) {
			case 0, 1, 2: // several lines of output
				for i := 0; i < 1+next(8); i++ {
					if next(2) == 0 {
						b.WriteString(sgr[next(len(sgr))])
					}
					b.WriteString(lines[next(len(lines))])
					if next(2) == 0 {
						b.WriteString(sgr[next(len(sgr))])
					}
					b.WriteString("\r\n")
				}
				flush()
			case 3: // output that wraps
				b.WriteString(strings.Repeat("X", 60+next(300)) + "\r\n")
				flush()
			case 4: // wide characters
				for i := 0; i < 10+next(30); i++ {
					b.WriteString(wide[next(len(wide))])
				}
				b.WriteString("\r\n")
				flush()
			case 5: // a progress bar on one row
				b.WriteString("downloading")
				for i := 0; i <= 10; i++ {
					flush()
					b.WriteString("\r[" + strings.Repeat("#", i) + strings.Repeat(".", 10-i) + "]")
				}
				b.WriteString("\r\ndone\r\n")
				flush()
			case 6: // a screen clear
				if next(2) == 0 {
					b.WriteString("\x1b[H\x1b[2J")
				} else {
					b.WriteString("\x1b[3J\x1b[H\x1b[2J")
				}
				flush()
				b.WriteString(p + "after\r\n")
				flush()
			case 7: // a full-screen program
				b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
				flush()
				for i := 0; i < 3+next(10); i++ {
					b.WriteString("~ full screen line\r\n")
				}
				flush()
				b.WriteString("\x1b[?1049l")
				flush()
			}
		}
		b.WriteString(p)
		flush()
		cases[fmt.Sprintf("gen%02d", c)] = writes
	}
	return cases
}

// xtermRand is a small deterministic generator: the corpus has to be the same on
// every run, or a failure cannot be reproduced from its own name.
func xtermRand(seed uint32) func(n int) int {
	s := seed
	return func(n int) int {
		if n <= 0 {
			return 0
		}
		s = s*1664525 + 1013904223
		return int((s >> 16) % uint32(n))
	}
}

// xtermProbe is what the node side reports for one case.
type xtermProbe struct {
	// Rows is the row each byte landed on, in order: rows[i] is the cursor's
	// buffer row after byte i was consumed.
	Rows []int `json:"rows"`
	// Text is what the terminal shows on each row at the end.
	Text []string `json:"text"`
	// RowCount and Cursor are the final buffer size and cursor row.
	RowCount int `json:"rowCount"`
	Cursor   int `json:"cursor"`
}

// TestRowLayoutAgreesWithXterm replays a corpus of shell output through the model
// and through xterm.js, and requires them to place every byte on the same row.
func TestRowLayoutAgreesWithXterm(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot compare the row model with xterm")
	}
	xterm, err := readAsset("static/xterm/xterm.js")
	if err != nil {
		t.Skipf("the bundled xterm is not readable: %v", err)
	}

	dir := t.TempDir()
	// Both files live in the test's own directory: the node side is told where to
	// read and where to write, so nothing depends on a shared path or on the
	// working directory.
	casesPath := filepath.Join(dir, "cases.json")
	outPath := filepath.Join(dir, "probe.json")
	xtermPath := filepath.Join(dir, "xterm.js")
	if err := os.WriteFile(xtermPath, []byte(xterm), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := xtermCorpus()
	// The log is what the model is fed, and it is assembled here rather than on the
	// node side so both sides consume byte-for-byte the same stream.
	logs := map[string]string{}
	probeIn := map[string][]string{}
	for name, writes := range cases {
		var b strings.Builder
		for _, w := range writes {
			// A bare newline is what the log holds for a line ending; the terminal
			// the log came from had it translated to CRLF already, so the corpus
			// writes CRLF explicitly and this does not rewrite anything.
			b.WriteString(w)
		}
		logs[name] = b.String()
		probeIn[name] = writes
	}
	blob, err := json.Marshal(probeIn)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casesPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	const script = `
global.window = global; global.self = global;
const fs = require('fs');
const { Terminal } = require(process.argv[2]);
const cases = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
const ROWS = 24, COLS = 80;
const names = Object.keys(cases).sort();
const out = {};
let idx = 0;
function next() {
  if (idx >= names.length) {
    fs.writeFileSync(process.argv[4], JSON.stringify(out));
    return;
  }
  const name = names[idx++];
  // The chunks are re-encoded exactly as the model will see them: a write that is
  // not valid UTF-8 cannot come out of the log, so the byte boundaries the two are
  // compared on have to be the encoded ones.
  const chunks = cases[name].map(c => Buffer.from(c, 'utf8'));
  const t = new Terminal({ rows: ROWS, cols: COLS, scrollback: 100000 });
  const rows = [];
  let ci = 0, bi = 0;
  const step = () => {
    if (ci >= chunks.length) {
      const b = t.buffer.active;
      out[name] = {
        rows,
        text: Array.from({ length: b.length }, (_, k) => { const l = b.getLine(k); return l ? l.translateToString(true) : ''; }),
        rowCount: b.length,
        cursor: b.baseY + b.cursorY
      };
      t.dispose();
      setImmediate(next);
      return;
    }
    const chunk = chunks[ci];
    if (bi >= chunk.length) { ci++; bi = 0; step(); return; }
    t.write(chunk.subarray(bi, bi + 1), () => {
      const b = t.buffer.active;
      rows.push(b.baseY + b.cursorY);
      bi++;
      step();
    });
  };
  step();
}
next();
`
	scriptPath := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, scriptPath, xtermPath, casesPath, outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the xterm probe failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var probes map[string]xtermProbe
	if err := json.Unmarshal(raw, &probes); err != nil {
		t.Fatal(err)
	}

	// The comparison walks the log byte by byte, so the model is exercised the way
	// the endpoint uses it — a long log read in pieces, not one buffer.
	names := make([]string, 0, len(logs))
	for name := range logs {
		names = append(names, name)
	}
	sort.Strings(names)
	checked, mismatched := 0, 0
	for _, name := range names {
		log := logs[name]
		probe, ok := probes[name]
		if !ok {
			t.Fatalf("%s: the xterm probe reported nothing", name)
		}
		if got := len(probe.Rows); got != len(log) {
			t.Fatalf("%s: xterm reported %d byte positions for a %d-byte log", name, got, len(log))
		}

		data := []byte(log)
		g := newRowLayout(80, 24)
		for i := 0; i < len(data); i++ {
			// A full-screen program's bytes are deliberately not recorded: the model
			// keeps the transcript's rows while xterm's cursor is on the program's own
			// screen, so the two describe different buffers until it exits. That is
			// pinned by TestShellRailAltScreenLeavesTheTranscriptAlone; here it is
			// skipped so the rest of the case is still compared.
			wasAlt := g.alt
			g.feed(int64(i), data[i:i+1])
			if wasAlt || g.alt {
				continue
			}
			checked++
			if g.row != probe.Rows[i] {
				mismatched++
				if mismatched <= 10 {
					lo := i - 12
					if lo < 0 {
						lo = 0
					}
					hi := i + 12
					if hi > len(log) {
						hi = len(log)
					}
					t.Errorf("%s: after byte %d the model is on row %d and xterm on row %d (context %q)",
						name, i, g.row, probe.Rows[i], log[lo:hi])
				}
			}
		}

		// The buffer the two end up with has to be the same size once the log has
		// filled the screen: a model that keeps rows the terminal trimmed reports rows
		// that do not exist, and every cell below the trim point is then a row out.
		//
		// A log shorter than one screen is the one case they legitimately differ: the
		// terminal pads its buffer to a full screen of blank rows, while the model only
		// holds the rows the log reached. The rail indexes rows on screen, and a row
		// holding no bytes draws no cell either way, so the padding is not something the
		// model has to reproduce — but it must not be the reason the counts differ past
		// a screenful.
		if probe.RowCount > 24 {
			if g.rowCount() != probe.RowCount {
				t.Errorf("%s: the model holds %d rows, xterm holds %d", name, g.rowCount(), probe.RowCount)
			}
		} else if g.rowCount() > probe.RowCount {
			t.Errorf("%s: the model holds %d rows, more than xterm's %d", name, g.rowCount(), probe.RowCount)
		}
		if g.row != probe.Cursor {
			t.Errorf("%s: the model ends on row %d, xterm on row %d", name, g.row, probe.Cursor)
		}

		// And the rows hold text consistent with the bytes the model assigned to each
		// row: the mapping is only right if it is right about the content too.
		//
		// The mirror below renders a row's *range*, and a range is not a screen. A range
		// that begins mid-line — after a repaint (`\r[100%]` written over `download`,
		// which leaves the `load` the terminal never erased), after a cursor move
		// (`\x1b[2;5H` then `\u4e2d\u6587`, which xterm shows as "    \u4e2d\u6587"), or at a
		// soft wrap — is only ever part of what the row shows: what the range covers is
		// contiguous and in order, but columns outside it belong to an earlier version
		// of the row. Such rows are held to containment, which still catches text
		// landing on the wrong row. A range that starts at a line boundary is the whole
		// of what was written on its row and has to match exactly.
		//
		// Backspace is the one shape neither rule fits: it moves the cursor back over
		// columns the model does not track, so a range's bytes render as a
		// concatenation (`ab\b\b\b\bsecret` -> "absecret") where the terminal shows the
		// overwritten result ("secret"). Those rows are skipped. Their byte-to-row
		// placement is still checked above; modelling columns is deliberately not
		// something the layout does.
		modelRows := make([]string, 0, g.rowCount())
		for i := range g.rows {
			sp := g.rows[i]
			if sp.End <= sp.Start {
				modelRows = append(modelRows, "")
				continue
			}
			modelRows = append(modelRows, xtermRowText(data[sp.Start:sp.End]))
		}
		for i := 0; i < len(modelRows) && i < len(probe.Text); i++ {
			if strings.TrimSpace(modelRows[i]) == "" {
				continue
			}
			sp := g.rows[i]
			raw := data[sp.Start:sp.End]
			if bytes.IndexByte(raw, '\b') >= 0 {
				continue
			}
			got := strings.TrimRight(modelRows[i], " ")
			want := strings.TrimRight(probe.Text[i], " ")
			if got == want {
				continue
			}
			// A range preceded by a line ending was written whole: a difference is not
			// something a repaint or a cursor move explains.
			atLineStart := sp.Start == 0 || data[sp.Start-1] == '\n'
			if atLineStart {
				t.Errorf("%s: row %d holds %q in the model and %q in xterm", name, i, got, want)
				continue
			}
			if !strings.Contains(want, got) {
				t.Errorf("%s: row %d holds %q, which does not appear in what xterm shows (%q)", name, i, got, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("nothing was compared")
	}
	t.Logf("compared %d byte positions across %d cases", checked, len(names))
}

// xtermRowText is what a terminal shows for the bytes a row holds: the printable
// runes, with escape sequences and carriage returns dropped and tabs expanded to
// the tab stops xterm uses. It is the mirror of `translateToString` on the node
// side, and it is deliberately dumb — a row's range covers bytes of several kinds
// and only the visible ones are text.
func xtermRowText(b []byte) string {
	var out []rune
	col := 0
	esc := 0
	for i := 0; i < len(b); {
		c := b[i]
		if esc != 0 {
			switch esc {
			case escEsc:
				if c == '[' {
					esc = escCSI
				} else if c == ']' {
					esc = escOSC
				} else {
					esc = 0
				}
			case escCSI:
				if c >= 0x40 && c <= 0x7e {
					esc = 0
				}
			case escOSC:
				if c == 0x07 {
					esc = 0
				} else if c == 0x1b {
					esc = escOSCEsc
				}
			case escOSCEsc:
				esc = 0
			}
			i++
			continue
		}
		switch {
		case c == 0x1b:
			esc = escEsc
			i++
		case c == '\r' || c == '\n' || c == '\b':
			// A bare carriage return starts a repaint, but the carriage return in
			// CRLF is only the first half of a line ending: xterm leaves the text
			// standing on that row. The model makes the same distinction when it
			// arms a repaint.
			if c == '\r' && (i+1 >= len(b) || b[i+1] != '\n') {
				out = out[:0]
				col = 0
			}
			i++
		case c == '\t':
			n := shellRailTabStop - (col % shellRailTabStop)
			for k := 0; k < n; k++ {
				out = append(out, ' ')
			}
			col += n
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			out = append(out, r)
			if kind := utf8.RuneLen(r); kind > 0 {
				col++
			}
			i += size
		}
	}
	return string(out)
}
