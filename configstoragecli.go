package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/configstore"
	"golang.org/x/term"
)

func runConfigStorageBrief(w io.Writer) {
	fmt.Fprintln(w, "termcp config-storage - manage encrypted connection profiles")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions (the action goes right after the subcommand):")
	fmt.Fprintln(w, "  termcp config-storage status   inspect the selected data directory")
	fmt.Fprintln(w, "  termcp config-storage rotate-key   compatibility rotation for an old provider-only store")
	fmt.Fprintln(w, "  termcp config-storage verify-password   check the configuration password without changing state")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `termcp config-storage --help` for the provider and safety details.")
}

func runConfigStorageHelp(w io.Writer) {
	fmt.Fprintln(w, "termcp config-storage - manage encrypted connection profiles")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  termcp config-storage status   report keys, revision, and profile count")
	fmt.Fprintln(w, "  termcp config-storage rotate-key   compatibility rotation for an old provider-only store")
	fmt.Fprintln(w, "  termcp config-storage verify-password   verify the configuration password from stdin")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Select hosts, set a password, unlock, and change the password in the Web UI.")
	fmt.Fprintln(w, "Password-derived stores reject rotate-key; use the Web UI password form.")
	fmt.Fprintln(w, "Stop the server before running rotate-key on a legacy data directory.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Unlock sequence:")
	fmt.Fprintln(w, "  1. TERMCP_CONFIG_KEK, when set (strict base64 for one 32-byte key)")
	fmt.Fprintln(w, "  2. otherwise, the operating system credential store")
	fmt.Fprintln(w, "  3. when automatic unlocking is unavailable, enter the password in the Web UI")
}

func runConfigStorageCommand(cfg *config.Config, action string) error {
	switch action {
	case actionStatus:
		return runConfigStorageStatus(cfg)
	case actionRotateKey:
		repo, err := configstore.OpenRepository(cfg.DataDir, configStorageProviders()...)
		if err != nil {
			return err
		}
		defer repo.Close()
		if repo.Status().PasswordConfigured {
			return errors.New("change a password-derived Main Key using current/new password in the Web UI")
		}
		if err := repo.RotateMainKey(configStorageProviders()...); err != nil {
			return err
		}
		fmt.Printf("config storage: Main Key replaced (%x)\n", repo.Status().MainKeyID)
		return nil
	case actionVerifyPassword:
		return runConfigStorageVerifyPassword(cfg)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

func runConfigStorageStatus(cfg *config.Config) error {
	keys := filepath.Join(cfg.DataDir, "keys", "config.keys")
	localState := configstore.LocalStatePath(cfg.DataDir)
	_, keyErr := os.Stat(keys)
	_, stateErr := os.Stat(localState)
	if errors.Is(keyErr, os.ErrNotExist) && errors.Is(stateErr, os.ErrNotExist) {
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
	} else if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		return keyErr
	} else if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	}
	if errors.Is(keyErr, os.ErrNotExist) && stateErr == nil {
		fmt.Println("config storage: encrypted (local-state)")
		fmt.Printf("  data-dir: %s\n", cfg.DataDir)
		fmt.Println("  unlock:   enter the configuration password in the Web UI")
		return nil
	}
	repo, err := configstore.OpenRepository(cfg.DataDir, configStorageProviders()...)
	if err != nil {
		if errors.Is(err, configstore.ErrProviderUnavailable) ||
			errors.Is(err, configstore.ErrAuthentication) || errors.Is(err, configstore.ErrPasswordRequired) {
			fmt.Println("config storage: encrypted (locked)")
			fmt.Printf("  data-dir: %s\n", cfg.DataDir)
			fmt.Println("  unlock:   enter the encryption password in the Web UI")
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
	fmt.Printf("  main key:  %x\n", status.MainKeyID)
	if status.RotationPending {
		if status.PasswordConfigured {
			fmt.Println("  rotation:  pending; resume in the Web UI with the new password")
		} else {
			fmt.Println("  rotation:  pending; resume with config-storage rotate-key")
		}
	}
	mode := "plaintext"
	if status.EncryptNew {
		mode = "encrypted"
	}
	fmt.Printf("  new mode:  %s\n", mode)
	return nil
}

func runConfigStorageVerifyPassword(cfg *config.Config) error {
	fmt.Fprint(os.Stderr, "Configuration password: ")
	var password string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		password = string(raw)
		clear(raw)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		password = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	}
	defer func() {
		password = ""
	}()
	if password == "" {
		return errors.New("configuration password is required")
	}
	if err := configstore.VerifyDataDirPassword(cfg.DataDir, password); err != nil {
		return fmt.Errorf("configuration password was not accepted: %w", err)
	}
	fmt.Println("configuration password: accepted")
	return nil
}

func configStorageProviders() []configstore.Provider {
	if os.Getenv(configstore.EnvKEKVariable) != "" {
		return []configstore.Provider{configstore.NewEnvProvider("")}
	}
	return []configstore.Provider{configstore.SystemProvider()}
}
