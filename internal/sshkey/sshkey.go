// Package sshkey generates and inspects the ed25519 deploy keys Sluice can
// manage per bridge (an alternative to mounting ~/.ssh/id_ed25519).
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Generate creates an ed25519 keypair and returns the unencrypted OpenSSH
// private key (PEM) plus the authorized_keys public line to register on the
// source and Gitea.
func Generate(comment string) (privatePEM, publicAuthorized string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), authorizedLine(sshPub), nil
}

// PublicKeyFromPrivate parses an unencrypted OpenSSH private key and returns
// its authorized_keys public line. Used when an operator pastes a key.
func PublicKeyFromPrivate(privatePEM string) (string, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		return "", fmt.Errorf("not a valid unencrypted OpenSSH private key (passphrase-protected keys are not supported): %w", err)
	}
	return authorizedLine(signer.PublicKey()), nil
}

func authorizedLine(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

// EnsureTrailingNewline guarantees the PEM ends with a newline; ssh rejects a
// private key file that does not.
func EnsureTrailingNewline(s string) string {
	if !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}
