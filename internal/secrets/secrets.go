// Package secrets encrypts provider tokens before they reach the database.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// KeyEnv names the environment variable that holds the encryption key.
const KeyEnv = "SOLARGUARD_SECRET_KEY"

const minKeyLen = 32

// ErrNoKey reports that the encryption key is not configured.
var ErrNoKey = errors.New(KeyEnv + " não definida")

// Box encrypts and decrypts with one key.
type Box struct {
	aead cipher.AEAD
}

// New derives an AES-256-GCM key from a long random string.
func New(key string) (*Box, error) {
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("%s precisa ter pelo menos %d caracteres", KeyEnv, minKeyLen)
	}
	derived, err := hkdf.Key(sha256.New, []byte(key), nil, "solarguard provider token", 32)
	if err != nil {
		return nil, fmt.Errorf("derivar chave: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("criar cifra: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("criar cifra: %w", err)
	}
	return &Box{aead: aead}, nil
}

// FromEnv builds a Box from KeyEnv. It returns ErrNoKey when the variable is unset.
func FromEnv() (*Box, error) {
	key := os.Getenv(KeyEnv)
	if key == "" {
		return nil, ErrNoKey
	}
	return New(key)
}

// Seal returns the encrypted value, ready to store as text.
func (b *Box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("gerar nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(encoded string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decodificar segredo: %w", err)
	}
	if len(sealed) < b.aead.NonceSize() {
		return "", errors.New("segredo truncado")
	}
	nonce, ciphertext := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("não foi possível abrir o segredo: chave errada ou dado alterado")
	}
	return string(plaintext), nil
}
