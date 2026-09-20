package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ErrIdempotencyKeyTaken says the key was spent between a lookup that found
// nothing and the insert that followed — two first-tries of one key racing.
//
// It never reaches the client. The service catches it, abandons the entry it
// was in the middle of creating (the whole thing is one transaction, so
// abandoning is just returning the error), and replays the winner's row. What
// the caller sees is what it would have seen had its retry arrived a moment
// later: the entry the key produced.
var ErrIdempotencyKeyTaken = errors.New("content: idempotency key was spent concurrently")

// ErrIdempotencyFingerprintMismatch is the 409 of ADR-013 §9: this key is spent,
// and on a DIFFERENT request than the one now being made.
//
// Refusing is the whole point of storing the fingerprint. The alternative —
// returning the original entry — answers "did you save this?" with a 201 and
// somebody else's content, which an unattended writer has no way to detect.
var ErrIdempotencyFingerprintMismatch = apperrors.New(
	"CONTENT_IDEMPOTENCY_KEY_REUSED",
	"this Idempotency-Key was already used for a different request; use a new key",
	http.StatusConflict,
)

// EntryIdempotency is one spent key: which request it was spent on, and what
// that request produced.
type EntryIdempotency struct {
	TenantID string
	ActorKey string
	Key      string
	// Fingerprint digests the request. See 000036's column comment for what goes
	// into it and why an over-strict digest is the safe direction.
	Fingerprint []byte
	EntryID     uuid.UUID
}

func (r *PostgresContentRepository) FindEntryIdempotency(ctx context.Context, tenantID, actorKey, idemKey string) (*EntryIdempotency, error) {
	rec := EntryIdempotency{TenantID: tenantID, ActorKey: actorKey, Key: idemKey}
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT fingerprint, entry_id
			FROM entry_idempotency
			WHERE tenant_id = $1 AND actor_key = $2 AND idem_key = $3`,
			tenantID, actorKey, idemKey,
		).Scan(&rec.Fingerprint, &rec.EntryID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Unspent. Not an error: the overwhelmingly common case is a key nobody
		// has used, and a caller that has to distinguish "no row" from "failed to
		// look" by string is a caller that will eventually stop distinguishing.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find entry idempotency: %w", err)
	}
	return &rec, nil
}

func (r *PostgresContentRepository) RecordEntryIdempotency(ctx context.Context, rec EntryIdempotency) error {
	return r.withTenant(ctx, rec.TenantID, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO entry_idempotency (tenant_id, actor_key, idem_key, fingerprint, entry_id)
			VALUES ($1, $2, $3, $4, $5)`,
			rec.TenantID, rec.ActorKey, rec.Key, rec.Fingerprint, rec.EntryID,
		)
		// No ON CONFLICT DO NOTHING, deliberately. Swallowing the collision would
		// commit an entry that no key points at while the caller is told its key
		// produced it — a duplicate row created by the very statement meant to
		// prevent one. The race has a correct answer and it is upstream of here.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrIdempotencyKeyTaken
		}
		if err != nil {
			return fmt.Errorf("record entry idempotency: %w", err)
		}
		return nil
	})
}
