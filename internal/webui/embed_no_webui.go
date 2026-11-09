//go:build no_webui

package webui

import (
	"embed"
	"io/fs"
)

// uiEmbedded is false in the pure-API build: there is no browser UI to serve.
const uiEmbedded = false

// The pure-API build still carries the agent documents. They are the HTTP API's
// own reference and the extension layer's guide, and an instance that cannot
// hand an agent the docs for the surfaces it serves is not usable by that agent
// — only the browser UI is dropped. Selected by `go build -tags no_webui`
// (make build-api).
//
//go:embed assets/api.md
var embeddedAssets embed.FS

// vendoredAssets is nil in the pure-API build: no browser UI, no browser
// terminal, and therefore none of its bytes in the binary.
func vendoredAssets() fs.FS { return nil }
