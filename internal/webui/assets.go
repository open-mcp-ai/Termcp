package webui

import (
	"io/fs"
)

// Assets returns the embedded asset FS rooted at the assets directory. Besides
// the browser UI it holds the agent-facing documents (api.md, skills.md) that the
// static server publishes
// and that the MCP server registers as resources.
func Assets() fs.FS {
	root, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic("webui: embed assets: " + err.Error())
	}
	return root
}
