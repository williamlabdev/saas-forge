package service

import (
	"context"
	"testing"

	"__MODULE__/internal/__domain__/domain"
	"__MODULE__/internal/__domain__/repository"
	"__MODULE__/internal/pkg/authn"
	"__MODULE__/internal/pkg/authz"
	apperrors "__MODULE__/internal/pkg/errors"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memRepo struct {
	items []*domain.__Domain__
}

func (m *memRepo) Create(_ context.Context, x *domain.__Domain__) error {
	m.items = append(m.items, x)
	return nil
}

func (m *memRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.__Domain__, error) {
	for _, it := range m.items {
		if it.ID == id {
			return it, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) List(_ context.Context, f repository.ListFilter) (repository.ListResult, error) {
	var out []*domain.__Domain__
	for _, it := range m.items {
		if it.OwnerID == f.OwnerID {
			out = append(out, it)
		}
	}
	return repository.ListResult{Items: out, Total: len(out)}, nil
}

func (m *memRepo) Update(_ context.Context, x *domain.__Domain__) error {
	for i, it := range m.items {
		if it.ID == x.ID {
			m.items[i] = x
			return nil
		}
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) Delete(_ context.Context, id uuid.UUID) error {
	for i, it := range m.items {
		if it.ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			return nil
		}
	}
	return apperrors.ErrNotFound
}

func newTestService() __Domain__Service {
	return New__Domain__Service(&memRepo{}, authz.NewAllowAllAuthorizer())
}

func ctxWithSubject(uid uuid.UUID) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID: uid,
		Roles:  []string{"member"},
	})
}

func TestService_CreateGetListUpdateDelete(t *testing.T) {
	uid := uuid.New()
	svc := newTestService()
	ctx := ctxWithSubject(uid)

	created, err := svc.Create(ctx, CreateInput{Name: "First"})
	require.NoError(t, err)
	assert.Equal(t, "First", created.Name)
	assert.Equal(t, domain.StatusActive, created.Status)

	got, err := svc.GetByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)

	list, err := svc.List(ctx, ListInput{Limit: 10})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)

	updated, err := svc.Update(ctx, UpdateInput{ID: created.ID, Name: "Second", Status: domain.StatusArchived})
	require.NoError(t, err)
	assert.Equal(t, "Second", updated.Name)
	assert.Equal(t, domain.StatusArchived, updated.Status)

	require.NoError(t, svc.Delete(ctx, created.ID))
	_, err = svc.GetByID(ctx, created.ID)
	require.Error(t, err)
}

func TestService_Validation(t *testing.T) {
	cases := []struct {
		name string
		in   CreateInput
		ok   bool
	}{
		{name: "valid", in: CreateInput{Name: "ok"}, ok: true},
		{name: "empty name", in: CreateInput{Name: ""}, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService()
			ctx := ctxWithSubject(uuid.New())
			_, err := svc.Create(ctx, tc.in)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestService_Unauthorized(t *testing.T) {
	svc := newTestService()
	_, err := svc.List(context.Background(), ListInput{Limit: 10})
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)
}

var _ repository.__Domain__Repository = (*memRepo)(nil)

// Deliberately runs on AllowAllAuthorizer: the point is that the service layer
// refuses on its own, without help from a policy. AUTHZ_MODE=allow is the dev
// default, and an action-level rbac/opa rule ("may this subject read
// __domain__s?") is no help either — neither is given the record.
func TestService_CrossOwnerAccessDenied(t *testing.T) {
	owner, intruder := uuid.New(), uuid.New()
	repo := &memRepo{}
	svc := New__Domain__Service(repo, authz.NewAllowAllAuthorizer())

	created, err := svc.Create(ctxWithSubject(owner), CreateInput{Name: "Owner's"})
	require.NoError(t, err)

	other := ctxWithSubject(intruder)

	_, err = svc.GetByID(other, created.ID)
	assert.ErrorIs(t, err, apperrors.ErrNotFound, "read of another owner's record")

	_, err = svc.Update(other, UpdateInput{ID: created.ID, Name: "Hijacked", Status: domain.StatusArchived})
	assert.ErrorIs(t, err, apperrors.ErrNotFound, "update of another owner's record")

	assert.ErrorIs(t, svc.Delete(other, created.ID), apperrors.ErrNotFound, "delete of another owner's record")

	// The record is untouched and the owner still reaches it.
	got, err := svc.GetByID(ctxWithSubject(owner), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "Owner's", got.Name)
	assert.Equal(t, domain.StatusActive, got.Status)
}
