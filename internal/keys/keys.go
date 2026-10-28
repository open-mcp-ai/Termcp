// Package keys owns the Ed25519 root key a local caller signs its own tokens with.
//
// WHY A FILE AND NOT A SOCKET. A socket would hand out tokens and never let the
// secret leave the daemon, which sounds strictly better and is not: the threat
// both designs answer is "another process of the same OS user", and against that
// a 0600 file and a same-user socket are the same door. The differences that do
// exist are exfiltration (a file can be copied off the machine), auditing (a
// socket request can be logged, a read cannot), and rotation cost — and only the
// first is worth designing around. So the file is the format, and rotation is the
// answer to a copy: see Rotate below and the token record in tokens.json.
//
// THE KEY DOES NOT DECIDE AUTHORITY. What a token is allowed to do comes from
// WHICH key signed it, never from claims the signer wrote (docs/design/
// access-policy.md §1.1). A local caller that reads this file can mint a token
// claiming any scope; the daemon replaces those with the built-in MCP template.
// That is what makes handing out the private key acceptable at all: the OS user
// who can read it already has a shell on this host, so the token adds nothing
// they did not have. The invariant that keeps it true lives elsewhere — human
// authority needs a second key, sealed by a passphrase the machine does not hold.
//
// WHAT THIS PACKAGE REFUSES TO DO:
//
//   - read a key from the environment. An inline secret lands in /proc/1/environ,
//     in crash dumps and in `docker inspect`, so it is a different (worse) mode
//     wearing the same name. Only a PATH is accepted.
//   - load a file that is group- or world-readable. The permission bits ARE the
//     authorization for local callers, so a loose mode is not cosmetic: it means
//     every account on the host can mint tokens. Checked on read, not only on
//     create, because the file outlives this process and chmod happens elsewhere.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables. Both name a PATH; neither carries key material.
const (
	// EnvKeyFile points at the root key. It exists so a container can mount the
	// secret at an arbitrary path without also rewriting the command line.
	EnvKeyFile = "TERMCP_ROOT_KEY_FILE"

	// EnvInlineKey is deliberately a DIFFERENT name from EnvKeyFile so that
	// setting it is a visible act rather than a typo: a value under this name is
	// refused by Load, and the refusal says where to put the bytes instead.
	EnvInlineKey = "TERMCP_ROOT_KEY_INLINE"
)

const (
	// RootName is the key file's name inside Addr.Dir.
	RootName = "root.key"
	// MetaName records the kid and algorithm beside the key, so a rotated key is
	// identifiable without parsing anything.
	MetaName = "root.key.meta"

	// fileMode is the whole authorization story for local callers. See the
	// package comment.
	fileMode = 0o600
	dirMode  = 0o700
)

// Addr is where the access material lives.
//
// A path, never a value. The zero value is unusable and Load says so, because a
// default silently picked from the process's working directory would make the
// key's location depend on where the binary was launched — the exact class of
// bug that put a transcript export into the wrong repository once already.
type Addr struct {
	Dir  string // the data directory (…/access is created inside it)
	File string // optional override for the key file itself
}

// Root is a loaded root key.
type Root struct {
	// Kid identifies this key among rotated ones. Derived from the public key, so
	// it cannot disagree with the material it names and needs no separate trust.
	Kid    string
	Public ed25519.PublicKey
	Path   string

	private ed25519.PrivateKey
}

// Sign produces a detached signature. Callers build their own token bytes; this
// package does not know what a token is.
func (r Root) Sign(msg []byte) []byte { return ed25519.Sign(r.private, msg) }

// Verify reports whether sig is a valid signature by this key.
func (r Root) Verify(msg, sig []byte) bool { return ed25519.Verify(r.Public, msg, sig) }

// accessDir is the directory the key lives in.
func (a Addr) accessDir() string { return filepath.Join(a.Dir, "access") }

// Path resolves the key file's location: an explicit File wins, then the
// environment, then the default under the data directory.
//
// THE ORDER IS FIXED AND DOCUMENTED because a key that resolves differently
// between two processes is a key mismatch that looks like a signature failure.
func (a Addr) Path() (string, error) {
	if a.Dir == "" && a.File == "" && strings.TrimSpace(os.Getenv(EnvKeyFile)) == "" {
		return "", errors.New("keys: no data directory, no key file and no " + EnvKeyFile + " — refusing to guess where the root key lives")
	}
	if p := strings.TrimSpace(a.File); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(os.Getenv(EnvKeyFile)); p != "" {
		return p, nil
	}
	return filepath.Join(a.accessDir(), RootName), nil
}

// Load reads the root key at the resolved path, creating one if the file does
// not exist yet.
//
// CREATION IS THE DAEMON'S JOB AND ONLY THE DAEMON'S. A caller that starts
// signing before anyone has looked at the host would decide the key's location
// and lifetime for everyone else, so the first run belongs to the process that
// owns the data directory.
func (a Addr) Load() (Root, error) {
	if v := strings.TrimSpace(os.Getenv(EnvInlineKey)); v != "" {
		return Root{}, fmt.Errorf("keys: %s carries key material, which this build refuses: an inline secret is visible in /proc/1/environ, in crash dumps and in `docker inspect`. Write the bytes to a file and point %s at its path", EnvInlineKey, EnvKeyFile)
	}

	path, err := a.Path()
	if err != nil {
		return Root{}, err
	}

	if _, statErr := os.Stat(path); statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return Root{}, fmt.Errorf("keys: stat %s: %w", path, statErr)
		}
		return a.create(path)
	}
	return load(path)
}

