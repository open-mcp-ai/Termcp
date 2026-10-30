package sshconfig

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

func TestStorePasswordWorkDoesNotHoldOrdinaryLock(t *testing.T) {
	s := NewStore(t.TempDir())
	t.Cleanup(func() { _ = s.Close() })
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		done <- s.passwordRepository(func(repo *configstore.Repository) (*configstore.Repository, bool, error) {
			close(entered)
			<-resume
			return repo, false, nil
		})
	}()
	<-entered
	listed := make(chan error, 1)
	go func() { _, err := s.List(); listed <- err }()
	select {
	case err := <-listed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("password work held Store.mu")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStoreCloseDuringInitializationDisposesCandidate(t *testing.T) {
	s := NewStore(t.TempDir())
	t.Cleanup(func() { _ = s.Close() })
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		done <- s.passwordRepository(func(repo *configstore.Repository) (*configstore.Repository, bool, error) {
			close(entered)
			<-resume
			candidate, err := configstore.InitializeRepositoryWithPassword(s.dataDir, "synthetic master password", "synthetic master password")
			return candidate, true, err
		})
	}()
	<-entered
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; !errors.Is(err, configstore.ErrClosed) {
		t.Fatalf("initialization adopted a closed Store: %v", err)
	}
	if s.repositorySnapshot() != nil {
		t.Fatal("closed Store retained the candidate")
	}
	reopened, err := configstore.OpenRepositoryForRecovery(s.dataDir)
	if err != nil {
		t.Fatal("candidate leaked its directory lock", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}

func TestPasswordNotificationCanReenterStore(t *testing.T) {
	s := NewStore(t.TempDir())
	t.Cleanup(func() { _ = s.Close() })
	const password = "synthetic notification password"
	called := make(chan error, 1)
	s.SetOnChange(func() { called <- s.UnlockAuthentication(password) })
	done := make(chan error, 1)
	go func() { done <- s.InitializeEncryption(password, password) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("password notification retained a Store lock")
	}
	select {
	case err := <-called:
		if err != nil {
			t.Fatal("notification could not use Store", err)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not called")
	}
}
