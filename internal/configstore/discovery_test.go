package configstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* Resolving one host reads one directory, not the whole tree.
 *
 * There is no index, so a name is answered by that name's own directory. Doing it
 * by rescanning every host — which is what the whole-tree view is for — made a
 * listing quadratic: with 245 hosts, painting the list took minutes, because every
 * one of the 245 loads rescanned all 245 directories. The fallback scan still
 * exists for the case it is actually needed: a name whose spelling differs in case.
 */
func TestResolvingOneHostDoesNotScanTheOthers(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x77))
	repo, err := InitializeRepository(t.TempDir(), NewEnvProvider(""))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := repo.Save(name, []byte("host = \""+name+".example\"\n")); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	if _, err := repo.Read("alpha"); err != nil {
		t.Fatalf("reading alpha: %v", err)
	}
	// Make alpha ambiguous: two committed generations in its directory. That is a
	// fact about alpha, and a whole-tree scan reports it.
	dir := filepath.Join(repo.dataDir, "ssh_configs", "alpha")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".enc") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "duplicate.enc"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		break
	}
	if _, err := repo.List(); err == nil {
		t.Fatal("listing did not report the ambiguous host")
	}
	// Reading beta is one directory — alpha's ambiguity is not beta's problem, and
	// the read must not have to look at every other host to find out.
	if _, err := repo.Read("beta"); err != nil {
		t.Fatalf("reading beta while alpha is ambiguous: %v", err)
	}
	// And the case-insensitive fallback still resolves a different spelling.
	if _, err := repo.Read("BETA"); err != nil {
		t.Fatalf("reading BETA (different case): %v", err)
	}
}
