package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

/* The contract this file pins down is the deployment's whole reason for having a
 * system provider: an initialized store must come back usable after a restart
 * with nobody at the keyboard. A password wrapper cannot do that — it needs a
 * human by definition — so the machine-bound keyring is the default path, and a
 * password is only an additional wrapper for carrying the data directory to a
 * different machine.
 *
 * Mutation check: passing no provider to OpenConfiguredStore (the pre-fix state
 * in core.New) leaves the reopened store locked and fails this test.
 */
func TestMachineBoundKeyringReopensWithoutAPassword(t *testing.T) {
	system := configstore.SystemProvider()
	if !configstore.SystemAvailable() {
		t.Skipf("no native credential store on %s", runtime.GOOS)
	}
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// No password: the provider writes the only wrapper, so this must produce an
	// unlocked, password-less keyring.
	if err := store.InitializeEncryption("", "", system); err != nil {
		t.Fatalf("machine-bound initialize: %v", err)
	}
	remote := []byte(`kind = "remote"
host = "host.example"
user = "operator"
password = "synthetic-disable-secret"
`)
	if err := store.Save("locked-host", remote); err != nil {
		t.Fatal(err)
	}
	if results, err := store.EncryptProfiles([]string{"locked-host"}); err != nil || !results[0].OK {
		t.Fatalf("encrypt: %+v, %v", results, err)
	}
	if mode, err := store.StorageMode("locked-host"); err != nil || mode != StorageEncrypted {
		t.Fatalf("mode = %q, err %v", mode, err)
	}
	state, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if state.PasswordConfigured {
		t.Fatal("machine-bound keyring reports a password; nothing was typed")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	/* Restart, with no password anywhere. This is the daily case: the daemon is
	   re-spawned by its MCP client and the agent has to keep working. */
	reopened, err := OpenConfiguredStore(dir, configstore.SystemProvider())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := reopened.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Locked {
		t.Fatal("store came back locked; a machine-bound keyring must unlock itself on restart")
	}
	if !restarted.AutomaticUnlock {
		t.Fatal("status does not report automatic unlock although the system provider opened it")
	}
	entry, err := reopened.Load("locked-host")
	if err != nil {
		t.Fatalf("reading an encrypted host after restart: %v", err)
	}
	if entry.Host != "host.example" {
		t.Fatalf("host = %q", entry.Host)
	}
}

/* A copy of the data directory on another machine must not open by itself: that
 * is the protection the encryption exists for. Proven here by opening the same
 * directory with a provider whose key material is a different DPAPI user context
 * — simulated by an unrelated env provider, which cannot unwrap a dpapi slot. */
func TestMachineBoundKeyringStaysSealedWithoutItsProvider(t *testing.T) {
	if !configstore.SystemAvailable() {
		t.Skipf("no native credential store on %s", runtime.GOOS)
	}
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEncryption("", "", configstore.SystemProvider()); err != nil {
		t.Fatalf("machine-bound initialize: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys", "config.keys")); err != nil {
		t.Fatalf("keyring file missing: %v", err)
	}
	foreign, err := OpenConfiguredStore(dir, configstore.NewEnvProvider("TERMCP_SYNTHETIC_ABSENT_KEK"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	state, err := foreign.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Locked {
		t.Fatal("a store whose keyring wrapper is not available opened unlocked")
	}
}

/* 删除密钥 deletes the key and the catalog index — and nothing else. The
 * encrypted profile stays on disk, unreadable forever. That is the consequence
 * the operator was warned about, and it must not be replaced by a silent
 * cleanup that destroys more than the request.
 *
 * Mutation check: also removing the profile directories (the earlier, more
 * aggressive behaviour) fails the ciphertext-still-there assertion.
 */
func TestDestroyEncryptionDeletesTheKeyOnly(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x81))
	const password = "synthetic destroy password"
	if err := store.InitializeEncryption(password, password, configstore.NewEnvProvider("")); err != nil {
		t.Fatalf("enable: %v", err)
	}
	remote := []byte(`kind = "remote"
host = "host.example"
user = "operator"
password = "synthetic-destroy-secret"
`)
	if err := store.Save("gone", remote); err != nil {
		t.Fatal(err)
	}
	if results, err := store.EncryptProfiles([]string{"gone"}); err != nil || !results[0].OK {
		t.Fatalf("encrypt: %+v, %v", results, err)
	}
	if err := store.DestroyEncryption(); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	for _, path := range []string{"keys/config.keys", "ssh_configs/store.enc"} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("%s survived destroy", path)
		}
	}
	// The ciphertext is still there.
	found := false
	entries, err := os.ReadDir(filepath.Join(dir, "ssh_configs", "gone"))
	if err != nil {
		t.Fatalf("profile directory was removed with the key: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			found = true
		}
	}
	if !found {
		t.Fatal("destroy removed the ciphertext; the user asked to delete the key, not the data")
	}
	// Reopening shows the host as encrypted and permanently unreadable.
	reopened, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("reopen after destroy: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if mode, err := reopened.StorageMode("gone"); err != nil || mode != StorageEncrypted {
		t.Fatalf("mode after destroy = %q, err %v; want encrypted", mode, err)
	}
	if _, err := reopened.Load("gone"); err == nil || !strings.Contains(err.Error(), "key was deleted") {
		t.Fatalf("load after destroy = %v; want a message naming the deleted key", err)
	}
}

/* 上锁 is the deliberate version of a restart: the key leaves memory, the file
 * stays, encrypted hosts stop being readable, plaintext ones are untouched, and
 * unlocking brings everything back. No credential is required to lock.
 *
 * Mutation check: making Lock a no-op fails the locked-after assertion; making
 * Lock also break plaintext loading fails the plain assertion.
 */
func TestLockAndUnlockCycle(t *testing.T) {
	dir := t.TempDir()
	if !configstore.SystemAvailable() {
		t.Skipf("no native credential store on %s", runtime.GOOS)
	}
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	remote := []byte(`kind = "remote"
host = "host.example"
user = "operator"
password = "synthetic-lock-secret"
`)
	// 加密启用后新建的主机默认加密，所以明文主机必须在启用之前创建。
	if err := store.Save("plain", remote); err != nil {
		t.Fatal(err)
	}
	const password = "synthetic lock password"
	if err := store.InitializeEncryption(password, password, configstore.SystemProvider()); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := store.Save("sealed", remote); err != nil {
		t.Fatal(err)
	}
	if results, err := store.EncryptProfiles([]string{"sealed"}); err != nil || !results[0].OK {
		t.Fatalf("encrypt: %+v, %v", results, err)
	}
	if err := store.LockEncryption(); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if state, err := store.EncryptionStatus(); err != nil || !state.Locked {
		t.Fatalf("status after lock = %+v, err %v", state, err)
	}
	if _, err := store.Load("sealed"); !errors.Is(err, configstore.ErrLocked) {
		t.Fatalf("locked store served an encrypted host: %v", err)
	}
	if _, err := store.Load("plain"); err != nil {
		t.Fatalf("locking must not touch plaintext hosts: %v", err)
	}
	// 解锁（并顺带绑定本机，检验显式参数路径），密文恢复可读。
	if err := store.UnlockEncryption(password, true, configstore.SystemProvider()); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if entry, err := store.Load("sealed"); err != nil || entry.Host != "host.example" {
		t.Fatalf("load after unlock = %+v, %v", entry, err)
	}
}
