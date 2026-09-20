package idempotency

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// ErrInvalidKey is returned when Idempotency-Key format is invalid.
var ErrInvalidKey = apperrors.New("INVALID_IDEMPOTENCY_KEY", "idempotency key must be 8-128 chars (A-Za-z0-9_-)", 400)

// RegistrationStore maps registration idempotency keys to user IDs.
type RegistrationStore interface {
	UserIDByKey(ctx context.Context, key string) (uuid.UUID, bool, error)
	RecordTx(ctx context.Context, tx pgx.Tx, key string, userID uuid.UUID) error
}

// PostgresRegistrationStore implements RegistrationStore.
type PostgresRegistrationStore struct {
	pool *pgxpool.Pool
}

func NewPostgresRegistrationStore(pool *pgxpool.Pool) *PostgresRegistrationStore {
	return &PostgresRegistrationStore{pool: pool}
}

// NormalizeKey validates and trims the client idempotency key.
func NormalizeKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", nil
	}
	if !keyPattern.MatchString(key) {
		return "", ErrInvalidKey
	}
	return key, nil
}

func (s *PostgresRegistrationStore) UserIDByKey(ctx context.Context, key string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT user_id FROM registration_idempotency WHERE idempotency_key = $1
	`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("idempotency: lookup: %w", err)
	}
	return id, true, nil
}

func (s *PostgresRegistrationStore) RecordTx(ctx context.Context, tx pgx.Tx, key string, userID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO registration_idempotency (idempotency_key, user_id) VALUES ($1, $2)
	`, key, userID)
	return err
}

// IsUniqueViolation reports duplicate idempotency key insert.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "registration_idempotency_pkey"
}
