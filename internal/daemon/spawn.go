// Package daemon manages the detached background termcp instance behind the
// `termcp daemon` subcommands: spawning it, finding the instance that answers
// a given HTTP endpoint, asking it to stop, and the idle countdown that ends a
// daemon on its own. Discovery is deliberately HTTP-only — an instance is
// found because it answers at its endpoint, whichever data directory or
// platform started it, and whether or not it was started as a daemon.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/config"
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
	if cfg.MCPDeferTools {
		args = append(args, "--mcp-defer-tools")
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

// spawn starts the detached instance: stdin from the null device, stdout and
// stderr appended to the daemon log, and no wait — the child outlives this
// process.
func spawn(exe string, args, env []string, logPath string) (int, error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", exe, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // never wait: the child is detached
	return pid, nil
}

func stopQuietly(pid int) {
	_ = terminate(pid)
}
