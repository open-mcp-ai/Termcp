package sshconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

func TestEncryptProfileKeepsCiphertextInOriginalHostDirectory(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	t.Cleanup(func() { _ = s.Close() })
	raw := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"synthetic-host-secret\"\n")
	if err := s.Save("host", raw); err != nil {
		t.Fatal(err)
	}
	host := s.ConfigDir("host")
	beforeDir, err := os.Stat(host)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(host, "associated.txt")
	if err = os.WriteFile(marker, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.InitializeEncryption("configuration-password-one", "configuration-password-one"); err != nil {
		t.Fatal(err)
	}
	results, err := s.EncryptProfiles([]string{"host"})
	if err != nil || len(results) != 1 || !results[0].OK {
		t.Fatal("encryption failed", results, err)
	}
	afterDir, err := os.Stat(host)
	if err != nil || !os.SameFile(beforeDir, afterDir) {
		t.Fatal("encryption moved the host directory", err)
	}
	paths, err := filepath.Glob(filepath.Join(host, "*.enc"))
	if err != nil || len(paths) != 1 {
		t.Fatal("ciphertext not retained in host directory", paths, err)
	}
	body, err := os.ReadFile(paths[0])
	if err != nil || bytes.Contains(body, []byte("synthetic-host-secret")) {
		t.Fatal("host body is not encrypted", err)
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeEncryptionPassword("configuration-password-one", "configuration-password-two", "configuration-password-two"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(paths[0])
	if err != nil || !bytes.Equal(body, after) {
		t.Fatal("password change rewrote host ciphertext", err)
	}
	current, err := os.Stat(paths[0])
	if err != nil || !os.SameFile(info, current) {
		t.Fatal("password change replaced host ciphertext", err)
	}
	if data, err := s.ReadRaw("host"); err != nil || !bytes.Equal(data, raw) {
		t.Fatal("encrypted profile no longer readable", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained" {
		t.Fatal("encryption removed an associated file", err)
	}
	err = filepath.WalkDir(filepath.Join(dir, "local-state", "objects"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) > 12 && data[12] == byte(configstore.KindProfile) {
			t.Errorf("host body relocated to central object storage")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
