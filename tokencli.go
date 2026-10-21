package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/daemon"
)

// runTokenCLI dispatches `termcp token <action>`. Tokens live in the running
// instance's table, so all three verbs are REST calls with the operator
// credential (token management is operator-only server-side).
//
// The namespace carries its own FlagSet: create's options (--tools, --ttl, …)
// are not global flags, and the global parser would choke on them before we
// ran. Shared connection flags behave exactly as elsewhere.
func runTokenCLI(cfg *config.Config, action string, rest []string) {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.StringVar(&cfg.Host, "host", cfg.Host, "instance address")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "instance port")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory")
	fs.StringVar(&cfg.AuthToken, "auth-token", cfg.AuthToken, "operator token")
	fs.StringVar(&cfg.AuthHash, "auth-hash", cfg.AuthHash, "operator token hash")
	var kind, tools, sessions, ttl string
	var approvalRequired bool
	fs.StringVar(&kind, "kind", "agent", "create: human|agent")
	fs.StringVar(&tools, "tools", "", "create: comma-separated capability ids (shell.input, ...)")
	fs.StringVar(&sessions, "sessions", "", "create: comma-separated session ids or profile names")
	fs.StringVar(&ttl, "ttl", "", "create: lifetime like 1h (empty = no expiry)")
	fs.BoolVar(&approvalRequired, "approval-required", false, "create: every write waits for a human decision")
	if action != "" && action != actionHelp {
		if err := fs.Parse(rest); err != nil {
			os.Exit(2)
		}
	}

	var err error
	switch action {
	case "", actionHelp:
		printTokenBrief(os.Stdout)
		return
	case "create":
		err = tokenCreate(cfg, kind, tools, sessions, ttl, approvalRequired)
	case "list", "ls":
		err = tokenList(cfg)
	case "revoke", "rm":
		if fs.NArg() != 1 {
			err = fmt.Errorf("usage: termcp token revoke [flags] <id> (flags before the id)")
		} else {
			err = tokenRevoke(cfg, fs.Arg(0))
		}
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "token %s: %v\n", action, err)
		os.Exit(1)
	}
}

func printTokenBrief(w io.Writer) {
	fmt.Fprintln(w, "termcp token — scoped tokens for agents/harnesses (issue #105)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  create --kind agent [--tools shell.input,shell.output] [--sessions id,...]")
	fmt.Fprintln(w, "         [--approval-required] [--ttl 1h]   mint a token (printed ONCE)")
	fmt.Fprintln(w, "  list                 show the table (verifiers never displayed)")
	fmt.Fprintln(w, "  revoke <id>          revoke, effective on the next request")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Tools name capability ids (shell.input, file.write, ...), not tool names.")
}

func tokenCreate(cfg *config.Config, kind, tools, sessions, ttl string, approvalRequired bool) error {
	req := map[string]any{"kind": kind}
	if tools != "" {
		req["tools"] = strings.Split(tools, ",")
	}
	if sessions != "" {
		req["sessions"] = strings.Split(sessions, ",")
	}
	if approvalRequired {
		req["approval_required"] = true
	}
	if ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return fmt.Errorf("--ttl: %v", err)
		}
		req["ttl_seconds"] = int64(d.Seconds())
	}
	code, body, err := tokenAPI(cfg, http.MethodPost, "/api/tokens", req)
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var resp struct {
		Token  string          `json:"token"`
		Record json.RawMessage `json:"record"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	fmt.Println(resp.Token)
	fmt.Fprintln(os.Stderr, "the token is printed once; store it somewhere safe")
	return nil
}

func tokenList(cfg *config.Config) error {
	code, body, err := tokenAPI(cfg, http.MethodGet, "/api/tokens", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var resp struct {
		Tokens []struct {
			ID               string   `json:"id"`
			Kind             string   `json:"kind"`
			Tools            []string `json:"tools"`
			Sessions         []string `json:"sessions"`
			ApprovalRequired bool     `json:"approval_required"`
			ExpiresAt        string   `json:"expires_at"`
			RevokedAt        string   `json:"revoked_at"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	if len(resp.Tokens) == 0 {
		fmt.Println("no scoped tokens")
		return nil
	}
	for _, t := range resp.Tokens {
		state := "active"
		if t.RevokedAt != "" && !strings.HasPrefix(t.RevokedAt, "0001-01-01") {
			state = "revoked " + t.RevokedAt
		}
		scope := "all operations"
		if len(t.Tools) > 0 {
			scope = "tools " + strings.Join(t.Tools, ",")
		}
		if len(t.Sessions) > 0 {
			scope += " sessions " + strings.Join(t.Sessions, ",")
		}
		if t.ApprovalRequired {
			scope += " +approval"
		}
		exp := "never"
		if t.ExpiresAt != "" && !strings.HasPrefix(t.ExpiresAt, "0001-01-01") {
			exp = t.ExpiresAt
		}
		fmt.Printf("%-14s %-6s %-24s expires %-25s %s\n", t.ID, t.Kind, scope, exp, state)
	}
	return nil
}

func tokenRevoke(cfg *config.Config, id string) error {
	code, body, err := tokenAPI(cfg, http.MethodDelete, "/api/tokens/"+id, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent:
		fmt.Println("revoked: " + id)
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("no such token id %q", id)
	default:
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
}

// tokenAPI is the token CLI's REST client, sibling of vaultAPI.
func tokenAPI(cfg *config.Config, method, path string, body map[string]any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
		return 0, nil, fmt.Errorf("no instance answering at %s: %w", daemon.BaseURL(cfg.Host, cfg.Port), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return 0, nil, fmt.Errorf("forbidden: token management needs the operator credential")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return 0, nil, fmt.Errorf("unauthorized: pass the operator token with --auth-token")
	}
	return resp.StatusCode, data, nil
}
