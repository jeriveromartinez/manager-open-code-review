// Package crypto provides authenticated encryption for secrets stored at rest.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	// KeySize is the required key length in bytes (AES-256).
	KeySize = 32
	// nonceSize is the standard GCM nonce length in bytes.
	nonceSize = 12
)

// Box encrypts and decrypts small secrets with AES-256-GCM. The zero value is
// not usable; create a Box with NewBox.
type Box struct {
	aead cipher.AEAD
}

// NewBox returns a Box bound to key, which must be exactly 32 bytes (AES-256).
func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be exactly %d bytes, got %d", KeySize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new GCM: %w", err)
	}

	return &Box{aead: aead}, nil
}

// Seal encrypts plain with a fresh random nonce and returns the standard base64
// encoding of nonce||ciphertext+tag.
func (b *Box) Seal(plain []byte) (string, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}

	sealed := make([]byte, 0, nonceSize+len(plain)+b.aead.Overhead())
	sealed = append(sealed, nonce...)
	sealed = b.aead.Seal(sealed, nonce, plain, nil)

	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal. It returns an error, and no
// plaintext, when the value is malformed or fails authentication.
func (b *Box) Open(sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode sealed value: %w", err)
	}
	if len(raw) < nonceSize+b.aead.Overhead() {
		return nil, errors.New("crypto: sealed value is too short")
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plain, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: open sealed value: %w", err)
	}

	return plain, nil
}
