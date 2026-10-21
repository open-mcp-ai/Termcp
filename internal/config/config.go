package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// EnvDataDir overrides the default data directory when set.
const EnvDataDir = "TERMCP_DATA_DIR"

// EnvAssetsDir overrides the default external assets directory when set.
const EnvAssetsDir = "TERMCP_ASSETS_DIR"

// EnvAuthToken / EnvAuthHash configure HTTP authentication from the
// environment. Flags take precedence over these when both are set.
const (
	EnvAuthToken = "TERMCP_AUTH_TOKEN"
	EnvAuthHash  = "TERMCP_AUTH_HASH"
)

// EnvMCPAuthToken / EnvMCPAuthHash configure the MCP-surface credential from
// the environment: the operator/agent token split (scoped-tokens.md PR 0).
const (
	EnvMCPAuthToken = "TERMCP_MCP_AUTH_TOKEN"
	EnvMCPAuthHash  = "TERMCP_MCP_AUTH_HASH"
)

// EnvDisableAuth turns HTTP authentication off from the environment. It is the
// explicit escape hatch for hosts where network reachability is already the
// access control (loopback, container-internal only, trusted lab LAN).
const EnvDisableAuth = "TERMCP_DISABLE_AUTH_TOKEN"

// Config holds all runtime configuration for the server.
type Config struct {
	Host                string // HTTP server bind address (default: "127.0.0.1" = loopback; use 0.0.0.0 for all interfaces)
	Port                int    // HTTP server port, must be 1-65535 (default: 18765)
	DataDir             string // persistent storage directory; empty means default ($TERMCP_DATA_DIR or ~/.termcp)
	AssetsDir           string // external static assets directory; files there override the embedded UI/docs (default: $TERMCP_ASSETS_DIR or ~/.termcp/assets)
	LogLevel            string // log verbosity: debug|info|warn|error (default: "info")
	NoInternal          bool   // disable the built-in loopback SSH profile
	MCPManageSSHConfigs bool   // enable the profile write actions (create/edit/copy/delete) (default: false)
	AuthToken           string // plaintext HTTP auth token ($TERMCP_AUTH_TOKEN); mutually exclusive with AuthHash
	AuthHash            string // salted SHA-256 token hash ($TERMCP_AUTH_HASH); generate with `termcp --gen-auth-hash`
	MCPAuthToken        string // plaintext MCP-surface token ($TERMCP_MCP_AUTH_TOKEN); mutually exclusive with MCPAuthHash
	MCPAuthHash         string // salted SHA-256 MCP token hash ($TERMCP_MCP_AUTH_HASH)
	DisableAuth         bool   // force HTTP authentication off (--disable-auth / $TERMCP_DISABLE_AUTH_TOKEN)
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Host:     "127.0.0.1",
		Port:     18765,
		DataDir:  "", // resolved to $TERMCP_DATA_DIR or ~/.termcp at startup
		LogLevel: "info",
	}
}

// DefaultDataDir resolves the default data directory: $TERMCP_DATA_DIR when
// set, otherwise ~/.termcp. A fixed per-user location keeps sessions and SSH
// configs in one place regardless of where the binary lives or runs from.
func DefaultDataDir() (string, error) {
	if env := strings.TrimSpace(os.Getenv(EnvDataDir)); env != "" {
		return filepath.Clean(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".termcp"), nil
}

// DefaultAssetsDir resolves the default external assets directory:
// $TERMCP_ASSETS_DIR when set, otherwise ~/.termcp/assets. The directory is
// allowed not to exist: a missing override leaves every request served from the
// copy embedded in the binary, which is the normal case for a stock install.
func DefaultAssetsDir() (string, error) {
	if env := strings.TrimSpace(os.Getenv(EnvAssetsDir)); env != "" {
		return filepath.Clean(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".termcp", "assets"), nil
}

// DefaultThemesDir is an application resource directory, independent of DataDir.
func DefaultThemesDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".termcp", "themes"), nil
}

// ApplyEnv fills unset auth fields from the corresponding environment
// variables. The caller should invoke this after parsing flags: a non-empty
// flag value therefore wins over the environment.
func (c *Config) ApplyEnv() {
	if c.AuthToken == "" {
		c.AuthToken = strings.TrimSpace(os.Getenv(EnvAuthToken))
	}
	if c.AuthHash == "" {
		c.AuthHash = strings.TrimSpace(os.Getenv(EnvAuthHash))
	}
	if c.MCPAuthToken == "" {
		c.MCPAuthToken = strings.TrimSpace(os.Getenv(EnvMCPAuthToken))
	}
	if c.MCPAuthHash == "" {
		c.MCPAuthHash = strings.TrimSpace(os.Getenv(EnvMCPAuthHash))
	}
	if !c.DisableAuth && envTruthy(os.Getenv(EnvDisableAuth)) {
		c.DisableAuth = true
	}
}

// envTruthy reports whether an environment variable was set to an affirmative
// value. "0", "false", "no" and "off" (any case) mean "not set" so that
// `TERMCP_DISABLE_AUTH_TOKEN=0` cannot surprise an operator into an open server.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Validate checks that all fields are within valid ranges. A non-loopback
// listener must be protected by an auth token or hash; loopback-only binds
// retain the convenient unauthenticated development default.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	if c.AuthToken != "" && c.AuthHash != "" {
		return fmt.Errorf("auth-token and auth-hash are mutually exclusive")
	}
	if c.MCPAuthToken != "" && c.MCPAuthHash != "" {
		return fmt.Errorf("mcp-auth-token and mcp-auth-hash are mutually exclusive")
	}
	// Disabling auth is a deliberate operator decision, so it never silently
	// wins: supplying credentials *and* asking for no auth is a configuration
	// mistake and reported instead of guessed at.
	if c.DisableAuth {
		switch {
		case c.AuthToken != "":
			return fmt.Errorf("auth is disabled (--disable-auth / $%s) but --auth-token / $%s is also set; remove one of them", EnvDisableAuth, EnvAuthToken)
		case c.AuthHash != "":
			return fmt.Errorf("auth is disabled (--disable-auth / $%s) but --auth-hash / $%s is also set; remove one of them", EnvDisableAuth, EnvAuthHash)
		case c.MCPAuthToken != "":
			return fmt.Errorf("auth is disabled (--disable-auth / $%s) but --mcp-auth-token / $%s is also set; remove one of them", EnvDisableAuth, EnvMCPAuthToken)
		case c.MCPAuthHash != "":
			return fmt.Errorf("auth is disabled (--disable-auth / $%s) but --mcp-auth-hash / $%s is also set; remove one of them", EnvDisableAuth, EnvMCPAuthHash)
		}
		return nil
	}
	// The MCP token never counts as "auth is configured" here: it guards the
	// MCP transports only, so it cannot protect the operator surface this
	// check exists for.
	if isNonLoopbackBind(c.Host) && c.AuthToken == "" && c.AuthHash == "" {
		return fmt.Errorf("non-loopback host %q requires --auth-token, --auth-hash, TERMCP_AUTH_TOKEN, or TERMCP_AUTH_HASH (or pass --disable-auth / %s=1 to run open on purpose)", c.Host, EnvDisableAuth)
	}
	return nil
}

func isNonLoopbackBind(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return true
	}
	if parsed := net.ParseIP(strings.Trim(host, "[]")); parsed != nil {
		return !parsed.IsLoopback()
	}
	// Hostnames can resolve differently at runtime; conservatively require
	// authentication unless they are the explicit loopback names.
	return !strings.EqualFold(host, "localhost")
}
