//go:build no_webui

package webui

import "embed"

// uiEmbedded is false in the pure-API build: there is no browser UI to serve.
const uiEmbedded = false

// The pure-API build still carries the two agent documents. They are the HTTP
// API's own reference, and an instance that cannot hand an agent the docs for
// the API it serves is not usable by that agent — only the browser UI is
// dropped. Selected by `go build -tags no_webui` (make build-api).
//
//go:embed assets/api.md assets/skills.md
var embeddedAssets embed.FS
