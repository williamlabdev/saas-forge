package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
)

// ListFilter narrows ListForUser. TenantID is the caller's CURRENT tenant
// (from the JWT), applied as `tenant_id = TenantID OR tenant_id IS NULL` —
// never as "no filter": a nil TenantID (no active tenant on the subject)
// still scopes to tenant_id IS NULL rows only, because SQL's
// `tenant_id = NULL` is UNKNOWN rather than true, so a caller with no active
// tenant can never see another tenant's notice by having none. UnreadOnly
// narrows to ReadAt IS NULL. Limit <= 0 falls back to the repository's
// default cap.
type ListFilter struct {
	TenantID   *uuid.UUID
	UnreadOnly bool
	Limit      int
}

type NotificationRepository interface {
	Create(ctx context.Context, n *domain.Notification) error
	// ListForUser returns userID's mailbox, most recent first, narrowed by
	// filter.
	ListForUser(ctx context.Context, userID uuid.UUID, filter ListFilter) ([]*domain.Notification, error)
	// UnreadCount counts userID's unread mailbox, scoped by the same tenant
	// rule ListForUser applies when tenantID is non-nil.
	UnreadCount(ctx context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int, error)
	// MarkRead marks one notification read. It is scoped to userID as well as
	// id: a notification that exists but belongs to someone else returns
	// apperrors.ErrNotFound rather than a forbidden — existence of another
	// user's row is not something a caller gets to learn from the error shape.
	// Returns the updated row so the handler can answer with its current state
	// (idempotent: marking an already-read notification read again succeeds
	// and returns it unchanged rather than erroring).
	MarkRead(ctx context.Context, userID, id uuid.UUID) (*domain.Notification, error)
	// MarkAllRead marks every currently-unread notification in userID's
	// mailbox read, scoped by tenantID the same way ListForUser is (nil means
	// no tenant filter). Returns the number of rows actually changed.
	MarkAllRead(ctx context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int, error)
}
