# Vendored xterm.js

`internal/webui/assets/static/xterm/` contains upstream build artifacts of the
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
| `internal/webui/assets/static/xterm/xterm.js` | `package/lib/xterm.js` | `14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972` |
| `internal/webui/assets/static/xterm/xterm.css` | `package/css/xterm.css` | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |
| `internal/webui/assets/static/xterm/LICENSE` | `package/LICENSE` | upstream MIT license |

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
cmp internal/webui/assets/static/xterm/xterm.js package/lib/xterm.js
cmp internal/webui/assets/static/xterm/xterm.css package/css/xterm.css
cmp internal/webui/assets/static/xterm/LICENSE package/LICENSE
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

Download the target `@xterm/xterm` npm tarball, copy `package/lib/xterm.js`,
`package/css/xterm.css` and `package/LICENSE` to
`internal/webui/assets/static/xterm/`, and update the checksums in this file.
Re-read the DOM notes before a major bump.
