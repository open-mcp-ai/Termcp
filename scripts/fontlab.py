#!/usr/bin/env python3
"""Consolidated font-stack measurement for the termcp Web UI terminal.

Everything here runs the REAL bundled xterm.js in headless Chrome, because the
questions that matter cannot be answered by reading the CSS:

  1. Which face does each name actually resolve to on this host?
  2. What is the resulting cell width, and how much does xterm have to pad a Han
     glyph to make it fill exactly two cells?

The second is the objective quality metric for a terminal font stack, and it is
not a matter of taste. xterm aligns a cell by correcting each span:

    letterSpacing = cells * cellWidth - glyphWidth     (cells = 2 for Han)

A Han advance that is not exactly twice the Latin cell width shows up as
non-zero letter-spacing, i.e. Chinese text that looks loosely spaced (positive)
or collides (negative). Note that the Latin face and the Han face are selected
independently by the browser, so this is about *matching* the two, not about
either one being good in isolation.

Run: python scripts/fontlab.py
"""
import json
import os
import re
import subprocess
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
XTERM_JS = os.path.join(ROOT, "internal/webui/assets/static/xterm/xterm.js")
XTERM_CSS = os.path.join(ROOT, "internal/webui/assets/static/xterm/xterm.css")
TOKENS = os.path.join(ROOT, "internal/webui/assets/static/css/tokens.css")
CHROME = os.environ.get("CHROME", r"C:\Program Files\Google\Chrome\Application\chrome.exe")

# NSimSun and SimSun are both probed on purpose: the shipped stack uses NSimSun
# and not SimSun, and that distinction came out of this measurement (the SimSun
# family's monospaced face is the one that wins at Han = 2 x Latin).
MONO_PROBE = ["ui-monospace", "monospace", "SF Mono", "Menlo", "Monaco", "Consolas",
              "Cascadia Mono", "JetBrains Mono", "DejaVu Sans Mono", "Noto Sans Mono",
              "MS Gothic", "SimSun", "NSimSun", "Sarasa Mono SC", "Noto Sans Mono CJK SC",
              "Microsoft YaHei", "Microsoft JhengHei", "Noto Sans CJK SC"]

STACKS = [
    ("OLD terminal (before fix)", "Consolas, Monaco, monospace"),
    ("NEW terminal (shipped)", "__SHIPPED__"),
    ("Latin-only + generic Han", '"Consolas", monospace'),
    ("Single CJK face, both scripts", '"SimSun"'),
    ("CJK mono first, then Latin", '"Noto Sans Mono CJK SC", "Sarasa Mono SC", Consolas, monospace'),
]

PROBE_PAGE = r"""<!DOCTYPE html><meta charset="utf-8"><title>p</title><body><script>
function probe(f) { var c = document.createElement('canvas').getContext('2d');
  c.font = '13px ' + f;
  return [+c.measureText('W').width.toFixed(3), +c.measureText('M').width.toFixed(3),
          +c.measureText('i').width.toFixed(3), +c.measureText('\u4e2d').width.toFixed(3)]; }
var names = __NAMES__, out = {};
for (var k = 0; k < names.length; k++) {
  var n = names[k];
  var q = /^[A-Za-z][A-Za-z0-9-]*$/.test(n) ? n : '"' + n + '"';
  out[n] = probe(q);
}
document.title = JSON.stringify(out);
</script></body>"""

RENDER_PAGE = r"""<!DOCTYPE html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="__CSS__">
<style>html,body{margin:0}#t{width:900px;height:200px}</style></head>
<body><div id="t"></div><script src="__JS__"></script><script>
var term = new Terminal({fontSize: 13, fontFamily: __FONT__, scrollback: 50});
term.open(document.getElementById('t'));
var NL = String.fromCharCode(13, 10);
term.write('MM' + NL + '\u4e2d\u6587' + NL, function () {
  setTimeout(function () {
    var cellW = term.element.querySelector('.xterm-screen').clientWidth / term.cols;
    var out = {cellW: +cellW.toFixed(4), latin: null, han: null, hanCls: null};
    [].slice.call(document.querySelectorAll('.xterm-rows span')).forEach(function (s) {
      var ls = parseFloat(s.style.letterSpacing);
      ls = isNaN(ls) ? null : +ls.toFixed(4);
      if (/[\u4e00-\u9fff]/.test(s.textContent)) { out.han = ls; out.hanCls = s.style.fontFamily || null; }
      else if (/M/.test(s.textContent)) out.latin = ls;
    });
    document.title = JSON.stringify(out);
  }, 350);
});
</script></body></html>"""


