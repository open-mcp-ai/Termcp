package session

import (
	"strings"
	"testing"
)

func TestAuthSummaryNamesTheAttemptWithoutLeakingCredentials(t *testing.T) {
	r := &RemoteSSH{
		Host: "example.test", Port: 2222, User: "pi",
		Password: "secret-password", PrivateKey: "secret-private-key",
		KeyPassphrase: "secret-passphrase",
	}
	got := authSummary(r)
	for _, want := range []string{
		`user "pi"`, "example.test:2222", "password set",
		"private_key set", "from the profile's `private_key`", "with a passphrase",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authSummary %q does not identify %q", got, want)
		}
	}
	for _, secret := range []string{r.Password, r.PrivateKey, r.KeyPassphrase} {
		if strings.Contains(got, secret) {
			t.Errorf("authSummary leaks credential %q: %s", secret, got)
		}
	}
}

func TestAuthSummaryMakesMissingUserVisible(t *testing.T) {
	got := authSummary(&RemoteSSH{Host: "example.test", Port: 22})
	for _, want := range []string{"(empty", "example.test:22", "password absent", "private_key absent"} {
		if !strings.Contains(got, want) {
			t.Errorf("authSummary %q does not identify %q", got, want)
		}
	}
}
