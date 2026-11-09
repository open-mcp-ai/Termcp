//go:build !no_webui

package webui

import (
	"embed"
	"io/fs"

	"github.com/open-mcp-ai/termcp/third_party/xterm"
)

// uiEmbedded reports whether this build carries the browser UI. The default
// build does; `go build -tags no_webui` selects embed_no_webui.go instead,
// which drops both the UI assets and their routes.
const uiEmbedded = true

//go:embed assets
var embeddedAssets embed.FS

// vendoredAssets is the third-party browser terminal, embedded by its own
// package (third_party/xterm/embed.go) so that the files describing it —
// README.md, NOTICE — stay in the repository. The pure-API build returns nil
// and carries none of it.
func vendoredAssets() fs.FS { return xterm.Assets }
