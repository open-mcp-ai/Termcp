package auth

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

type pausedPasswordVault struct {
	Vault
	phase   string
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (p *pausedPasswordVault) pause(phase string) {
	if p.phase == phase {
		p.once.Do(func() { close(p.entered); <-p.resume })
	}
}
func (p *pausedPasswordVault) UnlockAuthentication(password string, providers ...configstore.Provider) error {
	p.pause("unlock")
	return p.Vault.UnlockAuthentication(password, providers...)
}
func (p *pausedPasswordVault) PrepareAuthentication(password, previous string, automatic bool, providers ...configstore.Provider) error {
	p.pause("prepare")
	return p.Vault.PrepareAuthentication(password, previous, automatic, providers...)
}
func (p *pausedPasswordVault) ChangeEncryptionPassword(current, password, confirmation string, providers ...configstore.Provider) error {
	p.pause("change")
	return p.Vault.ChangeEncryptionPassword(current, password, confirmation, providers...)
}
func pauseVault(t *testing.T, m *Managed, phase string) (<-chan struct{}, func()) {
	t.Helper()
	p := &pausedPasswordVault{Vault: m.vault, phase: phase, entered: make(chan struct{}), resume: make(chan struct{})}
	m.vault = p
	var once sync.Once
	release := func() { once.Do(func() { close(p.resume) }) }
	t.Cleanup(release)
	return p.entered, release
}
func waitForVaultPhase(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("password operation did not reach barrier")
	}
}
func waitForVaultResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("password operation did not complete")
		return nil
	}
}

func TestLoginPasswordWorkAllowsAuthenticationAndConcurrentPolicySave(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	agent := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"client"}`)
	entered, resume := pauseVault(t, m, "unlock")
	logged := make(chan error, 1)
	go func() { _, _, err := m.login("admin", testPassword); logged <- err }()
	waitForVaultPhase(t, entered)
	traffic := make(chan error, 1)
	go func() {
		if _, _, err := m.verify(cookie); err != nil {
			traffic <- err
			return
		}
		w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "PATCH", "/auth/agents/"+agent.Agent.ID, `{"revision":1,"enabled":false}`, cookie, session.CSRF)
		if w.Code != 200 {
			traffic <- configstore.ErrAuthentication
			return
		}
		traffic <- nil
	}()
	var trafficErr error
	trafficDone := false
	select {
	case trafficErr = <-traffic:
		trafficDone = true
		if trafficErr != nil {
			t.Error(trafficErr)
		}
	case <-time.After(2 * time.Second):
		t.Error("login KDF held the authentication policy lock")
	}
	resume()
	if err := waitForVaultResult(t, logged); err != nil {
		t.Fatal(err)
	}
	if !trafficDone {
		if err := waitForVaultResult(t, traffic); err != nil {
			t.Error(err)
		}
	}
	m.mu.Lock()
	enabled := m.state.Agents[agent.Agent.ID].Enabled
	m.mu.Unlock()
	if enabled {
		t.Fatal("login restored stale policy state")
	}
}

func TestPasswordChangeWorkAllowsExistingMCPCredentials(t *testing.T) {
	for _, phase := range []string{"unlock", "change"} {
		t.Run(phase, func(t *testing.T) {
			m, cookie, session := configuredManaged(t)
			agent := issueTestCredential(t, m, cookie, session, "/auth/agents", `{"name":"client"}`)
			entered, resume := pauseVault(t, m, phase)
			changed := make(chan error, 1)
			go func() {
				changed <- m.ChangePassword(testPassword, "synthetic new master password", "synthetic new master password")
			}()
			waitForVaultPhase(t, entered)
			traffic := make(chan int, 1)
			go func() {
				h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
				traffic <- bearerRequest(h, "POST", "/stream", agent.Token, "").Code
			}()
			trafficDone := false
			select {
			case code := <-traffic:
				trafficDone = true
				if code != 204 {
					t.Errorf("MCP authentication during password work: %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Error("master-password work held the ordinary authentication lock")
			}
			resume()
			if err := waitForVaultResult(t, changed); err != nil {
				t.Fatal(err)
			}
			if !trafficDone {
				select {
				case <-traffic:
				case <-time.After(10 * time.Second):
					t.Fatal("MCP request did not finish")
				}
			}

		})
	}
}
