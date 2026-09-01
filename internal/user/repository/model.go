package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

// userRow is the persistence representation (ciphertext + lookup hashes).
type userRow struct {
	ID                        uuid.UUID
	Username                  string
	UsernameLookupHash        []byte
	EmailLookupHash           []byte
	EmailEncrypted            []byte
	EmailEncryptedNonce       []byte
	DisplayNameEncrypted      []byte
	DisplayNameEncryptedNonce []byte
	PhoneEncrypted            []byte
	PhoneEncryptedNonce       []byte
	Preferences               []byte
	Status                    domain.Status
	StatusVersion             int
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	DeletedAt                 *time.Time
}
