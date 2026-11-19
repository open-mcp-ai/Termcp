package daemon

import (
	"time"

	public "github.com/open-mcp-ai/termcp/pkg/daemon"
)

// DefaultIdleTimeout is Termcp's idle policy, not a library default.
const DefaultIdleTimeout = 30 * time.Second

type IdleWatcher = public.IdleWatcher

func NewIdleWatcher(timeout time.Duration, fire func()) *IdleWatcher {
	return public.NewIdleWatcher(timeout, fire)
}
