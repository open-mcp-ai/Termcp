//go:build !no_webui

package webui

import "embed"

// uiEmbedded reports whether this build carries the browser UI. The default
// build does; `go build -tags no_webui` selects embed_no_webui.go instead,
// which drops both the UI assets and their routes.
const uiEmbedded = true

//go:embed assets
var embeddedAssets embed.FS
