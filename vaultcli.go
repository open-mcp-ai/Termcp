package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/vault"
)

// runVaultCLI dispatches `termcp vault <action>`. init, migrate, reseal and
// the recipients verbs work on the data directory directly; unlock, lock and
// status talk to the running instance over its REST API, because the vault's
// unlocked state is per-process memory — only the daemon can unlock itself.
//
// Like the token namespace it parses its own FlagSet: actions carry
// positionals (a recipient name, an age1… key) the global parser would choke
// on or swallow. Shared connection flags must come before the positionals.
func runVaultCLI(cfg *config.Config, action string, rest []string) {
	fs := flag.NewFlagSet("vault", flag.ContinueOnError)
	fs.StringVar(&cfg.Host, "host", cfg.Host, "instance address")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "instance port")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory")
	fs.StringVar(&cfg.AuthToken, "auth-token", cfg.AuthToken, "operator token")
	fs.StringVar(&cfg.AuthHash, "auth-hash", cfg.AuthHash, "operator token hash")
	if action != "" && action != actionHelp {
		if err := fs.Parse(rest); err != nil {
			os.Exit(2)
		}
		for _, a := range fs.Args() {
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "vault %s: flags come before the action's arguments (got %q in the tail)\n", action, a)
				os.Exit(2)
			}
		}
	}
	args := fs.Args()

	var err error
	switch action {
	case "", actionHelp:
		printVaultBrief(os.Stdout)
		return
	case actionInit:
		err = vaultInit(cfg)
	case actionUnlock:
		var pw string
		pw, err = promptSecret("Vault passphrase: ")
		if err == nil && pw == "" {
			err = errors.New("passphrase must not be empty")
		}
		if err == nil {
			err = vaultRemote(cfg, http.MethodPost, "/api/vault/unlock", map[string]string{"passphrase": pw})
		}
	case actionLock:
		err = vaultRemote(cfg, http.MethodPost, "/api/vault/lock", nil)
	case actionStatus:
		err = vaultStatus(cfg)
	case actionMigrate:
		err = vaultMigrate(cfg)
	case "reseal":
		err = vaultReseal(cfg)
	case actionRecipients:
		err = vaultRecipients(cfg, args)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault %s: %v\n", action, err)
		os.Exit(1)
	}
}

func printVaultBrief(w io.Writer) {
	fmt.Fprintln(w, "termcp vault — the encrypted credential vault (issue #105)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  init                 create the vault (prompts for a passphrase; mirrors to the OS keychain when available)")
	fmt.Fprintln(w, "  unlock               unlock the running instance (prompts; keychain unlocks happen at startup)")
	fmt.Fprintln(w, "  lock                 lock the running instance")
	fmt.Fprintln(w, "  status               show vault state (initialized/unlocked, sealed profiles)")
	fmt.Fprintln(w, "  migrate              seal every plaintext profile into the vault and scrub the TOML")
	fmt.Fprintln(w, "  reseal               re-encrypt all profiles to the current recipients (after recipients add/remove)")
	fmt.Fprintln(w, "  recipients           list devices; `recipients add <name> <age1...> [note]` / `remove <name>`")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Reads and writes while locked fail with guidance; listing names always works.")
}

// vaultInit creates the vault offline. The passphrase is the portable
// fallback; when the OS keychain answers, the identity is mirrored there and
// later daemons unlock without prompting.
func vaultInit(cfg *config.Config) error {
	v := vault.New(cfg.DataDir)
	if v.Exists() {
		return errors.New("vault already initialized (see `termcp vault status`)")
	}
	pw, err := promptTwice()
	if err != nil {
		return err
	}
	recipient, err := v.Init(pw)
	if err != nil {
		return err
	}
	fmt.Println("vault initialized")
	fmt.Printf("  this device's recipient: %s\n", recipient)
	if v.CanAutoUnlock() {
		fmt.Println("  auto-unlock: OS credential store holds the identity; daemons unlock at startup")
	} else {
		fmt.Println("  auto-unlock: unavailable (no OS credential store) — unlock per start with `termcp vault unlock`")
		fmt.Println("               or set TERMCP_VAULT_NO_KEYCHAIN=1 to silence keychain probes")
	}
	fmt.Println("next: `termcp vault migrate` seals existing plaintext profiles; new writes seal automatically")
	return nil
}

