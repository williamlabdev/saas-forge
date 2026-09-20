package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// FieldEncryptor encrypts and decrypts field-level PII (AES-256-GCM).
type FieldEncryptor interface {
	Encrypt(plaintext []byte) (ciphertext, nonce []byte, err error)
	Decrypt(ciphertext, nonce []byte) ([]byte, error)
}

// BlindIndexer produces deterministic lookup hashes (HMAC-SHA256).
type BlindIndexer interface {
	Index(normalized string) ([]byte, error)
}

type AESGCMEncryptor struct {
	aead cipher.AEAD
}

func NewAESGCMEncryptor(key []byte) (*AESGCMEncryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: AES-256 key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new GCM: %w", err)
	}
	return &AESGCMEncryptor{aead: aead}, nil
}

func (e *AESGCMEncryptor) Encrypt(plaintext []byte) ([]byte, []byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	return e.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

func (e *AESGCMEncryptor) Decrypt(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != e.aead.NonceSize() {
		return nil, errors.New("crypto: invalid nonce size")
	}
	plain, err := e.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plain, nil
}

type HMACBlindIndexer struct {
	pepper []byte
}

func NewHMACBlindIndexer(pepper []byte) (*HMACBlindIndexer, error) {
	if len(pepper) < 16 {
		return nil, errors.New("crypto: blind index pepper must be at least 16 bytes")
	}
	return &HMACBlindIndexer{pepper: append([]byte(nil), pepper...)}, nil
}

func (b *HMACBlindIndexer) Index(normalized string) ([]byte, error) {
	mac := hmac.New(sha256.New, b.pepper)
	if _, err := mac.Write([]byte(normalized)); err != nil {
		return nil, fmt.Errorf("crypto: hmac write: %w", err)
	}
	return mac.Sum(nil), nil
}
