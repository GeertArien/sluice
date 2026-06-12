// Package secrets encrypts Gitea tokens and webhook secrets at rest using
// NaCl secretbox with a key supplied via the environment (spec §9.3).
package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
)

// Box wraps a 32-byte secretbox key.
type Box struct{ key [32]byte }

// New derives a Box from a 64-char hex key (32 bytes).
func New(hexKey string) (*Box, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("secret key must be 64 hex characters (32 bytes); generate one with: openssl rand -hex 32")
	}
	b := &Box{}
	copy(b.key[:], raw)
	return b, nil
}

// Encrypt seals plaintext; the random nonce is prepended to the ciphertext.
func (b *Box) Encrypt(plaintext string) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &b.key), nil
}

// Decrypt opens a value produced by Encrypt.
func (b *Box) Decrypt(ciphertext []byte) (string, error) {
	if len(ciphertext) < 24 {
		return "", fmt.Errorf("ciphertext too short")
	}
	var nonce [24]byte
	copy(nonce[:], ciphertext[:24])
	plain, ok := secretbox.Open(nil, ciphertext[24:], &nonce, &b.key)
	if !ok {
		return "", errors.New("decryption failed (wrong SLUICE_SECRET_KEY?)")
	}
	return string(plain), nil
}