// vaultStatus prefers the running instance's answer (it knows the lock
// state); with nothing answering it reports what the data dir can prove.
func vaultStatus(cfg *config.Config) error {
	code, body, err := vaultAPI(cfg, http.MethodGet, "/api/vault", nil)
	if err == nil && code == http.StatusOK {
		var st struct {
			Initialized   bool     `json:"initialized"`
			Unlocked      bool     `json:"unlocked"`
			CanAutoUnlock bool     `json:"can_auto_unlock"`
			Profiles      []string `json:"profiles"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return err
		}
		fmt.Printf("vault: %s\n", vaultStateWords(st.Initialized, st.Unlocked))
		if st.Initialized {
			fmt.Printf("  auto-unlock: %v\n", st.CanAutoUnlock)
			fmt.Printf("  sealed profiles: %s\n", strings.Join(st.Profiles, ", "))
		}
		return nil
	}
	// No instance answering: the data dir still proves existence and names.
	v := vault.New(cfg.DataDir)
	if !v.Exists() {
		fmt.Println("vault: not initialized (run `termcp vault init`)")
		return nil
	}
	names, _ := v.List()
	fmt.Println("vault: initialized, no running instance to report the lock state")
	fmt.Printf("  sealed profiles: %s\n", strings.Join(names, ", "))
	return nil
}

func vaultStateWords(initialized, unlocked bool) string {
	switch {
	case !initialized:
		return "not initialized"
	case unlocked:
		return "unlocked"
	default:
		return "locked"
	}
}

// vaultMigrate seals all plaintext profiles. It runs offline against the data
// dir: unlock happens in this process (keychain first, then a prompt), and a
// running instance picks the sealed files up on its next read.
func vaultMigrate(cfg *config.Config) error {
	v := vault.New(cfg.DataDir)
	if !v.Exists() {
		return errors.New("no vault: run `termcp vault init` first")
	}
	if err := v.UnlockAuto(); err != nil {
		pw, err := promptSecret("Vault passphrase: ")
		if err != nil {
			return err
		}
		if err := v.Unlock(pw); err != nil {
			return err
		}
	}
	st := sshconfig.NewStore(cfg.DataDir)
	st.SetVault(v)
	res, err := st.MigrateToVault()
	if err != nil {
		return err
	}
	switch {
	case len(res.Migrated) == 0 && len(res.Failed) == 0:
		fmt.Println("nothing to migrate: no plaintext profiles found")
	default:
		if len(res.Migrated) > 0 {
			fmt.Printf("migrated and scrubbed: %s\n", strings.Join(res.Migrated, ", "))
		}
		for _, f := range res.Failed {
			fmt.Fprintf(os.Stderr, "  failed: %s: %v\n", f.Name, f.Err)
		}
	}
	if len(res.Failed) > 0 {
		return fmt.Errorf("%d profile(s) failed; plaintext kept for each", len(res.Failed))
	}
	return nil
}

// vaultReseal re-encrypts every sealed profile to the current recipients
// table — the multi-device verb: a newly added device opens the vault's
// existing files only after a reseal.
func vaultReseal(cfg *config.Config) error {
	v := vault.New(cfg.DataDir)
	if !v.Exists() {
		return errors.New("no vault: run `termcp vault init` first")
	}
	if !v.IsUnlocked() {
		if err := v.UnlockAuto(); err != nil {
			pw, err := promptSecret("Vault passphrase: ")
			if err != nil {
				return err
			}
			if err := v.Unlock(pw); err != nil {
				return err
			}
		}
	}
	n, err := v.ResealAll()
	if err != nil {
		return err
	}
	fmt.Printf("resealed %d profile(s) to the current recipients\n", n)
	return nil
}

// vaultRecipients edits the public device table. It needs no unlock: the
// table holds public keys only.
func vaultRecipients(cfg *config.Config, args []string) error {
	v := vault.New(cfg.DataDir)
	if !v.Exists() {
		return errors.New("no vault: run `termcp vault init` first")
	}
	if len(args) == 0 {
		recs, err := v.Recipients()
		if err != nil {
			return err
		}
		fmt.Println("recipients (devices this vault seals to):")
		for _, r := range recs {
			note := ""
			if r.Note != "" {
				note = " — " + r.Note
			}
			fmt.Printf("  %-16s %s%s\n", r.Name, r.Recipient, note)
		}
		return nil
	}
	switch args[0] {
	case "add":
		if len(args) < 3 {
			return errors.New("usage: termcp vault recipients add <name> <age1...> [note]")
		}
		if err := v.AddRecipient(args[1], args[2], strings.Join(args[3:], " ")); err != nil {
			return err
		}
		fmt.Printf("recipient %q added; new writes seal to it. Re-seal existing profiles by re-saving them\n", args[1])
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return errors.New("usage: termcp vault recipients remove <name>")
		}
		if err := v.RemoveRecipient(args[1]); err != nil {
			return err
		}
		fmt.Printf("recipient %q removed; future writes skip it, existing ciphertext still opens for it\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown recipients action %q (add | remove | bare to list)", args[0])
	}
}

// vaultRemote drives unlock/lock on the running instance.
func vaultRemote(cfg *config.Config, method, path string, body map[string]string) error {
	code, respBody, err := vaultAPI(cfg, method, path, body)
	if err != nil {
		return fmt.Errorf("no instance answering at %s (start one, or use `termcp vault migrate` offline): %w",
			daemon.BaseURL(cfg.Host, cfg.Port), err)
	}
	switch code {
	case http.StatusNoContent, http.StatusOK:
		fmt.Println(strings.TrimPrefix(path, "/api/vault/") + ": ok")
		return nil
	case http.StatusNotFound:
		return errors.New("the instance has no vault (run `termcp vault init` first)")
	case http.StatusUnauthorized:
		return errors.New("wrong passphrase (or the instance's credential was rejected — check --auth-token)")
	case http.StatusConflict:
		return fmt.Errorf("conflict: %s", strings.TrimSpace(string(respBody)))
	default:
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(respBody)))
	}
}

// vaultAPI is the CLI's thin REST client for the vault endpoints: the same
// host/port/credential resolution the daemon management commands use.
func vaultAPI(cfg *config.Config, method, path string, body map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, daemon.BaseURL(cfg.Host, cfg.Port)+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if cred := daemon.Credential(cfg); cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

// promptSecret reads a passphrase without echo on a terminal, or one line
// from a pipe — the same convention as --gen-auth-hash's token input.
func promptSecret(prompt string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read passphrase: %w", err)
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read passphrase from stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// promptTwice prompts with confirmation on a terminal; a piped passphrase is
// read once (there is nobody to confirm to).
func promptTwice() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return promptSecret("")
	}
	first, err := promptSecret("New vault passphrase: ")
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", errors.New("passphrase must not be empty")
	}
	second, err := promptSecret("Confirm passphrase: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("passphrases do not match")
	}
	return first, nil
}
