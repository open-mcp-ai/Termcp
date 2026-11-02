package configstore

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func readStoreFileForTest(path string) ([]byte, error) { return readBoundedFile(path, maxLocalState) }

// overwriteStoreFileForTest injects arbitrary corruption at the actual storage
// boundary. Production writers refuse mutable body replacement; corruption
// tests must bypass that guard deliberately rather than create ignored files.
func overwriteStoreFileForTest(path string, data []byte, _ os.FileMode) error {
	root, entry, ok := logicalStorePath(path)
	if !ok {
		return os.WriteFile(path, data, 0600)
	}
	state, err := loadLocalState(root)
	if err != nil {
		return err
	}
	state = cloneLocalState(state)
	if entry == "config.keys" {
		state.Keyring = json.RawMessage(bytes.Clone(data))
		return publishLocalState(root, state, writeFileAtomic)
	}
	if validHostBodyEntry(entry) {
		current, e := readPhysicalFile(path, maxLocalState)
		if e != nil {
			return e
		}
		entry = bodyEnvelopeKey(current)
	}
	if entry == "catalog" || entry == "authentication" {
		entry = state.Heads[entry]
	}
	if len(data) < envelopeHeaderSize {
		return ErrFormat
	}
	state.Envelopes[entry] = bytes.Clone(data[:envelopeHeaderSize])
	body := append(bytes.Clone(data[:dataHeaderSize]), data[envelopeHeaderSize:]...)
	bodyPath, err := bodyPathInState(root, state, entry)
	if err != nil {
		return err
	}
	if err = writeFileAtomic(bodyPath, body); err != nil {
		return err
	}
	return publishLocalState(root, state, writeFileAtomic)
}

func TestLocalStateRejectsMissingStateWithoutLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	r, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Save("test", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err = os.Remove(LocalStatePath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenRepositoryForRecovery(dir); err == nil {
		t.Fatal("orphaned objects became a new vault")
	}
}
