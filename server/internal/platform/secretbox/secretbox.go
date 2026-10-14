// Package secretbox derives purpose-bound subkeys from the application secret and encrypts values
// that must be stored but never read back in plaintext, such as SMTP credentials.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// KeyBytes is the length of every derived subkey and of the AES-256 key it feeds.
const KeyBytes = 32

var ErrMalformed = errors.New("ciphertext is malformed")

// Derive returns a 32-byte subkey bound to a single purpose. Distinct labels keep one leaked
// subkey from being usable for another purpose, which is the reason callers never share a key.
func Derive(secret []byte, label string) ([]byte, error) {
	if len(secret) < KeyBytes {
		return nil, fmt.Errorf("application secret is too short")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, label, KeyBytes)
	if err != nil {
		return nil, fmt.Errorf("cannot derive key: %w", err)
	}
	return key, nil
}

// Seal encrypts plaintext with AES-256-GCM under a fresh random nonce and returns nonce||sealed.
func Seal(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cannot build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cannot build aead: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("cannot read nonce: %w", err)
	}
	// The nonce is not secret, so prefixing it keeps decryption self-contained.
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a value produced by Seal. It reports ErrMalformed for truncated input and a wrapped
// error when authentication fails, so callers can tell "wrong key or tampered" from "empty".
func Open(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cannot build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cannot build aead: %w", err)
	}
	if len(ciphertext) <= aead.NonceSize() {
		return nil, ErrMalformed
	}
	nonce, sealed := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt stored secret: %w", err)
	}
	return plaintext, nil
}
