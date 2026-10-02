package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(probe)
}

func runGenAuthHash(args []string) error {
	var token string
	switch len(args) {
	case 0:
		t, err := readTokenFromStdin()
		if err != nil {
			return err
		}
		token = t
	case 1:
		token = args[0]
	default:
		return errors.New("too many arguments: expected at most one token")
	}
	if token == "" {
		return errors.New("token must not be empty")
	}
	hash, err := auth.Hash(token)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}

func readTokenFromStdin() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Token: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
