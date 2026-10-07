package sshclient

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// startSession sets up pipes, optional PTY, starts the command, and returns an ExecSession.
// When ownClient is false, error paths close session but not client.
func startSession(client *ssh.Client, session *ssh.Session, command string, args []string, pty bool, rows, cols int, ownClient bool) (*ExecSession, error) {
	closeClient := func() {
		if ownClient {
			client.Close()
		}
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		closeClient()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		stdin.Close()
		session.Close()
		closeClient()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		closeIfCloser(stdout)
		stdin.Close()
		session.Close()
		closeClient()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if pty {
		if err := session.RequestPty("xterm-256color", rows, cols, defaultPTYModes()); err != nil {
			closeIfCloser(stderr)
			closeIfCloser(stdout)
			stdin.Close()
			session.Close()
			closeClient()
			return nil, fmt.Errorf("request pty: %w", err)
		}
	}

	var startErr error
	if trimmed := strings.TrimSpace(command); trimmed == "" && len(args) == 0 {
		if !pty {
			// Pipe mode has no "give me a login shell" request: it can only exec a
			// concrete command. Guessing one from THIS host's detector sends the
			// local path (e.g. C:\Program Files\WindowsApps\…\pwsh.exe) to the
			// remote sshd, which answers "command not found". Refuse instead.
			err := fmt.Errorf("empty command in pipe mode: a pipe shell runs a command, not a login shell")
			closeIfCloser(stderr)
			closeIfCloser(stdout)
			stdin.Close()
			session.Close()
			closeClient()
			return nil, err
		}
		// PTY: the shell request lets the SERVER decide its login shell. The
		// client never supplies one — a path from the local detector is only valid
		// when the target is this host, and the client cannot ask which it is.
		startErr = session.Shell()
	} else {
		startErr = session.Start(shellQuote(trimmed, args))
	}
	if startErr != nil {
		closeIfCloser(stderr)
		closeIfCloser(stdout)
		stdin.Close()
		session.Close()
		closeClient()
		return nil, fmt.Errorf("start command: %w", startErr)
	}

	es := &ExecSession{
		client:    client,
		session:   session,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		done:      make(chan struct{}),
		ownClient: ownClient,
	}

	go func() {
		es.err = session.Wait()
		if es.err != nil {
			if exitErr, ok := es.err.(*ssh.ExitError); ok {
				es.exitCode = exitErr.ExitStatus()
			}
		}
		close(es.done)
	}()

	return es, nil
}
