package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/notification/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// memRepo is a minimal in-memory double for repository.NotificationRepository
// — enough to exercise the service's own logic (authorization, tenant
// resolution, DTO mapping) without a database. It mirrors the real
// postgres_repository.go's MarkRead boundary rule exactly (task 8: "someone
// else's notification -> not found, no existence leak"), since that boundary
// is the one property this test suite must pin regardless of which
// repository implementation backs it.
type memRepo struct {
	items []*domain.Notification
}

func (m *memRepo) Create(_ context.Context, n *domain.Notification) error {
	m.items = append(m.items, n)
	return nil
}

func (m *memRepo) ListForUser(_ context.Context, userID uuid.UUID, filter repository.ListFilter) ([]*domain.Notification, error) {
	var out []*domain.Notification
	for _, it := range m.items {
		if it.UserID != userID {
			continue
		}
		if !tenantMatches(it.TenantID, filter.TenantID) {
			continue
		}
		if filter.UnreadOnly && it.ReadAt != nil {
			continue
		}
		out = append(out, it)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (m *memRepo) UnreadCount(_ context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int, error) {
	n := 0
	for _, it := range m.items {
		if it.UserID == userID && it.ReadAt == nil && tenantMatches(it.TenantID, tenantID) {
			n++
		}
	}
	return n, nil
}

// MarkRead is scoped to userID as well as id, same as the real repository:
// a row that exists but belongs to someone else answers ErrNotFound, not a
// forbidden — existence of another user's row must not leak through the
// error shape (task 8's boundary test below pins exactly this).
func (m *memRepo) MarkRead(_ context.Context, userID, id uuid.UUID) (*domain.Notification, error) {
	for _, it := range m.items {
		if it.ID == id && it.UserID == userID {
			if it.ReadAt == nil {
				now := time.Now().UTC()
				it.ReadAt = &now
			}
			return it, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) MarkAllRead(_ context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int, error) {
	now := time.Now().UTC()
	n := 0
	for _, it := range m.items {
		if it.UserID == userID && it.ReadAt == nil && tenantMatches(it.TenantID, tenantID) {
			it.ReadAt = &now
			n++
		}
	}
	return n, nil
}

// tenantMatches mirrors ListFilter's documented rule: `tenant_id = TenantID
// OR tenant_id IS NULL`.
func tenantMatches(rowTenant, filterTenant *uuid.UUID) bool {
	if rowTenant == nil {
		return true
	}
	return filterTenant != nil && *rowTenant == *filterTenant
}

func subjectCtx(uid uuid.UUID, tenantID string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:   uid,
		TenantID: tenantID,
		Roles:    []string{"member"},
	})
}

func TestNotificationService_CreateAndList(t *testing.T) {
	uid := uuid.New()
	repo := &memRepo{}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())
	ctx := subjectCtx(uid, "")

	dto, err := svc.Create(ctx, CreateInput{Title: "Hi", Body: "Body"})
	require.NoError(t, err)
	assert.Equal(t, "Hi", dto.Title)
	assert.Equal(t, domain.KindGeneral, dto.Kind, "the self-notify create path always writes the general kind")

	list, err := svc.ListMine(ctx, 10, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestNotificationService_Unauthorized(t *testing.T) {
	svc := NewNotificationService(&memRepo{}, authz.NewRBACAuthorizer())
	_, err := svc.ListMine(context.Background(), 10, false)
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)
}

func TestNotificationService_ListMine_UnreadFilter(t *testing.T) {
	uid := uuid.New()
	repo := &memRepo{}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())
	ctx := subjectCtx(uid, "")

	_, err := svc.Create(ctx, CreateInput{Title: "A", Body: "a"})
	require.NoError(t, err)
	dto, err := svc.Create(ctx, CreateInput{Title: "B", Body: "b"})
	require.NoError(t, err)
	_, err = svc.MarkRead(ctx, dto.ID)
	require.NoError(t, err)

	all, err := svc.ListMine(ctx, 10, false)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	unread, err := svc.ListMine(ctx, 10, true)
	require.NoError(t, err)
	require.Len(t, unread, 1)
	assert.Equal(t, "A", unread[0].Title)
}

func TestNotificationService_UnreadCount(t *testing.T) {
	uid := uuid.New()
	repo := &memRepo{}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())
	ctx := subjectCtx(uid, "")

	_, err := svc.Create(ctx, CreateInput{Title: "A", Body: "a"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateInput{Title: "B", Body: "b"})
	require.NoError(t, err)

	count, err := svc.UnreadCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// The mark-read authorization boundary (task 8): a notification that exists
// but belongs to someone else must answer ErrNotFound, exactly like a
// notification that does not exist at all — the handler must never be able
// to distinguish "not yours" from "not real" through the error it gets back.
func TestNotificationService_MarkRead_OtherUsersNotificationIsNotFound(t *testing.T) {
	owner := uuid.New()
	stranger := uuid.New()
	repo := &memRepo{}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())

	ownerCtx := subjectCtx(owner, "")
	dto, err := svc.Create(ownerCtx, CreateInput{Title: "Mine", Body: "body"})
	require.NoError(t, err)

	strangerCtx := subjectCtx(stranger, "")
	_, err = svc.MarkRead(strangerCtx, dto.ID)
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrNotFound.Code, ae.Code, "someone else's notification must read back as not-found, never forbidden")

	nonexistentErr := func() error {
		_, err := svc.MarkRead(strangerCtx, uuid.New())
		return err
	}()
	require.Error(t, nonexistentErr)
	nae, ok := apperrors.As(nonexistentErr)
	require.True(t, ok)
	assert.Equal(t, ae.Code, nae.Code, "a real-but-not-yours id and a nonexistent id must be indistinguishable")
}

func TestNotificationService_MarkRead_OwnNotificationSucceeds(t *testing.T) {
	uid := uuid.New()
	repo := &memRepo{}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())
	ctx := subjectCtx(uid, "")

	dto, err := svc.Create(ctx, CreateInput{Title: "Mine", Body: "body"})
	require.NoError(t, err)
	assert.False(t, dto.Read)

	got, err := svc.MarkRead(ctx, dto.ID)
	require.NoError(t, err)
	assert.True(t, got.Read)
}

func TestNotificationService_MarkAllRead_ScopedToCurrentTenant(t *testing.T) {
	uid := uuid.New()
	tenantA := uuid.New()
	tenantB := uuid.New()
	repo := &memRepo{items: []*domain.Notification{
		{ID: uuid.New(), UserID: uid, Title: "a1", TenantID: &tenantA, CreatedAt: time.Now()},
		{ID: uuid.New(), UserID: uid, Title: "a2", TenantID: &tenantA, CreatedAt: time.Now()},
		{ID: uuid.New(), UserID: uid, Title: "b1", TenantID: &tenantB, CreatedAt: time.Now()},
	}}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())
	ctx := subjectCtx(uid, tenantA.String())

	n, err := svc.MarkAllRead(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "only the current tenant's unread rows are marked, not tenant B's")

	countB, err := repo.UnreadCount(context.Background(), uid, &tenantB)
	require.NoError(t, err)
	assert.Equal(t, 1, countB, "tenant B's notification is untouched")
}

var _ repository.NotificationRepository = (*memRepo)(nil)
