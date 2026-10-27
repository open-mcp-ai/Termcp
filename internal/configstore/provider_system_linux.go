//go:build linux

package configstore

import "strings"

// SystemProvider uses libsecret's Secret Service implementation.
func SystemProvider() Provider {
	return credentialProvider{name: "secret-service", store: secretServiceStore{}}
}

type secretServiceStore struct{}

func (secretServiceStore) store(reference, secret string) error {
	output, err := credentialCommand(
		"secret-tool",
		[]string{"store", "--label=Termcp configuration key", "service", systemService, "slot", reference},
		strings.NewReader(secret),
	)
	clear(output)
	return err
}

func (secretServiceStore) load(reference string) (string, error) {
	output, err := credentialCommand(
		"secret-tool",
		[]string{"lookup", "service", systemService, "slot", reference},
		nil,
	)
	defer clear(output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
