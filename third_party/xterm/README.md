# Vendored xterm.js — what this directory is

Upstream build artifacts of [xterm.js](https://github.com/xtermjs/xterm.js),
copied byte-for-byte from the official npm distribution:

| file | upstream path | sha256 |
|---|---|---|
| `xterm.js` | `@xterm/xterm@6.0.0` → `package/lib/xterm.js` | `14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972` |
| `xterm.css` | `@xterm/xterm@6.0.0` → `package/css/xterm.css` | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

**These are not termcp source and they are not obfuscated by us.** `xterm.js` is
one minified line because that is upstream's own distribution format — the exact
bytes `npm install @xterm/xterm` writes. We do not re-minify, patch or annotate
them, so any reviewer can verify them bit-for-bit against the published release:

```sh
npm pack @xterm/xterm@6.0.0 && tar -xzf xterm-6.0.0.tgz
cmp xterm.js  package/lib/xterm.js     # identical
cmp xterm.css package/css/xterm.css    # identical
```

Full provenance (tarball URL, npm shasum/integrity, upgrade steps, the reason
the bundle is minified): [`docs/xterm.md`](../../docs/xterm.md).
The licence: [`NOTICE`](./NOTICE).

## What ships, and what stays here

[`embed.go`](./embed.go) embeds **two files by name**:

```go
//go:embed xterm.js xterm.css
```

so `NOTICE` and this README are read by whoever opens the directory — a
reviewer, a scanner, the next person upgrading xterm — and never enter the
binary. A directory pattern could not express that, and it is why this
directory exists at all instead of the files sitting inside the Web UI's
`assets/` tree: the provenance lives next to the bytes, the shipped bytes stay
exactly two, and the file that decides which is which is Go source that cannot
drift from the build.

The Web UI still serves them at their historical URLs (`static/xterm/xterm.js`,
`static/xterm/xterm.css`) — `internal/webui/vendored.go` puts this filesystem
behind those two paths, so `index.html` did not change.

Because the licence does not ship inside the binary, it travels with the
artifacts: `THIRD_PARTY_NOTICES.md` is attached to every GitHub release and
copied into the container image (`/usr/share/doc/termcp/`). The `go install`
path is covered by the module itself, which contains this directory.

`internal/webui/xterm_provenance_test.go` asserts the whole arrangement: the
served bytes hash to the values above, `NOTICE` and `README.md` are absent from
the served filesystem, and `embed.go` still names exactly those two files.

## Do not edit

Changing `xterm.js` or `xterm.css` breaks the byte-for-byte guarantee the
checksums exist to provide. To upgrade, follow the steps in `docs/xterm.md` and
update the hashes there, here, and in `THIRD_PARTY_NOTICES.md` — the test fails
if any of them drift apart.
