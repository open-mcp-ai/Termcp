// Package daemon manages the detached background termcp instance behind the
// `termcp daemon` subcommands: spawning it, finding the instance that answers
// a given HTTP endpoint, asking it to stop, and the idle countdown that ends a
// daemon on its own. Discovery is deliberately HTTP-only — an instance is
// found because it answers at its endpoint, whichever data directory or
// platform started it, and whether or not it was started as a daemon.
package daemon

import (
	"os"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/config"
	pkgdaemon "github.com/open-mcp-ai/termcp/pkg/daemon"
)

// ChildArgs builds the command-line flags for the detached instance. The
// parent rebuilds flags from its resolved configuration instead of replaying
// argv, so the child gets a complete, explicit setup. Secrets are NOT included:
// they travel via the environment (childEnv), never argv.
func ChildArgs(cfg *config.Config, idleTimeout string) []string {
	args := []string{
		"--host", cfg.Host,
		"--port", strconv.Itoa(cfg.Port),
		"--data-dir", cfg.DataDir,
	}
	if cfg.AssetsDir != "" {
		args = append(args, "--assets", cfg.AssetsDir)
	}
	if cfg.LogLevel != "" {
		args = append(args, "--log-level", cfg.LogLevel)
	}
	if cfg.NoInternal {
		args = append(args, "--no-internal")
	}
	if cfg.MCPManageSSHConfigs {
		args = append(args, "--mcp-manage-ssh-configs")
	}
	if idleTimeout != "" {
		args = append(args, "--idle-timeout", idleTimeout)
	}
	return args
}

// childEnv is the environment for the detached instance: the inherited
// environment with daemon markers and credentials (re)appended, so that flag
// values resolved by the parent win over any inherited TERMCP_* variables.
func childEnv(cfg *config.Config) []string {
	env := filterEnv(os.Environ(), []string{
		EnvChild + "=",
		config.EnvAuthToken + "=",
		config.EnvAuthHash + "=",
		config.EnvDisableAuth + "=",
	})
	env = append(env, EnvChild+"=1")
	if cfg.AuthToken != "" {
		env = append(env, config.EnvAuthToken+"="+cfg.AuthToken)
	}
	if cfg.AuthHash != "" {
		env = append(env, config.EnvAuthHash+"="+cfg.AuthHash)
	}
	if cfg.DisableAuth {
		env = append(env, config.EnvDisableAuth+"=1")
	}
	return env
}

// filterEnv drops entries with any of the given name prefixes (matched
// case-insensitively: Windows environment blocks are case-insensitive).
func filterEnv(env, prefixes []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		skip := false
		for _, p := range prefixes {
			if len(kv) >= len(p) && strings.EqualFold(kv[:len(p)], p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

// spawn starts the detached instance through pkg/daemon, so the redirect-and-detach
// recipe exists once (issue #309).
func spawn(exe string, args, env []string, logPath string) (int, error) {
	return pkgdaemon.Spawn(exe, args, env, logPath)
}

func stopQuietly(pid int) {
	_ = terminate(pid)
}
