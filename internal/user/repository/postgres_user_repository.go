package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/idempotency"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/repository/sqlc"
)

// PostgresUserRepository implements UserRepository using sqlc-generated queries.
type PostgresUserRepository struct {
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	enc     crypto.FieldEncryptor
	outbox  outbox.Repository
	creds   CredentialStore
	idem    idempotency.RegistrationStore
	tenants TenantProvisioner
}

// CredentialStore is implemented by the auth repository. Two duties, both
// bound to a user-lifecycle event this module owns: writing credentials inside
// the registration transaction, and revoking sessions when the account leaves
// the active state.
type CredentialStore interface {
	InsertCredentialsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, passwordHash string) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
}

// TenantProvisioner is implemented by the tenant repository: self-serve
// registration creates a fresh tenant + owner membership inside the same
// transaction as the user row, so they commit or roll back together (D4/D8).
type TenantProvisioner interface {
	ProvisionOwnerTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (slug string, err error)
}

// NewPostgresUserRepository wires sqlc queries, encryption, outbox, credentials, idempotency, and tenant provisioning.
func NewPostgresUserRepository(pool *pgxpool.Pool, enc crypto.FieldEncryptor, ob outbox.Repository, creds CredentialStore, idem idempotency.RegistrationStore, tenants TenantProvisioner) *PostgresUserRepository {
	return &PostgresUserRepository{pool: pool, q: sqlc.New(pool), enc: enc, outbox: ob, creds: creds, idem: idem, tenants: tenants}
}

