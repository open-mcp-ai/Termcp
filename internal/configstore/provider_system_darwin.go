//go:build darwin

package configstore

import "strings"

// SystemProvider uses the login Keychain through its interactive stdin mode.
func SystemProvider() Provider {
	return credentialProvider{name: "keychain", store: keychainStore{}}
}

type keychainStore struct{}

func (keychainStore) store(reference, secret string) error {
	command := "add-generic-password -s " + systemService + " -a " + reference + " -w " + secret + "\n"
	output, err := credentialCommand("/usr/bin/security", []string{"-i"}, strings.NewReader(command))
	clear(output)
	if err != nil {
		return err
	}
	got, err := (keychainStore{}).load(reference)
	if err != nil || got != secret {
		return ErrProviderUnavailable
	}
	return nil
}

func (keychainStore) load(reference string) (string, error) {
	output, err := credentialCommand(
		"/usr/bin/security",
		[]string{"find-generic-password", "-s", systemService, "-a", reference, "-w"},
		nil,
	)
	defer clear(output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
