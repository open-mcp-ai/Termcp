package forward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// ErrNotFound reports that no forward with the given id exists. Callers can
// branch on it with errors.Is instead of matching the message.
var ErrNotFound = errors.New("not found")

// ForwardManager manages port forwards for SSH connections.
type ForwardManager struct {
	mu       sync.Mutex
	forwards map[string]*forwardState
	onChange func()
}

// NewForwardManager creates a ForwardManager.
func NewForwardManager() *ForwardManager {
	return &ForwardManager{
		forwards: make(map[string]*forwardState),
	}
}

// SetOnChange sets a callback invoked when forwards are added or removed.
func (fm *ForwardManager) SetOnChange(fn func()) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.onChange = fn
}

func (fm *ForwardManager) notifyChange() {
	fm.mu.Lock()
	fn := fm.onChange
	fm.mu.Unlock()
	if fn != nil {
		fn()
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

var forwardCounter struct {
	mu    sync.Mutex
	count int
}

// ForwardID generates a unique forward id.
func ForwardID(direction ForwardDirection) string {
	forwardCounter.mu.Lock()
	defer forwardCounter.mu.Unlock()
	forwardCounter.count++
	return fmt.Sprintf("fw-%s-%d-%d", direction, clock.Now(), forwardCounter.count)
}

// put registers a forward after SessionID/SSHConfig are set on fw.
// Callers must set metadata before put; the map stores a copy of ForwardInfo.
func (fm *ForwardManager) put(fw *ForwardInfo, ln net.Listener, cancel context.CancelFunc) {
	if fw == nil {
		return
	}
	fm.mu.Lock()
	fm.forwards[fw.ForwardID] = &forwardState{ForwardInfo: *fw, listener: ln, cancelFunc: cancel}
	fm.mu.Unlock()
	fm.notifyChange()
}

// CreateLocal creates a local forward (ssh -L) and registers it under sessionID.
func (fm *ForwardManager) CreateLocal(sessionID, sshConfig, remoteHost string, remotePort, localPort int, sshClient *ssh.Client) (*ForwardInfo, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := LocalForwardSSH(ctx, sshClient, remoteHost, remotePort, localPort)
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
func (fm *ForwardManager) CreateDynamic(sessionID, sshConfig string, localPort int, sshClient *ssh.Client, localDial bool) (*ForwardInfo, error) {
	if !localDial && sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	if localDial {
		return fm.dynamicLocal(sessionID, sshConfig, localPort)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := DynamicForwardSSH(ctx, sshClient, localPort)
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
	fm.mu.Lock()
	defer fm.mu.Unlock()
	out := make([]ForwardInfo, 0, len(fm.forwards))
	for _, fw := range fm.forwards {
		out = append(out, fw.ForwardInfo)
	}
	return out
}

// ListBySession returns forwards belonging to sessionID.
func (fm *ForwardManager) ListBySession(sessionID string) []ForwardInfo {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	out := make([]ForwardInfo, 0)
	for _, fw := range fm.forwards {
		if fw.SessionID == sessionID {
			out = append(out, fw.ForwardInfo)
		}
	}
	return out
}

// CloseBySession closes all forwards belonging to the given session ID.
func (fm *ForwardManager) CloseBySession(sessionID string) {
	fm.mu.Lock()
	var toClose []*forwardState
	for id, fw := range fm.forwards {
		if fw.SessionID == sessionID {
			toClose = append(toClose, fw)
			delete(fm.forwards, id)
		}
	}
	fm.mu.Unlock()
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
	fm.mu.Lock()
	fw, ok := fm.forwards[forwardID]
	if !ok {
		fm.mu.Unlock()
		slog.Warn("forward close: not found", "forward_id", forwardID)
		return fmt.Errorf("forward %q %w", forwardID, ErrNotFound)
	}
	delete(fm.forwards, forwardID)
	fm.mu.Unlock()

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
