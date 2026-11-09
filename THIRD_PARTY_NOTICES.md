# Third-party notices

termcp ships one vendored third-party component. Everything else in this
repository is termcp's own code, under the [MIT License](./LICENSE).

| component | version | license | in-tree location | ships as |
|---|---|---|---|---|
| [xterm.js](https://github.com/xtermjs/xterm.js) | `@xterm/xterm@6.0.0` | MIT | `third_party/xterm/` | two runtime files inside the binary; this notice beside every release and in the container image |

## xterm.js

Copied **byte-for-byte** from the official npm distribution
(`package/lib/xterm.js`, `package/css/xterm.css`), not patched, not re-minified.

| file | sha256 |
|---|---|
| `xterm.js` | `14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972` |
| `xterm.css` | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

- Why it is vendored, and why the bundle is one minified line:
  [`docs/xterm.md`](./docs/xterm.md) — it is upstream's own distribution format,
  unmodified, so the bytes are verifiable against the published release.
- License text: reproduced at the bottom of this file, kept in
  [`third_party/xterm/NOTICE`](./third_party/xterm/NOTICE), and also in
  [`docs/xterm.md`](./docs/xterm.md#license).
- Where it travels: this file is attached to **every GitHub release** and copied
  into the **container image** at `/usr/share/doc/termcp/`. It is deliberately
  *not* embedded in the binary — the vendored bytes are embedded by name from
  [`third_party/xterm/embed.go`](./third_party/xterm/embed.go), which is what
  lets the notices sit beside them without shipping.
- Sidecar for whoever opens that directory:
  [`third_party/xterm/README.md`](./third_party/xterm/README.md).
- The hashes above are asserted by `internal/webui/xterm_provenance_test.go`:
  the shipped bytes and the recorded provenance cannot drift apart silently.

### Verify it yourself

```sh
npm pack @xterm/xterm@6.0.0 && tar -xzf xterm-6.0.0.tgz
cmp third_party/xterm/xterm.js  package/lib/xterm.js
cmp third_party/xterm/xterm.css package/css/xterm.css
```

## License text — xterm.js (MIT)

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
