package forward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// ErrNotFound reports that no forward with the given id exists. Callers can
// branch on it with errors.Is instead of matching the message.
var ErrNotFound = errors.New("not found")

// ForwardManager manages port forwards for SSH connections.
//
// One forward is one independent entry keyed by id, so the table is a sync.Map;
// the change callback is a single value read on every mutation, so it is an
// atomic.Pointer rather than something a lock has to guard.
type ForwardManager struct {
	forwards sync.Map // id string -> *forwardState
	onChange atomic.Pointer[func()]
}

// NewForwardManager creates a ForwardManager.
func NewForwardManager() *ForwardManager {
	return &ForwardManager{}
}

// SetOnChange sets a callback invoked when forwards are added or removed.
func (fm *ForwardManager) SetOnChange(fn func()) {
	if fn == nil {
		fm.onChange.Store(nil)
		return
	}
	fm.onChange.Store(&fn)
}

func (fm *ForwardManager) notifyChange() {
	if fn := fm.onChange.Load(); fn != nil && *fn != nil {
		(*fn)()
	}
}

// ForwardDirection describes the type of a port forward.
type ForwardDirection string

const (
	DirectionLocal   ForwardDirection = "local"
	DirectionRemote  ForwardDirection = "remote"
	DirectionDynamic ForwardDirection = "dynamic"
)

// ForwardInfo is the public-facing view of a port forward (returned in JSON APIs).
type ForwardInfo struct {
	ForwardID  string           `json:"forward_id"`
	SessionID  string           `json:"session_id"`
	Direction  ForwardDirection `json:"direction"`
	SSHConfig  string           `json:"ssh_config"`
	ListenAddr string           `json:"listen_addr"`
	TargetAddr string           `json:"target_addr"`
	Status     string           `json:"status"`
	CreatedAt  int64            `json:"created_at"` // Unix ms
}

// forwardState holds the runtime state of a port forward.
type forwardState struct {
	ForwardInfo
	listener   net.Listener
	cancelFunc context.CancelFunc
}

// forwardCounter is atomic rather than a mutex around an int: one counter is one
// atomic, and a lock that guards a single scalar is a deadlock waiting for a
// reason to happen.
var forwardCounter atomic.Uint64

// ForwardID generates a unique forward id.
func ForwardID(direction ForwardDirection) string {
	n := forwardCounter.Add(1)
	return fmt.Sprintf("fw-%s-%d-%d", direction, clock.Now(), n)
}

// put registers a forward after SessionID/SSHConfig are set on fw.
// Callers must set metadata before put; the map stores a copy of ForwardInfo.
func (fm *ForwardManager) put(fw *ForwardInfo, ln net.Listener, cancel context.CancelFunc) {
	if fw == nil {
		return
	}
	fm.forwards.Store(fw.ForwardID, &forwardState{ForwardInfo: *fw, listener: ln, cancelFunc: cancel})
	fm.notifyChange()
}

// CreateLocal creates a local forward (ssh -L) and registers it under sessionID.
// bindHost is the local address to listen on, empty for loopback (see
// ResolveBindHost); a non-loopback address publishes the tunnel to the network.
func (fm *ForwardManager) CreateLocal(sessionID, sshConfig, bindHost, remoteHost string, remotePort, localPort int, sshClient *ssh.Client) (*ForwardInfo, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := LocalForwardSSH(ctx, sshClient, bindHost, remoteHost, remotePort, localPort)
	if err != nil {
		cancel()
		return nil, err
	}
	fw.SessionID = sessionID
	fw.SSHConfig = sshConfig
	fm.put(fw, ln, cancel)
	return fw, nil
}

// CreateRemote creates a remote forward (ssh -R) and registers it under sessionID.
func (fm *ForwardManager) CreateRemote(sessionID, sshConfig, localHost string, localPort int, remoteHost string, remotePort int, sshClient *ssh.Client) (*ForwardInfo, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := RemoteForwardSSH(ctx, sshClient, localHost, localPort, remoteHost, remotePort)
	if err != nil {
		cancel()
		return nil, err
	}
	fw.SessionID = sessionID
	fw.SSHConfig = sshConfig
	fm.put(fw, ln, cancel)
	return fw, nil
}