def chrome(html, timeout=180):
    with tempfile.NamedTemporaryFile("w", suffix=".html", delete=False, encoding="utf-8") as fh:
        fh.write(html)
        path = fh.name
    try:
        p = subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--no-sandbox",
                            "--allow-file-access-from-files", "--virtual-time-budget=8000",
                            "--dump-dom", "file:///" + path.replace("\\", "/")],
                           capture_output=True, timeout=timeout)
        m = re.search(r"<title>(.*?)</title>", p.stdout.decode("utf-8", "replace"), re.S)
        return json.loads(m.group(1).replace("&quot;", '"').replace("&amp;", "&")) if m else None
    finally:
        os.unlink(path)


def shipped_mono():
    s = open(TOKENS, encoding="utf-8").read()
    m = re.search(r"--font-mono:(.*?);", s, re.S)
    return " ".join(re.sub(r"/\*.*?\*/", "", m.group(1), flags=re.S).split())


def main():
    shipped = shipped_mono()

    print("=" * 78)
    print("1. WHAT EACH NAME RESOLVES TO ON THIS HOST (13px, Latin metrics)")
    print("=" * 78)
    print("Grouped by identical metrics = same face answered. W==M and i==W means a")
    print("true monospace; W far from the Han advance means Han will need padding.\n")
    d = chrome(PROBE_PAGE.replace("__NAMES__", json.dumps(MONO_PROBE)))
    groups = {}
    for name, (w, mm, i, han) in d.items():
        groups.setdefault((w, mm, i, han), []).append(name)
    print("%9s %9s %9s %9s   %s" % ("W", "M", "i", "Han", "names"))
    for (w, mm, i, han), names in sorted(groups.items(), key=lambda kv: kv[0][0]):
        print("%9s %9s %9s %9s   %s" % (w, mm, i, han, ", ".join(names)))

    print()
    print("=" * 78)
    print("2. MEASURED THROUGH THE REAL BUNDLED xterm.js")
    print("=" * 78)
    print("pad = 2*cellW - HanAdvance = the letter-spacing xterm adds to a Han cell.")
    print("0.0 is ideal. Positive = the character is spaced out to fill its two cells.\n")
    print("%-32s %9s %9s %9s" % ("stack", "cellW", "Han pad", "verdict"))
    for name, stack in STACKS:
        if stack == "__SHIPPED__":
            stack = shipped
        page = (RENDER_PAGE.replace("__CSS__", "file:///" + XTERM_CSS.replace("\\", "/"))
                           .replace("__JS__", "file:///" + XTERM_JS.replace("\\", "/"))
                           .replace("__FONT__", json.dumps(stack)))
        r = chrome(page)
        if not r:
            print("%-32s     (chrome failed)" % name)
            continue
        pad = r["han"]
        if pad is None:
            v = "no Han span painted"
        elif abs(pad) < 0.05:
            v = "IDEAL"
        elif pad > 0:
            v = "padded"
        else:
            v = "OVERLAP"
        print("%-32s %9.4f %9s %9s"
              % (name, r["cellW"], "%.4f" % pad if pad is not None else "none", v))

    print()
    print("Reading: Han advance is ~1em in every CJK face, so pad is driven by the")
    print("Latin cell width the browser picked. The only way to reach pad 0 is a face")
    print("that supplies BOTH scripts at Han = 2 x Latin, which is what a CJK")
    print("monospaced font is built to do.")


if __name__ == "__main__":
    main()
