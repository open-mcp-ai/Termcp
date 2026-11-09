// Package xterm carries the vendored xterm.js browser terminal: upstream build
// artifacts, copied byte-for-byte from the official npm distribution
// (`@xterm/xterm@6.0.0`), never patched and never re-minified.
//
// THE EMBED LINE BELOW IS THE POINT OF THIS FILE. It names two files instead of
// embedding the directory, so everything else that belongs beside them — the
// provenance in README.md, the licence in NOTICE — stays in the repository and
// out of the binary. A directory pattern cannot express that, and naming the
// files here is more legible than an underscore prefix or a list kept somewhere
// else: whoever opens this directory reads, in Go, exactly what ships.
//
// Serving path is unchanged (the Web UI still asks for static/xterm/xterm.js);
// internal/webui puts this filesystem behind those two URLs.
//
// Upgrade procedure, the reason the bundle is one minified line, and the
// checksums that must agree: README.md in this directory and docs/xterm.md.
package xterm

import "embed"

//go:embed xterm.js xterm.css
var Assets embed.FS
