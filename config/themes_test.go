package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestThemesDirectoryIndependentOfDataAndAssets(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv(EnvAssetsDir, t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("home directory unavailable")
	}
	got, err := DefaultThemesDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".termcp", "themes"); got != want {
		t.Fatalf("theme directory = %q, want %q", got, want)
	}
}
