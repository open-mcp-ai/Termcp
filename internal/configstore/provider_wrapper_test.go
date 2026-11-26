package configstore

import (
	"errors"
	"strings"
	"testing"
)

/* A key belongs to ONE set of words. Two keys in one keychain each carry their own
 * slots, so a provider that opens the first key's slots opens nothing else — which
 * is what lets one store hold hosts at different trust levels.
 *
 * Mutation check: seal every key's words under every provider slot (whole-keychain
 * wrapping) and the "other key stays shut" assertion fails.
 */
func TestAProviderOnlyOpensTheSlotsItSealed(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x92))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	firstKeyFile := writeKeyFile(t, 0xa1)
	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	var firstID ID
	if err := firstID.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddUnlockTo(firstID, NewFileProvider(firstKeyFile)); err != nil {
		t.Fatalf("second way into the first key: %v", err)
	}

	secondKeyFile := writeKeyFile(t, 0xa2)
	second, err := repo.CreateKeyWithLabel(testPhrase(t), "", "second", NewFileProvider(secondKeyFile))
	if err != nil {
		t.Fatalf("create second key: %v", err)
	}
	var secondID ID
	if err := secondID.UnmarshalText([]byte(second.ID)); err != nil {
		t.Fatal(err)
	}

	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	/* Reopened with no providers at all: nothing is loaded, and each phrase must
	 * load exactly one key. */
	reopened, err := OpenRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if status := reopened.Status(); status.LoadedKeys != 0 {
		t.Fatalf("reopened with %d keys loaded; want none", status.LoadedKeys)
	}

	if err := reopened.PlugKey(firstID, NewFileProvider(secondKeyFile)); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("the second key's provider opened the first key: %v", err)
	}
	if err := reopened.PlugKey(firstID, NewFileProvider(firstKeyFile)); err != nil {
		t.Fatalf("the first key's own provider did not open it: %v", err)
	}
	if _, ok := reopened.snap().keyring.key(secondID); ok {
		t.Fatal("plugging one key also plugged in another")
	}
	if err := reopened.PlugKey(secondID, NewFileProvider(secondKeyFile)); err != nil {
		t.Fatalf("the second key's own provider did not open it: %v", err)
	}
	if got := reopened.Status().LoadedKeys; got != 2 {
		t.Fatalf("plugged keys = %d; want 2", got)
	}
}

/* Two keys are independent in the one way that matters: a profile sealed by A
 * cannot be opened with B. Everything else about them is deliberately alike.
 *
 * Mutation check: make SealWith ignore keyID and seal with the catalog owner
 * instead, and B opens A's profile.
 */
func TestHostsSealedByOneKeyStayShutForAnother(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x93))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	var aID ID
	if err := aID.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	b, err := repo.CreateKeyWithLabel(testPhrase(t), "", "b", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	var bID ID
	if err := bID.UnmarshalText([]byte(b.ID)); err != nil {
		t.Fatal(err)
	}

	if err := repo.SaveWithKey("alpha", []byte("host = \"a.example\"\n"), aID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithKey("beta", []byte("host = \"b.example\"\n"), bID); err != nil {
		t.Fatal(err)
	}
	usage := map[ID]int{}
	for _, ref := range repo.snap().refs {
		raw, err := repo.readObjectRaw(ref)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := Parse(raw, Identity{StoreID: repo.snap().keyring.StoreID(), ObjectID: ref.objectID, GenerationID: ref.generationID, Kind: KindProfile})
		if err != nil {
			t.Fatal(err)
		}
		usage[envelope.WrapKeyID()]++
	}
	if usage[aID] != 1 || usage[bID] != 1 {
		t.Fatalf("profiles per key = %v; want one each for %v and %v", usage, aID, bID)
	}

	// Only B unloaded: its host is unreadable and says which key it needs, while
	// A's host keeps working. Unloading a key does not touch the other one.
	if err := repo.UnplugKey(bID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read("beta"); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("reading B's host with B unplugged = %v; want ErrKeyNotLoaded", err)
	}
	if data, err := repo.Read("alpha"); err != nil || !strings.Contains(string(data), "a.example") {
		t.Fatalf("A's host while A stays loaded = %q, %v", data, err)
	}
	// B's material is gone from memory, so its envelope cannot be opened even
	// directly through the keyring.
	raw, err := repo.readObjectRaw(repo.snap().refs["beta"])
	if err != nil {
		t.Fatal(err)
	}
	ref := repo.snap().refs["beta"]
	envelope, err := Parse(raw, Identity{StoreID: repo.snap().keyring.StoreID(), ObjectID: ref.objectID, GenerationID: ref.generationID, Kind: KindProfile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.snap().keyring.Open(envelope, noActivationKey); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("opening B's envelope without B = %v; want ErrKeyNotLoaded", err)
	}
	if err := repo.PlugKey(bID, NewEnvProvider("")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read("beta"); err != nil {
		t.Fatalf("B's host after loading B back: %v", err)
	}
	/* Unplugging the catalog owner is allowed and costs nothing: the index is a
	   cache of the profile directories, so it is re-sealed under a key that is still
	   plugged in. B's host stays readable, and A's reports "plug A" — one host at a
	   time, not a store that goes dark. */
	if err := repo.UnplugKey(aID); err != nil {
		t.Fatalf("unplugging the catalog owner = %v; want nil", err)
	}
	if _, err := repo.Read("beta"); err != nil {
		t.Fatalf("B's host after unplugging the owner: %v", err)
	}
	if _, err := repo.Read("alpha"); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("A's host with A unplugged = %v; want ErrKeyNotLoaded", err)
	}
}

func mustID(t *testing.T) ID {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func splitWords(value string) []string { return strings.Fields(value) }
