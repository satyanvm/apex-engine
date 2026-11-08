// Package secret seals stored credentials with AES-256-GCM.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Box seals and opens values with one key.
type Box struct{ aead cipher.AEAD }

// New builds a Box from a base64-encoded 32-byte key.
func New(b64Key string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return nil, fmt.Errorf("APEX_SECRET_KEY is not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("APEX_SECRET_KEY must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plain, binding it to aad (the owning tenant and catalog) so a
// sealed value cannot be moved to another row.
func (b *Box) Seal(plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, aad), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value is too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], aad)
}