func (r *PostgresUserRepository) Create(ctx context.Context, u *domain.User, passwordHash, idempotencyKey string) error {
	if u.StatusVersion == 0 {
		u.StatusVersion = 1
	}
	row, err := r.domainToRow(u)
	if err != nil {
		return err
	}
	params, err := rowToCreateParams(row)
	if err != nil {
		return err
	}
	needsTx := (passwordHash != "" && r.creds != nil) || r.outbox != nil || (idempotencyKey != "" && r.idem != nil) || r.tenants != nil
	if !needsTx {
		return mapPgError(r.q.CreateUser(ctx, params))
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := r.q.WithTx(tx)
	if err := mapPgError(qtx.CreateUser(ctx, params)); err != nil {
		return err
	}
	if passwordHash != "" && r.creds != nil {
		if err := r.creds.InsertCredentialsTx(ctx, tx, u.ID, passwordHash); err != nil {
			return err
		}
	}
	if r.tenants != nil {
		// Self-serve: tenant + owner membership live or die with the user row
		// and its idempotency record — a replay never provisions twice (D8).
		if _, err := r.tenants.ProvisionOwnerTx(ctx, tx, u.ID); err != nil {
			return err
		}
	}
	if r.outbox != nil {
		if err := enqueueUserEvent(ctx, tx, r.outbox, u, outbox.EventUserCreated, u.StatusVersion); err != nil {
			return err
		}
	}
	if idempotencyKey != "" && r.idem != nil {
		if err := r.idem.RecordTx(ctx, tx, idempotencyKey, u.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *PostgresUserRepository) ByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	rec, err := r.q.UserByID(ctx, uuidToPG(id))
	if err != nil {
		return nil, mapPgError(err)
	}
	return r.rowToDomain(sqlcUserByIDToRow(rec))
}

func (r *PostgresUserRepository) ByEmailHash(ctx context.Context, emailHash []byte) (*domain.User, error) {
	rec, err := r.q.UserByEmailHash(ctx, emailHash)
	if err != nil {
		return nil, mapPgError(err)
	}
	return r.rowToDomain(sqlcUserByEmailHashToRow(rec))
}

func (r *PostgresUserRepository) ByUsernameHash(ctx context.Context, usernameHash []byte) (*domain.User, error) {
	rec, err := r.q.UserByUsernameHash(ctx, usernameHash)
	if err != nil {
		return nil, mapPgError(err)
	}
	return r.rowToDomain(sqlcUserByUsernameHashToRow(rec))
}

func (r *PostgresUserRepository) Update(ctx context.Context, u *domain.User) error {
	row, err := r.domainToRow(u)
	if err != nil {
		return err
	}
	params, err := rowToUpdateParams(row)
	if err != nil {
		return err
	}
	return mapPgError(r.q.UpdateUser(ctx, params))
}

func (r *PostgresUserRepository) UpdatePreferences(ctx context.Context, id uuid.UUID, prefs domain.Preferences, merge bool) error {
	raw, err := json.Marshal(prefs)
	if err != nil {
		return apperrors.Wrapf(apperrors.ErrInvalidPreferences.Code, apperrors.ErrInvalidPreferences.Message,
			apperrors.ErrInvalidPreferences.HTTPStatus, err, "marshal preferences")
	}
	pgID := uuidToPG(id)
	if merge {
		n, err := r.q.MergeUserPreferences(ctx, sqlc.MergeUserPreferencesParams{ID: pgID, Column2: raw})
		if err != nil {
			return mapPgError(err)
		}
		if n == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	}
	n, err := r.q.ReplaceUserPreferences(ctx, sqlc.ReplaceUserPreferencesParams{ID: pgID, Column2: raw})
	if err != nil {
		return mapPgError(err)
	}
	if n == 0 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *PostgresUserRepository) SoftDelete(ctx context.Context, id uuid.UUID) error {
	u, err := r.ByID(ctx, id)
	if err != nil {
		return err
	}
	n, err := r.q.SoftDeleteUser(ctx, uuidToPG(id))
	if err != nil {
		return mapPgError(err)
	}
	if n == 0 {
		return apperrors.ErrNotFound
	}
	// Flipping the status column does not end the sessions already issued
	// against it: an un-expired refresh token would keep minting new ones.
	// The auth path also re-checks status at every issue, so this is the
	// second of two independent stops, not the only one.
	if r.creds != nil {
		if err := r.creds.RevokeAllForUser(ctx, id); err != nil {
			return err
		}
	}
	u.Status = domain.StatusDeleted
	now := time.Now().UTC()
	u.DeletedAt = &now
	return r.publishUserEvent(ctx, u, outbox.EventUserDeleted)
}

func (r *PostgresUserRepository) PublishUserUpdated(ctx context.Context, u *domain.User) error {
	return r.publishUserEvent(ctx, u, outbox.EventUserUpdated)
}

func (r *PostgresUserRepository) PublishUserDeleted(ctx context.Context, u *domain.User) error {
	return r.publishUserEvent(ctx, u, outbox.EventUserDeleted)
}

func (r *PostgresUserRepository) publishUserEvent(ctx context.Context, u *domain.User, eventType string) error {
	if r.outbox == nil {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := r.q.WithTx(tx)
	version, err := qtx.BumpUserStatusVersion(ctx, uuidToPG(u.ID))
	if err != nil {
		return mapPgError(err)
	}
	u.StatusVersion = int(version)
	if err := enqueueUserEvent(ctx, tx, r.outbox, u, eventType, u.StatusVersion); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func rowToCreateParams(row *userRow) (sqlc.CreateUserParams, error) {
	var deleted pgtype.Timestamptz
	if row.DeletedAt != nil {
		deleted = timeToPG(*row.DeletedAt)
	}
	sv := int32(row.StatusVersion) //nolint:gosec // G115: StatusVersion originates from an int32 DB column (BumpUserStatusVersion); always within int32 range.
	if sv == 0 {
		sv = 1
	}
	return sqlc.CreateUserParams{
		ID:                        uuidToPG(row.ID),
		Username:                  row.Username,
		UsernameLookupHash:        row.UsernameLookupHash,
		EmailLookupHash:           row.EmailLookupHash,
		EmailEncrypted:            row.EmailEncrypted,
		EmailEncryptedNonce:       row.EmailEncryptedNonce,
		DisplayNameEncrypted:      row.DisplayNameEncrypted,
		DisplayNameEncryptedNonce: row.DisplayNameEncryptedNonce,
		PhoneEncrypted:            row.PhoneEncrypted,
		PhoneEncryptedNonce:       row.PhoneEncryptedNonce,
		Preferences:               row.Preferences,
		Status:                    statusToPG(row.Status),
		StatusVersion:             sv,
		CreatedAt:                 timeToPG(row.CreatedAt),
		UpdatedAt:                 timeToPG(row.UpdatedAt),
		DeletedAt:                 deleted,
	}, nil
}

func rowToUpdateParams(row *userRow) (sqlc.UpdateUserParams, error) {
	var deleted pgtype.Timestamptz
	if row.DeletedAt != nil {
		deleted = timeToPG(*row.DeletedAt)
	}
	return sqlc.UpdateUserParams{
		ID:                        uuidToPG(row.ID),
		Username:                  row.Username,
		UsernameLookupHash:        row.UsernameLookupHash,
		EmailLookupHash:           row.EmailLookupHash,
		EmailEncrypted:            row.EmailEncrypted,
		EmailEncryptedNonce:       row.EmailEncryptedNonce,
		DisplayNameEncrypted:      row.DisplayNameEncrypted,
		DisplayNameEncryptedNonce: row.DisplayNameEncryptedNonce,
		PhoneEncrypted:            row.PhoneEncrypted,
		PhoneEncryptedNonce:       row.PhoneEncryptedNonce,
		Preferences:               row.Preferences,
		Status:                    statusToPG(row.Status),
		DeletedAt:                 deleted,
	}, nil
}

func (r *PostgresUserRepository) domainToRow(u *domain.User) (*userRow, error) {
	emailEnc, emailNonce, err := r.enc.Encrypt([]byte(u.Email))
	if err != nil {
		return nil, apperrors.Wrapf("ENCRYPT_FAILED", "encrypt email", 500, err, "encrypt email")
	}

	row := &userRow{
		ID:                  u.ID,
		Username:            u.Username,
		UsernameLookupHash:  u.UsernameLookupHash,
		EmailLookupHash:     u.EmailLookupHash,
		EmailEncrypted:      emailEnc,
		EmailEncryptedNonce: emailNonce,
		Status:              u.Status,
		StatusVersion:       u.StatusVersion,
		CreatedAt:           u.CreatedAt,
		UpdatedAt:           u.UpdatedAt,
		DeletedAt:           u.DeletedAt,
	}

	if u.DisplayName != "" {
		enc, nonce, err := r.enc.Encrypt([]byte(u.DisplayName))
		if err != nil {
			return nil, apperrors.Wrapf("ENCRYPT_FAILED", "encrypt display_name", 500, err, "encrypt display_name")
		}
		row.DisplayNameEncrypted = enc
		row.DisplayNameEncryptedNonce = nonce
	}

	if u.Phone != "" {
		enc, nonce, err := r.enc.Encrypt([]byte(u.Phone))
		if err != nil {
			return nil, apperrors.Wrapf("ENCRYPT_FAILED", "encrypt phone", 500, err, "encrypt phone")
		}
		row.PhoneEncrypted = enc
		row.PhoneEncryptedNonce = nonce
	}

	raw, err := json.Marshal(u.Preferences)
	if err != nil {
		return nil, apperrors.Wrapf(apperrors.ErrInvalidPreferences.Code, apperrors.ErrInvalidPreferences.Message,
			apperrors.ErrInvalidPreferences.HTTPStatus, err, "marshal preferences")
	}
	row.Preferences = raw
	return row, nil
}

func (r *PostgresUserRepository) rowToDomain(row userRow) (*domain.User, error) {
	email, err := r.enc.Decrypt(row.EmailEncrypted, row.EmailEncryptedNonce)
	if err != nil {
		return nil, apperrors.Wrapf("DECRYPT_FAILED", "decrypt email", 500, err, "decrypt email")
	}

	u := &domain.User{
		ID:                 row.ID,
		Username:           row.Username,
		Email:              string(email),
		Preferences:        domain.Preferences{},
		Status:             row.Status,
		StatusVersion:      row.StatusVersion,
		UsernameLookupHash: row.UsernameLookupHash,
		EmailLookupHash:    row.EmailLookupHash,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
		DeletedAt:          row.DeletedAt,
	}

	if len(row.DisplayNameEncrypted) > 0 {
		plain, err := r.enc.Decrypt(row.DisplayNameEncrypted, row.DisplayNameEncryptedNonce)
		if err != nil {
			return nil, apperrors.Wrapf("DECRYPT_FAILED", "decrypt display_name", 500, err, "decrypt display_name")
		}
		u.DisplayName = string(plain)
	}

	if len(row.PhoneEncrypted) > 0 {
		plain, err := r.enc.Decrypt(row.PhoneEncrypted, row.PhoneEncryptedNonce)
		if err != nil {
			return nil, apperrors.Wrapf("DECRYPT_FAILED", "decrypt phone", 500, err, "decrypt phone")
		}
		u.Phone = string(plain)
	}

	if len(row.Preferences) > 0 {
		if err := json.Unmarshal(row.Preferences, &u.Preferences); err != nil {
			return nil, apperrors.Wrapf(apperrors.ErrInvalidPreferences.Code, apperrors.ErrInvalidPreferences.Message,
				apperrors.ErrInvalidPreferences.HTTPStatus, err, "unmarshal preferences")
		}
	}
	if u.Preferences == nil {
		u.Preferences = domain.Preferences{}
	}
	return u, nil
}

func mapPgError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperrors.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "uq_users_email_lookup_hash":
			return apperrors.ErrEmailTaken
		case "uq_users_username_lookup_hash":
			return apperrors.ErrUsernameTaken
		default:
			if pgErr.ColumnName == "email_lookup_hash" {
				return apperrors.ErrEmailTaken
			}
			if pgErr.ColumnName == "username_lookup_hash" {
				return apperrors.ErrUsernameTaken
			}
		}
		return apperrors.ErrEmailTaken
	}
	return fmt.Errorf("repository: %w", err)
}
