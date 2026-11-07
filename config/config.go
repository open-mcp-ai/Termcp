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

// EnvThemesDir overrides the default external theme directory when set. It
// joined late: the field existed, its resolver existed, and neither the flag nor
// the variable did — see bind.go for how the three were kept apart.
const EnvThemesDir = "TERMCP_THEMES_DIR"

// EnvAuthToken / EnvAuthHash configure HTTP authentication from the
// environment. Flags take precedence over these when both are set.
const (
	EnvAuthToken = "TERMCP_AUTH_TOKEN"
	EnvAuthHash  = "TERMCP_AUTH_HASH"
)

// EnvDisableAuth turns HTTP authentication off from the environment. It is the
// explicit escape hatch for hosts where network reachability is already the
// access control (loopback, container-internal only, trusted lab LAN).
const EnvDisableAuth = "TERMCP_DISABLE_AUTH_TOKEN"

// Config holds all runtime configuration for the server.
type Config struct {
	Host string `flag:"host" env:"TERMCP_HOST" usage:"HTTP bind address (127.0.0.1 = loopback default; 0.0.0.0 = all interfaces)"`
	Port int    `flag:"port" env:"TERMCP_PORT" usage:"HTTP server port"`
	// DataDir, AssetsDir and ThemesDir have no env tag: each one's environment
	// variable is read by its own resolver (DefaultDataDir and the two beside it),
	// because they also have a documented fallback to a path under the user's home.
	// An env tag here would be a second way to say the same thing, and the two
	// would drift exactly like the flags did.
	DataDir             string `flag:"data-dir" usage:"Data directory for JSON storage (default: $TERMCP_DATA_DIR or ~/.termcp)"`
	AssetsDir           string `flag:"assets" usage:"External static assets directory: files there override the embedded Web UI and docs; missing files fall back to the embed (default: $TERMCP_ASSETS_DIR or ~/.termcp/assets)"`
	ThemesDir           string `flag:"themes-dir" usage:"External theme directory served alongside the assets (default: $TERMCP_THEMES_DIR or ~/.termcp/themes)"`
	LogLevel            string `flag:"log-level" env:"TERMCP_LOG_LEVEL" usage:"Log verbosity: debug|info|warn|error"`
	NoInternal          bool   `flag:"no-internal" env:"TERMCP_NO_INTERNAL" usage:"Disable the built-in loopback SSH profile (no internal connection)"`
	MCPManageSSHConfigs bool   `flag:"mcp-manage-ssh-configs" env:"TERMCP_MCP_MANAGE_SSH_CONFIGS" usage:"Enable the profile write actions of the 'profile' directive (create/edit/copy/delete) over MCP (off by default; passwords/keys are never exposed)"`
	Headless            bool   `flag:"headless" env:"TERMCP_HEADLESS" usage:"Serve no browser UI: the MCP tool surface, POST /api/term and the agent docs remain. This is what 'go build -tags no_webui' compiles in, selectable at run time."`
	AuthToken           string `flag:"auth-token" env:"TERMCP_AUTH_TOKEN" usage:"HTTP authentication token (or $TERMCP_AUTH_TOKEN)"`
	AuthHash            string `flag:"auth-hash" env:"TERMCP_AUTH_HASH" usage:"Salted SHA-256 HTTP token hash (or $TERMCP_AUTH_HASH; generate with 'termcp --gen-auth-hash')"`
	DisableAuth         bool   `flag:"disable-auth" env:"TERMCP_DISABLE_AUTH_TOKEN" usage:"Disable HTTP authentication entirely, even on non-loopback binds (or TERMCP_DISABLE_AUTH_TOKEN=1). Errors out if a token or hash is also configured."`
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
	if env := strings.TrimSpace(os.Getenv(EnvThemesDir)); env != "" {
		return filepath.Clean(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".termcp", "themes"), nil
}

// ApplyEnv moved to bind.go: environment variables are read from the struct's
// env tags, so a field cannot have a flag and forget its variable (or the
// reverse) any more. config.ApplyEnv(fs, cfg) is the replacement, and it must
// run AFTER flag.Parse so "did the user type it" is a question with an answer.

// ValidateInstance checks the settings an instance needs no matter how it is
// reached: where it stores things, how it logs, and that its credentials do not
// contradict each other.
//
// IT SAYS NOTHING ABOUT A PORT, because a port is a property of LISTENING. The
// split exists because a program that never binds one was being forced to invent
// a free port it does not use — which is the symptom of asking a construction
// question that only the serve step can answer.
func (c *Config) ValidateInstance() error {
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
	// Disabling auth is a deliberate operator decision, so it never silently
	// wins: supplying credentials *and* asking for no auth is a configuration
	// mistake and reported instead of guessed at.
	if c.DisableAuth {
		switch {
		case c.AuthToken != "":
			return fmt.Errorf("auth is disabled (--disable-auth / $%s) but --auth-token / $%s is also set; remove one of them", EnvDisableAuth, EnvAuthToken)
		case c.AuthHash != "":
			return fmt.Errorf("auth is disabled (--disable-auth / $%s) but --auth-hash / $%s is also set; remove one of them", EnvDisableAuth, EnvAuthHash)
		}
	}
	return nil
}

// Validate checks what BINDING a port requires on top of the instance settings: a
// usable port, and a non-loopback address that is protected by credentials.
//
// A non-loopback listener must be protected by an auth token or hash;
// loopback-only binds retain the convenient unauthenticated development default.
func (c *Config) Validate() error {
	if err := c.ValidateInstance(); err != nil {
		return err
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.DisableAuth {
		return nil
	}
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
