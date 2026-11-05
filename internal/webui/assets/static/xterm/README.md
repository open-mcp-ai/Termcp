# Vendored xterm.js

The files in this directory are the upstream **xterm.js** browser terminal
emulator, copied verbatim from the official npm distribution. They are minified
build artifacts published by the xterm.js project — **not** termcp source code,
and not obfuscated by us.

## Contents

| File | Upstream package | Version | SHA-256 |
|---|---|---|---|
| `xterm.js` | `@xterm/xterm` (`lib/xterm.js`) | **6.0.0** | `14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972` |
| `xterm.css` | `@xterm/xterm` (`css/xterm.css`) | **6.0.0** | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

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

```sh
sha256sum internal/webui/assets/static/xterm/xterm.js internal/webui/assets/static/xterm/xterm.css
# compare against the npm tarball of the same version:
#   https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz
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
