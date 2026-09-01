package config

import (
	"encoding/hex"
	"errors"
	"os"
)

// User holds runtime configuration for the user module.
type User struct {
	DatabaseURL      string
	EncryptionKey    []byte // 32 bytes AES-256
	BlindIndexPepper []byte
	HTTPAddr         string
}

func LoadUserFromEnv() (User, error) {
	cfg := User{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("config: DATABASE_URL is required")
	}

	encKey, err := hex.DecodeString(os.Getenv("ENCRYPTION_KEY_HEX"))
	if err != nil || len(encKey) != 32 {
		return cfg, errors.New("config: ENCRYPTION_KEY_HEX must be 64 hex chars (32 bytes)")
	}
	cfg.EncryptionKey = encKey

	pepper, err := hex.DecodeString(os.Getenv("BLIND_INDEX_PEPPER_HEX"))
	if err != nil || len(pepper) < 16 {
		return cfg, errors.New("config: BLIND_INDEX_PEPPER_HEX must be at least 32 hex chars")
	}
	cfg.BlindIndexPepper = pepper

	return cfg, nil
}

// ValidateUserSecrets rejects the publicly-known dev throwaway encryption key
// and blind-index pepper when running in production (TKT-R3): they can decrypt
// stored PII and forge blind indexes, and length checks alone accept them.
func ValidateUserSecrets(cfg User, production bool) error {
	if !production {
		return nil
	}
	if IsKnownDevSecret(cfg.EncryptionKey) {
		return errors.New("config: ENCRYPTION_KEY_HEX is the publicly-known dev throwaway from .env.example — generate a real key: openssl rand -hex 32")
	}
	if IsKnownDevSecret(cfg.BlindIndexPepper) {
		return errors.New("config: BLIND_INDEX_PEPPER_HEX is the publicly-known dev throwaway from .env.example — generate a real pepper: openssl rand -hex 32")
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
