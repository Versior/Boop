// Package secretbox stores Boop's secrets with AES-256-GCM under
// BOOP_MASTER_KEY (docs/PRODUCT.md §5.4), so a copy of the database alone never
// reveals an OAuth client secret or an AI API key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeyBytes is the required master key size: AES-256 takes 32 bytes.
const KeyBytes = 32

// NonceBytes is the GCM nonce size; every Seal draws a fresh random one.
const NonceBytes = 12

var (
	// ErrKeySize rejects a master key that is not exactly 32 bytes.
	ErrKeySize = errors.New("secretbox: master key must be 32 bytes")
	// ErrKeyEncoding rejects a master key that is not base64.
	ErrKeyEncoding = errors.New("secretbox: master key is not valid base64")
	// ErrCiphertext covers a wrong key, a truncated row and a tampered value:
	// GCM authenticates the ciphertext, so all three fail the same way instead
	// of returning garbage.
	ErrCiphertext = errors.New("secretbox: value cannot be decrypted with this key")
)

// Box encrypts and decrypts stored secrets.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a raw 32-byte master key.
func New(key []byte) (*Box, error) {
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// NewFromBase64 builds a Box from the BOOP_MASTER_KEY value.
func NewFromBase64(encoded string) (*Box, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Report the encoding failure without echoing the key material.
		return nil, fmt.Errorf("%w: %v", ErrKeyEncoding, err)
	}
	return New(raw)
}

// Seal encrypts a value and returns the nonce to store next to it. The binding
// is authenticated as GCM additional data, so a ciphertext can only ever be
// opened for the same binding it was sealed for: moving a stored value to
// another setting (or another row) fails instead of returning plaintext. The
// plaintext is never written anywhere by this package.
func (b *Box) Seal(binding, value string) (nonce, ciphertext []byte, err error) {
	if b == nil || b.aead == nil {
		return nil, nil, errors.New("secretbox: nil box")
	}
	nonce = make([]byte, NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secretbox: generate nonce: %w", err)
	}
	return nonce, b.aead.Seal(nil, nonce, []byte(value), []byte(binding)), nil
}

// Open decrypts a stored value. The binding must be the exact value Seal
// received, otherwise the authentication fails like a wrong key would.
func (b *Box) Open(binding string, nonce, ciphertext []byte) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("secretbox: nil box")
	}
	if len(nonce) != NonceBytes || len(ciphertext) == 0 {
		return "", ErrCiphertext
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, []byte(binding))
	if err != nil {
		// The cipher error names no key material and no plaintext.
		return "", fmt.Errorf("%w: %v", ErrCiphertext, err)
	}
	return string(plaintext), nil
}