// CreateDynamic creates a SOCKS5 dynamic forward under sessionID.
// When localDial is true, connections are dialed from termcp itself (internal);
// otherwise they go through sshClient (ssh -D).
//
// bindHost is refused unless it is loopback, and that refusal is deliberate: the
// SOCKS5 handshake this server implements performs no authentication (RFC 1928
// method 0, matching `ssh -D`), so a listener on any other interface is an open
// proxy for every host that can reach it. A caller that wants to publish a
// service should use a local forward, which carries one fixed target rather than
// "dial anything".
func (fm *ForwardManager) CreateDynamic(sessionID, sshConfig, bindHost string, localPort int, sshClient *ssh.Client, localDial bool) (*ForwardInfo, error) {
	if !IsLoopbackHost(bindHost) {
		return nil, fmt.Errorf("SOCKS5 proxy %q cannot bind a non-loopback address: it performs no authentication, so it would be an open proxy for anything that can reach this host (use a local forward to publish one service)", ResolveBindHost(bindHost))
	}
	if !localDial && sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	if localDial {
		return fm.dynamicLocal(sessionID, sshConfig, bindHost, localPort)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := DynamicForwardSSH(ctx, sshClient, bindHost, localPort)
	if err != nil {
		cancel()
		return nil, err
	}
	fw.SessionID = sessionID
	fw.SSHConfig = sshConfig
	fm.put(fw, ln, cancel)
	return fw, nil
}

// List returns all forward infos.
func (fm *ForwardManager) List() []ForwardInfo {
	var out []ForwardInfo
	fm.forwards.Range(func(_, v any) bool {
		if fw, ok := v.(*forwardState); ok {
			out = append(out, fw.ForwardInfo)
		}
		return true
	})
	return out
}

// ListBySession returns forwards belonging to sessionID.
func (fm *ForwardManager) ListBySession(sessionID string) []ForwardInfo {
	var out []ForwardInfo
	fm.forwards.Range(func(_, v any) bool {
		if fw, ok := v.(*forwardState); ok && fw.SessionID == sessionID {
			out = append(out, fw.ForwardInfo)
		}
		return true
	})
	return out
}

// CloseBySession closes all forwards belonging to the given session ID.
func (fm *ForwardManager) CloseBySession(sessionID string) {
	var toClose []*forwardState
	fm.forwards.Range(func(k, v any) bool {
		fw, ok := v.(*forwardState)
		if !ok {
			return true
		}
		if fw.SessionID == sessionID {
			toClose = append(toClose, fw)
			fm.forwards.Delete(k)
		}
		return true
	})
	for _, fw := range toClose {
		slog.Info("forward: closing on session terminate", "forward_id", fw.ForwardID, "session_id", sessionID)
		if fw.cancelFunc != nil {
			fw.cancelFunc()
		}
		if fw.listener != nil {
			fw.listener.Close()
		}
	}
	if len(toClose) > 0 {
		fm.notifyChange()
	}
}

// Close closes a forward by ID and cleans up resources.
func (fm *ForwardManager) Close(forwardID string) error {
	v, ok := fm.forwards.Load(forwardID)
	if !ok {
		slog.Warn("forward close: not found", "forward_id", forwardID)
		return fmt.Errorf("forward %q %w", forwardID, ErrNotFound)
	}
	fm.forwards.Delete(forwardID)
	fw, _ := v.(*forwardState)

	slog.Info("forward close: removed from map", "forward_id", forwardID, "has_cancel", fw.cancelFunc != nil, "has_listener", fw.listener != nil)
	if fw.cancelFunc != nil {
		fw.cancelFunc()
		slog.Info("forward close: cancel called", "forward_id", forwardID)
	}
	if fw.listener != nil {
		slog.Info("forward close: closing listener", "forward_id", forwardID, "listen_addr", fmt.Sprintf("%v", fw.listener.Addr()))
		err := fw.listener.Close()
		slog.Info("forward close: listener closed", "forward_id", forwardID, "err", err)
	}
	fm.notifyChange()
	return nil
}
