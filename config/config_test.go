package config

import (
	"strings"
	"testing"
)

func TestDefault_HostBindsAllInterfaces(t *testing.T) {
	cfg := Default()
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("expected Host 127.0.0.1, got %q", cfg.Host)
	}
	if cfg.Port != 18765 {
		t.Fatalf("expected Port 18765, got %d", cfg.Port)
	}
}

func TestDefault_HasInfoLevel(t *testing.T) {
	cfg := Default()
	if cfg.LogLevel != "info" {
		t.Fatalf("expected LogLevel \"info\", got %q", cfg.LogLevel)
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	for _, port := range []int{-1, 0, 65536, 99999} {
		cfg := &Config{Host: "127.0.0.1", Port: port, DataDir: "/tmp/data"}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for port %d", port)
		}
	}
}

func TestValidate_EmptyDataDir(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: ""}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty DataDir")
	}
}

func TestValidate_RejectsUnknownLogLevel(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", LogLevel: "bogus"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown LogLevel")
	}
}

func TestValidate_AcceptsAllValidLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", LogLevel: level}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected %s to validate, got: %v", level, err)
		}
	}
}

func TestValidate_AuthTokenAndHashAreMutuallyExclusive(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", AuthToken: "plain", AuthHash: "sha256-aa-bb"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when both AuthToken and AuthHash are set")
	}
}

func TestValidate_NonLoopbackRequiresAuth(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "[::]", "", "192.168.1.25", "myserver.local"} {
		cfg := &Config{Host: host, Port: 8080, DataDir: "/tmp/data"}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected auth-required error for host %q", host)
		}
	}
}

func TestValidate_NonLoopbackWithAuthOK(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.25"} {
		for _, auth := range []*Config{{AuthToken: "tok"}, {AuthHash: "sha256-aa-68f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0"}} {
			cfg := &Config{Host: host, Port: 8080, DataDir: "/tmp/data", AuthToken: auth.AuthToken, AuthHash: auth.AuthHash}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("host %q with auth %+v: unexpected error: %v", host, auth, err)
			}
		}
	}
}

func TestValidate_LoopbackAllowsNoAuth(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "[::1]", "localhost", "LOCALHOST"} {
		cfg := &Config{Host: host, Port: 8080, DataDir: "/tmp/data"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("host %q should validate without auth, got error: %v", host, err)
		}
	}
}

func TestApplyEnv_PopulatesAuthFromEnv(t *testing.T) {
	t.Setenv(EnvAuthToken, "env-token")
	t.Setenv(EnvAuthHash, "env-hash")
	cfg := Default()
	cfg.ApplyEnv()
	if cfg.AuthToken != "env-token" || cfg.AuthHash != "env-hash" {
		t.Fatalf("ApplyEnv did not read env: token=%q hash=%q", cfg.AuthToken, cfg.AuthHash)
	}
}

func TestApplyEnv_FlagValueWins(t *testing.T) {
	t.Setenv(EnvAuthToken, "env-token")
	cfg := Default()
	cfg.AuthToken = "flag-token" // set by flag.Parse before ApplyEnv's callers run
	cfg.ApplyEnv()
	if cfg.AuthToken != "flag-token" {
		t.Fatalf("ApplyEnv overwrote flag value: got %q", cfg.AuthToken)
	}
}

func TestApplyEnv_BlankEnvCleared(t *testing.T) {
	t.Setenv(EnvAuthToken, "   ")
	t.Setenv(EnvAuthHash, "")
	cfg := Default()
	cfg.ApplyEnv()
	if cfg.AuthToken != "" || cfg.AuthHash != "" {
		t.Fatalf("blank env should be ignored, got token=%q hash=%q", cfg.AuthToken, cfg.AuthHash)
	}
}

func TestApplyEnv_DisableAuthFromEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		t.Setenv(EnvDisableAuth, v)
		cfg := Default()
		cfg.ApplyEnv()
		if !cfg.DisableAuth {
			t.Errorf("%s=%q should enable DisableAuth", EnvDisableAuth, v)
		}
	}
}

// A falsy-looking value must not read as "disable auth": an operator who writes
// =0 to turn the switch back off should not accidentally open the server.
func TestApplyEnv_DisableAuthFalsyValuesIgnored(t *testing.T) {
	for _, v := range []string{"", "0", "false", "no", "off", "   "} {
		t.Setenv(EnvDisableAuth, v)
		cfg := Default()
		cfg.ApplyEnv()
		if cfg.DisableAuth {
			t.Errorf("%s=%q must not enable DisableAuth", EnvDisableAuth, v)
		}
	}
}

func TestValidate_DisableAuthAllowsNonLoopback(t *testing.T) {
	cfg := &Config{Host: "0.0.0.0", Port: 8080, DataDir: "/tmp/data", DisableAuth: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DisableAuth should permit a non-loopback bind, got: %v", err)
	}
}

// Supplying credentials *and* disabling auth is contradictory, and guessing
// which one the operator meant is exactly the kind of ambiguity that leaves an
// exposed server open by accident. Report it instead.
func TestValidate_DisableAuthConflictsWithToken(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", DisableAuth: true, AuthToken: "tok"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error when DisableAuth and AuthToken are both set")
	}
	if !strings.Contains(err.Error(), "auth-token") {
		t.Fatalf("error should name the conflicting setting, got: %v", err)
	}
}

func TestValidate_DisableAuthConflictsWithHash(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", DisableAuth: true, AuthHash: "sha256-aa-bb"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error when DisableAuth and AuthHash are both set")
	}
	if !strings.Contains(err.Error(), "auth-hash") {
		t.Fatalf("error should name the conflicting setting, got: %v", err)
	}
}
