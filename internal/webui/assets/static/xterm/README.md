# Vendored xterm.js

The files in this directory are the upstream **xterm.js** browser terminal
emulator, copied verbatim from the official npm distribution. They are minified
build artifacts published by the xterm.js project — **not** termcp source code,
and not obfuscated by us.

## Contents

Upstream release: `@xterm/xterm@6.0.0`

| | |
|---|---|
| tarball | `https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz` |
| tarball SHA-1 (npm `dist.shasum`) | `93637b0f2ee3a70718b5746a27c9c506af16745b` |
| tarball integrity (npm `dist.integrity`) | `sha512-TQwDdQGtwwDt+2cgKDLn0IRaSxYu1tSUjgKarSDkUM0ZNiSRXFpjxEsvc/Zgc5kq5omJ+V0a8/kIM2WD3sMOYg==` |

Files copied out of that tarball, unmodified:

| File | Upstream path | SHA-256 |
|---|---|---|
| `xterm.js` | `package/lib/xterm.js` | `14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972` |
| `xterm.css` | `package/css/xterm.css` | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

Only the core package is vendored: the UI fits the terminal itself
(`fitShellTerminal` in `static/js/ui-socket.js`), so `@xterm/addon-fit` is not
needed and is not shipped.

## Why vendored, minified

- termcp ships a **single static binary** (`go:embed`); the Web UI must be
  self-contained, so build-time npm downloads are not an option.
- Minification is upstream's default distribution format (same bytes as
  `npm install`); we do not patch it, so it can be verified bit-for-bit against
  the npm release.
- The matching version is loaded by `internal/webui/assets/index.html`
  (`static/xterm/xterm.js` + `static/xterm/xterm.css`).

## Verify

Two independent checks, so a reader can pick whichever they trust:

```sh
# 1. The upstream tarball itself is the published release
curl -sLO https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz
sha1sum xterm-6.0.0.tgz     # 93637b0f2ee3a70718b5746a27c9c506af16745b

# 2. The two files here are byte-identical to the ones inside it
tar -xzf xterm-6.0.0.tgz
sha256sum package/lib/xterm.js package/css/xterm.css
sha256sum internal/webui/assets/static/xterm/xterm.js internal/webui/assets/static/xterm/xterm.css
```

## 6.0.0 integration notes

6.0.0 replaced the native-scrollbar viewport with a VS Code-derived scrollable
element. The DOM is now:

```
.xterm
  .xterm-viewport            (no longer scrolls)
  .xterm-scrollable-element
    .xterm-screen
    .scrollbar.vertical > .slider
```

What that changed for termcp's UI code (all handled in
`static/js/ui-socket.js`, `static/js/terminal-view.js`, `static/js/timeline.js`
and `static/css/terminal*.css`):

- Scroll position is read from `term.buffer.active.viewportY` / `baseY`, not
  from `.xterm-viewport.scrollTop` (which stays at zero).
- Scroll *events* come from `term.onScroll`; DOM `scroll` listeners on the
  viewport never fire.
- Column fitting reserves the scrollbar gutter explicitly, because the overlay
  no longer shrinks the viewport.
- `.xterm-screen` must not be absolutely positioned: it is now a child of the
  scrollable element, which collapses to zero height otherwise.
- Scrollbar slider colours come from the terminal theme
  (`scrollbarSliderBackground` and friends).

## Upgrading

Replacing a file means downloading the new npm tarball, copying `lib/xterm.js`
and `css/xterm.css`, and updating the table above. Re-read the DOM notes before
a major bump: 6.0.0 changed the scroll implementation, which the Web UI styles
and listeners depend on.
