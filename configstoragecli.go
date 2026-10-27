package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/configstore"
)

func runConfigStorageBrief(w io.Writer) {
	fmt.Fprintln(w, "termcp config-storage - manage encrypted connection profiles")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions (the action goes right after the subcommand):")
	fmt.Fprintln(w, "  termcp config-storage status   inspect the selected data directory")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `termcp config-storage --help` for the provider and safety details.")
}

func runConfigStorageHelp(w io.Writer) {
	fmt.Fprintln(w, "termcp config-storage - manage encrypted connection profiles")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  termcp config-storage status   report keys, revision, and profile count")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Encryption changes are made in the Web UI, where hosts can be selected")
	fmt.Fprintln(w, "individually. This command only inspects the configured data directory.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Unlock provider precedence:")
	fmt.Fprintln(w, "  TERMCP_CONFIG_KEK  strict base64 for one 32-byte key; used alone")
	fmt.Fprintln(w, "  otherwise          the operating system credential store")
}

func runConfigStorageCommand(cfg *config.Config, action string) error {
	switch action {
	case actionStatus:
		return runConfigStorageStatus(cfg)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

func runConfigStorageStatus(cfg *config.Config) error {
	keys := filepath.Join(cfg.DataDir, "keys", "config.keys")
	if _, err := os.Stat(keys); errors.Is(err, os.ErrNotExist) {
		fmt.Println("config storage: plaintext or uninitialized")
		fmt.Printf("  data-dir: %s\n", cfg.DataDir)
		legacy, legacyErr := configstore.HasLegacyEncryptionArtifacts(cfg.DataDir)
		if legacyErr != nil {
			return legacyErr
		}
		if legacy {
			fmt.Println("  warning:   incompatible encryption-test files are present and ignored")
		}
		return nil
	} else if err != nil {
		return err
	}
	repo, err := configstore.OpenRepository(cfg.DataDir, configStorageProviders()...)
	if err != nil {
		if errors.Is(err, configstore.ErrProviderUnavailable) ||
			errors.Is(err, configstore.ErrAuthentication) {
			fmt.Println("config storage: encrypted (locked)")
			fmt.Printf("  data-dir: %s\n", cfg.DataDir)
			return nil
		}
		return err
	}
	defer repo.Close()
	status := repo.Status()
	fmt.Println("config storage: encrypted (ready)")
	fmt.Printf("  data-dir:  %s\n", cfg.DataDir)
	fmt.Printf("  store:     %x\n", status.StoreID)
	fmt.Printf("  revision:  %d\n", status.Revision)
	fmt.Printf("  encrypted: %d\n", status.Profiles)
	encryptNew, err := configstore.LoadEncryptNew(cfg.DataDir)
	if err != nil {
		return err
	}
	mode := "plaintext"
	if encryptNew {
		mode = "encrypted"
	}
	fmt.Printf("  new mode:  %s\n", mode)
	return nil
}

func configStorageProviders() []configstore.Provider {
	if os.Getenv(configstore.EnvKEKVariable) != "" {
		return []configstore.Provider{configstore.NewEnvProvider("")}
	}
	return []configstore.Provider{configstore.SystemProvider()}
}
