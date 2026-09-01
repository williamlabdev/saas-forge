package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

func enqueueUserEvent(ctx context.Context, tx pgx.Tx, ob outbox.Repository, u *domain.User, eventType string, version int) error {
	payload, err := outbox.MarshalPayload(outbox.UserPayload{
		UserID:        u.ID.String(),
		Status:        string(u.Status),
		StatusVersion: version,
		Username:      u.Username,
	})
	if err != nil {
		return err
	}
	key := outbox.IdempotencyKey(u.ID, version)
	return ob.EnqueueTx(ctx, tx, u.ID, eventType, payload, key)
}
