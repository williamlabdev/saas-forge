package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/repository/sqlc"
)

func uuidToPG(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func uuidFromPG(id pgtype.UUID) (uuid.UUID, error) {
	if !id.Valid {
		return uuid.Nil, nil
	}
	return uuid.FromBytes(id.Bytes[:])
}

func timeToPG(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timeFromPG(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}

func statusToPG(s domain.Status) string {
	return string(s)
}

func statusFromPG(s string) domain.Status {
	return domain.Status(s)
}

func sqlcUserByIDToRow(u sqlc.UserByIDRow) userRow {
	return userSelectRowToRow(
		u.ID, u.Username, u.UsernameLookupHash, u.EmailLookupHash,
		u.EmailEncrypted, u.EmailEncryptedNonce,
		u.DisplayNameEncrypted, u.DisplayNameEncryptedNonce,
		u.PhoneEncrypted, u.PhoneEncryptedNonce,
		u.Preferences, u.Status, u.StatusVersion,
		u.CreatedAt, u.UpdatedAt, u.DeletedAt,
	)
}

func sqlcUserByEmailHashToRow(u sqlc.UserByEmailHashRow) userRow {
	return userSelectRowToRow(
		u.ID, u.Username, u.UsernameLookupHash, u.EmailLookupHash,
		u.EmailEncrypted, u.EmailEncryptedNonce,
		u.DisplayNameEncrypted, u.DisplayNameEncryptedNonce,
		u.PhoneEncrypted, u.PhoneEncryptedNonce,
		u.Preferences, u.Status, u.StatusVersion,
		u.CreatedAt, u.UpdatedAt, u.DeletedAt,
	)
}

func sqlcUserByUsernameHashToRow(u sqlc.UserByUsernameHashRow) userRow {
	return userSelectRowToRow(
		u.ID, u.Username, u.UsernameLookupHash, u.EmailLookupHash,
		u.EmailEncrypted, u.EmailEncryptedNonce,
		u.DisplayNameEncrypted, u.DisplayNameEncryptedNonce,
		u.PhoneEncrypted, u.PhoneEncryptedNonce,
		u.Preferences, u.Status, u.StatusVersion,
		u.CreatedAt, u.UpdatedAt, u.DeletedAt,
	)
}

func userSelectRowToRow(
	id pgtype.UUID,
	username string,
	usernameLookupHash, emailLookupHash []byte,
	emailEncrypted, emailEncryptedNonce []byte,
	displayNameEncrypted, displayNameEncryptedNonce []byte,
	phoneEncrypted, phoneEncryptedNonce []byte,
	preferences []byte,
	status string,
	statusVersion int32,
	createdAt, updatedAt pgtype.Timestamptz,
	deletedAt pgtype.Timestamptz,
) userRow {
	return userRow{
		ID:                        mustUUID(id),
		Username:                  username,
		UsernameLookupHash:        usernameLookupHash,
		EmailLookupHash:           emailLookupHash,
		EmailEncrypted:            emailEncrypted,
		EmailEncryptedNonce:       emailEncryptedNonce,
		DisplayNameEncrypted:      displayNameEncrypted,
		DisplayNameEncryptedNonce: displayNameEncryptedNonce,
		PhoneEncrypted:            phoneEncrypted,
		PhoneEncryptedNonce:       phoneEncryptedNonce,
		Preferences:               preferences,
		Status:                    statusFromPG(status),
		StatusVersion:             int(statusVersion),
		CreatedAt:                 createdAt.Time,
		UpdatedAt:                 updatedAt.Time,
		DeletedAt:                 timeFromPG(deletedAt),
	}
}

func mustUUID(id pgtype.UUID) uuid.UUID {
	u, err := uuidFromPG(id)
	if err != nil {
		return uuid.Nil
	}
	return u
}
