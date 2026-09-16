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

// EnvAuthToken / EnvAuthHash configure HTTP authentication from the
// environment. Flags take precedence over these when both are set.
const (
	EnvAuthToken = "TERMCP_AUTH_TOKEN"
	EnvAuthHash  = "TERMCP_AUTH_HASH"
)

// Config holds all runtime configuration for the server.
type Config struct {
	Host                string // HTTP server bind address (default: "127.0.0.1" = loopback; use 0.0.0.0 for all interfaces)
	Port                int    // HTTP server port, must be 1-65535 (default: 18765)
	DataDir             string // persistent storage directory; empty means default ($TERMCP_DATA_DIR or ~/.termcp)
	LogLevel            string // log verbosity: debug|info|warn|error (default: "info")
	NoInternal          bool   // disable the built-in loopback SSH profile
	MCPManageSSHConfigs bool   // enable MCP tools for creating/editing/deleting SSH configs (default: false)
	AuthToken           string // plaintext HTTP auth token ($TERMCP_AUTH_TOKEN); mutually exclusive with AuthHash
	AuthHash            string // salted SHA-256 token hash ($TERMCP_AUTH_HASH); generate with `termcp --gen-auth-hash`
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
	if isNonLoopbackBind(c.Host) && c.AuthToken == "" && c.AuthHash == "" {
		return fmt.Errorf("non-loopback host %q requires --auth-token, --auth-hash, TERMCP_AUTH_TOKEN, or TERMCP_AUTH_HASH", c.Host)
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
