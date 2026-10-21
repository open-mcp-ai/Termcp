package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestBuildClientConfig_MissingUser(t *testing.T) {
	_, err := BuildClientConfig(DialAuth{
		User:             "",
		Password:         "x",
		TrustUnknownHost: true,
		DialTimeout:      time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "ssh_user") {
		t.Fatalf("expected ssh_user error, got %v", err)
	}
}

func TestBuildClientConfig_NoAuth(t *testing.T) {
	_, err := BuildClientConfig(DialAuth{
		User:             "u",
		TrustUnknownHost: true,
	})
	if err == nil || !strings.Contains(err.Error(), "ssh_password") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestBuildClientConfig_StrictHostWithoutKnownHosts(t *testing.T) {
	_, err := BuildClientConfig(DialAuth{
		User:              "u",
		Password:          "p",
		TrustUnknownHost:  false,
		KnownHostsContent: "",
	})
	if err == nil || !strings.Contains(err.Error(), "ssh_known_hosts") {
		t.Fatalf("expected known_hosts error, got %v", err)
	}
}

func TestBuildClientConfig_SignersSatisfyAuth(t *testing.T) {
	// The vault design's SSH CA seam: a caller with an ephemeral certificate
	// authenticates without a password or a static private key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := BuildClientConfig(DialAuth{
		User:             "u",
		TrustUnknownHost: true,
		Signers:          []ssh.Signer{signer},
	})
	if err != nil {
		t.Fatalf("signers must satisfy the auth requirement: %v", err)
	}
	if len(cfg.Auth) == 0 {
		t.Fatal("no auth methods built from a signer")
	}
}
