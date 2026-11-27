package daemon

// THE IDLE COUNTDOWN IS pkg/daemon's (issue #309): it counts in-flight requests and
// fires once when they have been zero for a window, and nothing about that is specific
// to this program. Aliased here so the rest of termcp — and the tests that pin the
// behaviour — keep their spelling.

import (
	"time"

	pkgdaemon "github.com/open-mcp-ai/termcp/pkg/daemon"
)

// DefaultIdleTimeout is how long an instance stays alive without any activity.
const DefaultIdleTimeout = pkgdaemon.DefaultIdleTimeout

// IdleWatcher fires a callback once the wrapped handler has seen no in-flight requests
// for a given duration.
type IdleWatcher = pkgdaemon.IdleWatcher

// NewIdleWatcher returns a watcher for the given idle timeout.
func NewIdleWatcher(timeout time.Duration, fire func()) *IdleWatcher {
	return pkgdaemon.NewIdleWatcher(timeout, fire)
}