// create writes a new key atomically: a sibling temporary file that is renamed
// over the target, so a process that dies mid-write leaves either no key or a
// whole one. A half-written key would sign nothing and would be indistinguishable
// from a corrupt file.
func (a Addr) create(path string) (Root, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return Root{}, fmt.Errorf("keys: create %s: %w", dir, err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Root{}, fmt.Errorf("keys: generate: %w", err)
	}
	kid := Kid(pub)

	// The file holds seed||public in ed25519's own PrivateKeySize layout, base64
	// so it survives a copy through anything that mangles binaries.
	body := "# termcp root key (Ed25519). Anyone who can read this file can mint a token\n" +
		"# for the built-in MCP template. Keep it 0600 and out of backups you cannot guard.\n" +
		"kid = \"" + kid + "\"\n" +
		"algo = \"ed25519\"\n" +
		"key = \"" + base64.StdEncoding.EncodeToString(priv) + "\"\n"

	tmp := path + ".termcp-part"
	if err := os.WriteFile(tmp, []byte(body), fileMode); err != nil {
		return Root{}, fmt.Errorf("keys: write %s: %w", tmp, err)
	}
	// WriteFile honours the mode only when it creates the file; an existing
	// temporary from a previous crash keeps its old bits, so set them explicitly.
	if err := os.Chmod(tmp, fileMode); err != nil {
		return Root{}, fmt.Errorf("keys: chmod %s: %w", tmp, err)
	}
	if err := renameOver(tmp, path); err != nil {
		return Root{}, fmt.Errorf("keys: install %s: %w", path, err)
	}

	if err := writeMeta(dir, kid); err != nil {
		return Root{}, err
	}
	return Root{Kid: kid, Public: pub, Path: path, private: priv}, nil
}

// load reads an existing key and refuses the two states that would silently
// weaken it: a mode other users can read, and a kid that disagrees with the
// material.
func load(path string) (Root, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Root{}, fmt.Errorf("keys: stat %s: %w", path, err)
	}
	if info.IsDir() {
		return Root{}, fmt.Errorf("keys: %s is a directory, expected the key file", path)
	}
	// THE MODE CHECK IS THE POINT. A 0644 root key means every account on this
	// host can mint tokens for the MCP template; reporting it is the difference
	// between an operator fixing the bits and a permission system that is quietly
	// wide open. Windows has no POSIX mode, so the check is skipped there rather
	// than failing every load (file ACLs are the equivalent control and this
	// package does not read them).
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return Root{}, fmt.Errorf("keys: %s is mode %04o; it must not be readable by group or others (chmod 600). The permission bits are what authorizes local callers, so a loose mode lets any account on this host mint tokens", path, mode)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Root{}, fmt.Errorf("keys: read %s: %w", path, err)
	}
	body := string(raw)

	encoded := metaValue(body, "key")
	if encoded == "" {
		return Root{}, fmt.Errorf("keys: %s has no `key = \"…\"` line", path)
	}
	priv, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Root{}, fmt.Errorf("keys: %s: key is not base64: %w", path, err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return Root{}, fmt.Errorf("keys: %s: key is %d bytes, expected %d (a full Ed25519 private key, seed||public)", path, len(priv), ed25519.PrivateKeySize)
	}
	pub := ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)

	// The recorded kid is checked against the material rather than trusted: a key
	// file edited by hand can otherwise claim a kid that the daemon maps to a
	// different template, which is the one thing this file must not be able to
	// steer.
	if recorded := metaValue(body, "kid"); recorded != "" && recorded != Kid(pub) {
		return Root{}, fmt.Errorf("keys: %s records kid %q but the key material hashes to %q — the file was edited or the two halves were mixed up", path, recorded, Kid(pub))
	}

	return Root{Kid: Kid(pub), Public: pub, Path: path, private: ed25519.PrivateKey(priv)}, nil
}

// Kid derives a short, stable identifier from a public key.
//
// It is a hash of the key rather than a serial number because the kid has to be
// verifiable against the material it names: a hand-edited number could point at
// another key's template.
func Kid(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Rotate replaces the key material in place and reports the old kid, so the
// caller can mark it revoked.
//
// THIS IS THE ONLY REVOCATION A FILE-BASED KEY HAS. A copied root.key signs
// tokens that are indistinguishable from legitimate ones, so the answer to a
// suspected copy is new material plus a revoked kid — not a revocation list of
// signatures nobody can attribute.
func (a Addr) Rotate() (oldKid string, root Root, err error) {
	path, err := a.Path()
	if err != nil {
		return "", Root{}, err
	}
	if prev, loadErr := load(path); loadErr == nil {
		oldKid = prev.Kid
	}
	// The new key is written over the old one; a caller that cannot load the
	// previous file (missing, corrupt) still gets a usable key, which is why the
	// error above is not fatal.
	if err := removeIfExists(path); err != nil {
		return oldKid, Root{}, err
	}
	root, err = a.create(path)
	return oldKid, root, err
}

// writeMeta records the kid beside the key so an operator can tell which key a
// log line refers to without decoding anything.
func writeMeta(dir, kid string) error {
	p := filepath.Join(dir, MetaName)
	body := "# which root key this data directory currently trusts\nkid = \"" + kid + "\"\nalgo = \"ed25519\"\n"
	if err := os.WriteFile(p, []byte(body), fileMode); err != nil {
		return fmt.Errorf("keys: write %s: %w", p, err)
	}
	return nil
}

// metaValue reads one `name = "value"` line out of a small key=value document.
// It is not a general parser: this file has three fields and no nesting, and a
// TOML dependency for that would be a larger surface than the thing it parses.
func metaValue(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

// removeIfExists is used by Rotate only. Everything else in this package either
// creates or reads.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keys: remove %s: %w", path, err)
	}
	return nil
}
