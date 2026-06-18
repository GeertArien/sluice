package sshkey

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateRoundTrips(t *testing.T) {
	priv, pub, err := Generate("sluice-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(priv, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("private key not OpenSSH PEM:\n%s", priv)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("public line not ed25519: %q", pub)
	}
	// The PEM must be parseable as a private key.
	if _, err := ssh.ParsePrivateKey([]byte(priv)); err != nil {
		t.Fatalf("generated key does not parse: %v", err)
	}
	// Deriving the public key from the private must match Generate's output.
	got, err := PublicKeyFromPrivate(priv)
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Fatalf("public mismatch:\n gen=%s\nfrom=%s", pub, got)
	}
}

func TestPublicKeyFromPrivateRejectsGarbage(t *testing.T) {
	if _, err := PublicKeyFromPrivate("not a key"); err == nil {
		t.Fatal("expected error for non-key input")
	}
}

func TestEnsureTrailingNewline(t *testing.T) {
	if EnsureTrailingNewline("x") != "x\n" {
		t.Fatal("should append newline")
	}
	if EnsureTrailingNewline("x\n") != "x\n" {
		t.Fatal("should not double newline")
	}
}
