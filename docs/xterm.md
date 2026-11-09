# Vendored xterm.js

`third_party/xterm/` contains upstream build artifacts of the
[xterm.js](https://github.com/xtermjs/xterm.js) browser terminal emulator,
copied verbatim from the official npm distribution. They are minified
production artifacts published by the xterm.js project — **not** termcp source
code, and not obfuscated by us.

## Contents

Upstream release: `@xterm/xterm@6.0.0`

| | |
|---|---|
| npm package | `@xterm/xterm` |
| tarball URL | `https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz` |
| tarball SHA-1 (npm `dist.shasum`) | `93637b0f2ee3a70718b5746a27c9c506af16745b` |
| tarball integrity (npm `dist.integrity`) | `sha512-TQwDdQGtwwDt+2cgKDLn0IRaSxYu1tSUjgKarSDkUM0ZNiSRXFpjxEsvc/Zgc5kq5omJ+V0a8/kIM2WD3sMOYg==` |

Files copied out of that tarball, unmodified:

| In-tree path | Upstream path in tarball | SHA-256 |
|---|---|---|
| `third_party/xterm/xterm.js` | `package/lib/xterm.js` | `14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972` |
| `third_party/xterm/xterm.css` | `package/css/xterm.css` | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

Only those two runtime files are vendored. Three more files sit beside them and
**none of them ships**:

| File | Why |
|---|---|
| `NOTICE` | The MIT text. It travels with the release assets and the container image rather than inside the binary. |
| `README.md` | A one-screen answer to "what is this single-line file", for whoever opens the directory. |
| `embed.go` | The `//go:embed xterm.js xterm.css` line: it names the two files that ship, which is what keeps the other two out. |

**The embed list is the mechanism, and it is why this directory exists.** A
`//go:embed` of the directory would take everything in it; naming the files
takes exactly two. Keeping the vendored bytes out of `internal/webui/assets/`
is what makes that possible without an underscore prefix on the licence.

The Web UI still asks for `static/xterm/xterm.js` and `static/xterm/xterm.css`:
`internal/webui/vendored.go` serves this filesystem behind those two paths, so
`index.html` and the browser see no difference.

Longer documentation lives here in `docs/` rather than next to the assets,
because the assets directory is served as static routes and a file nobody links
to is not documentation.

`internal/webui/xterm_provenance_test.go` pins the arrangement: the embedded
bytes still hash to the values in the table above, `NOTICE` and `README.md` are
still *absent* from the served filesystem, and `embed.go` still names exactly
the two files that ship.

Only the core package is vendored. The Web UI implements its own grid fitting
(`fitShellTerminal` in `internal/webui/assets/static/js/ui-socket.js`), so
`@xterm/addon-fit` is not needed and is not shipped.

## Why vendored, minified

- termcp ships a **single static binary** (`go:embed`); the Web UI must be
  self-contained, so build-time npm downloads are not an option.
- Minification is upstream's default distribution format (the exact bytes
  `npm install` places in `node_modules`); we do not re-minify or patch it, so
  any reviewer can verify it bit-for-bit against the official release.
- The assets directory is embedded into the Go binary at build time. Keeping
  documentation in `docs/` rather than inside `assets/` ensures doc updates
  never bloat the embedded assets or get served as static web routes.

## How to verify

Two independent checks:

```sh
# 1. Verify the upstream tarball against the registry-published checksums:
curl -sLO https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz
sha1sum xterm-6.0.0.tgz     # matches 93637b0f2ee3a70718b5746a27c9c506af16745b

# 2. Verify in-tree files are byte-identical to the tarball's contents:
tar -xzf xterm-6.0.0.tgz
cmp third_party/xterm/xterm.js package/lib/xterm.js
cmp third_party/xterm/xterm.css package/css/xterm.css
```

## 6.0.0 integration notes

6.0.0 replaced the native-scrollbar viewport with a VS Code-derived scrollable
element. The DOM is:

```
.xterm
  .xterm-viewport            (no longer scrolls)
  .xterm-scrollable-element
    .xterm-screen
    .scrollbar.vertical > .slider
```

What that changed for termcp's UI code (all in
`internal/webui/assets/static/js/` and `internal/webui/assets/static/css/`):

- Scroll position is read from `term.buffer.active.viewportY` / `baseY`, not
  from `.xterm-viewport.scrollTop` (which stays at zero).
- Scroll *events* come from `term.onScroll`; DOM `scroll` listeners on the
  viewport never fire.
- Column fitting (`fitShellTerminal`) reserves the 14px scrollbar gutter
  explicitly using the official fit addon's formula, because the overlay no
  longer shrinks the viewport.
- `.xterm-screen` must not be absolutely positioned: it is now a child of the
  scrollable element, which collapses to zero height otherwise.
- Scrollbar slider colours come from the terminal theme
  (`scrollbarSliderBackground` and friends).

## Upgrading

Download the target `@xterm/xterm` npm tarball, copy `package/lib/xterm.js` and
`package/css/xterm.css` into `third_party/xterm/`, and update the three copies
of the checksums that must agree: this file, `README.md` beside the files, and
`THIRD_PARTY_NOTICES.md` at the repository root — plus the constants in
`internal/webui/xterm_provenance_test.go` (a mismatch fails the test). Re-read
the DOM notes before a major bump.

## License

xterm.js is MIT-licensed. The notice is reproduced here and in
`third_party/xterm/NOTICE`. It does **not** ship inside the binary: the two
distributed forms that can carry a file do it instead — every GitHub release
attaches `THIRD_PARTY_NOTICES.md`, and the container image copies it to
`/usr/share/doc/termcp/`. `go install` is covered by the module itself, which
contains this directory.

```
Copyright (c) 2017-2019, The xterm.js authors (https://github.com/xtermjs/xterm.js)
Copyright (c) 2014-2016, SourceLair Private Company (https://www.sourcelair.com)
Copyright (c) 2012-2013, Christopher Jeffrey (https://github.com/chjj/)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```
