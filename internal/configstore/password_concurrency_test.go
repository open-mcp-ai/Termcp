package configstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pausePasswordDerivation(t *testing.T, r *Repository, call int) (<-chan struct{}, func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	var calls atomic.Int32
	r.derivePassword = func(secret []byte, p *storedPasswordDerivation) (MainKey, error) {
		if int(calls.Add(1)) == call {
			close(entered)
			<-resume
		}
		return deriveMain(secret, p)
	}
	unblock := func() { release.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	return entered, unblock
}

func awaitPasswordPhase(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("password derivation did not reach its barrier")
	}
}
func awaitPasswordResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("password operation did not finish")
		return nil
	}
}

func TestPasswordDerivationAllowsRepositoryTraffic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  int
		change bool
	}{{"verify", 1, false}, {"verify-current-for-change", 1, true}, {"derive-new-for-change", 2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := passwordRepository(t)
			entered, resume := pausePasswordDerivation(t, r, tc.phase)
			done := make(chan error, 1)
			go func() {
				if tc.change {
					done <- r.ChangePassword(firstPassword, secondPassword, secondPassword)
				} else {
					done <- r.VerifyPassword(firstPassword)
				}
			}()
			awaitPasswordPhase(t, entered)
			traffic := make(chan error, 1)
			go func() {
				if err := r.Save("during-derivation", []byte("concurrent profile")); err != nil {
					traffic <- err
					return
				}
				_, err := r.Read("alpha")
				traffic <- err
			}()
			trafficDone := false
			select {
			case err := <-traffic:
				trafficDone = true
				if err != nil {
					t.Error(err)
				}
			case <-time.After(2 * time.Second):
				t.Error("KDF holds the repository's ordinary traffic lock")
			}
			resume()
			if err := awaitPasswordResult(t, done); err != nil {
				t.Fatal(err)
			}
			if !trafficDone {
				if err := awaitPasswordResult(t, traffic); err != nil {
					t.Error(err)
				}
			}

			if data, err := r.Read("during-derivation"); err != nil || string(data) != "concurrent profile" {
				t.Fatal("concurrent profile was lost", err)
			}
		})
	}
}

func TestUnlockDerivationCanBeClosedWithoutAdoptingCandidate(t *testing.T) {
	r := passwordRepository(t)
	dir := r.dataDir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	locked, err := OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	entered, resume := pausePasswordDerivation(t, locked, 1)
	done := make(chan error, 1)
	go func() { done <- locked.UnlockPassword(firstPassword) }()
	awaitPasswordPhase(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- locked.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Close waited for password derivation")
	}
	resume()
	if err := awaitPasswordResult(t, done); !errors.Is(err, ErrClosed) {
		t.Fatalf("candidate committed after Close: %v", err)
	}
}

func TestPasswordDerivationRejectsChangedKeyMetadata(t *testing.T) {
	r := passwordRepository(t)
	entered, resume := pausePasswordDerivation(t, r, 1)
	done := make(chan error, 1)
	go func() { done <- r.VerifyPassword(firstPassword) }()
	awaitPasswordPhase(t, entered)
	path := filepath.Join(r.dataDir, "keys", "config.keys")
	original, err := os.ReadFile(path)
	if err != nil {
		resume()
		t.Fatal(err)
	}
	modified := append(append([]byte(nil), original...), '\n')
	if err = os.WriteFile(path, modified, 0600); err != nil {
		resume()
		t.Fatal(err)
	}
	resume()
	if err := awaitPasswordResult(t, done); err == nil {
		t.Fatal("password proof ignored key metadata replacement")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
}

type pausedKeyProvider struct {
	Provider
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (p *pausedKeyProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	p.once.Do(func() { close(p.entered); <-p.resume })
	return p.Provider.unwrapKey(parameters)
}

func TestLegacyProviderDerivationDoesNotHoldRepositoryLock(t *testing.T) {
	r, provider := newRotationRepository(t)
	paused := &pausedKeyProvider{Provider: provider, entered: make(chan struct{}), resume: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(paused.resume) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- r.RotateMainKey(paused) }()
	awaitPasswordPhase(t, paused.entered)
	traffic := make(chan error, 1)
	go func() { traffic <- r.Save("during-provider", []byte("concurrent opaque data")) }()
	completed := false
	select {
	case err := <-traffic:
		completed = true
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("legacy provider derivation held Repository.mu")
	}
	release()
	if err := awaitPasswordResult(t, done); err != nil {
		t.Fatal(err)
	}
	if !completed {
		if err := awaitPasswordResult(t, traffic); err != nil {
			t.Error(err)
		}
	}
	if _, err := r.Read("during-provider"); err != nil {
		t.Fatal("rotation lost concurrent provider-era write", err)
	}
}
