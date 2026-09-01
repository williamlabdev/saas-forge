package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/iam/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

func TestIAMAdminService_ListRoles_ForbiddenForMember(t *testing.T) {
	repo := &stubIAMRepo{roles: []string{"member"}}
	svc := NewIAMAdminService(repo, authz.NewRBACAuthorizer())

	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(),
		Roles:  []string{"member"},
	})
	_, err := svc.ListRolesForUser(ctx, uuid.New())
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrForbidden.Code, ae.Code)
}

func TestIAMAdminService_ListRoles_AdminAllowed(t *testing.T) {
	target := uuid.New()
	repo := &stubIAMRepo{roles: []string{"admin", "member"}}
	svc := NewIAMAdminService(repo, authz.NewRBACAuthorizer())

	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(),
		Roles:  []string{"admin"},
	})
	got, err := svc.ListRolesForUser(ctx, target)
	require.NoError(t, err)
	assert.Equal(t, []string{"admin", "member"}, got)
}

func TestIAMAdminService_AssignRole_ForbiddenForMember(t *testing.T) {
	repo := &stubIAMRepo{roles: []string{"member"}}
	svc := NewIAMAdminService(repo, authz.NewRBACAuthorizer())

	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(),
		Roles:  []string{"member"},
	})
	err := svc.AssignRoleByName(ctx, uuid.New(), "admin")
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrForbidden.Code, ae.Code)
	assert.False(t, repo.assignCalled, "authz must be checked before the repository is touched")
}

func TestIAMAdminService_RevokeRole_ForbiddenForMember(t *testing.T) {
	repo := &stubIAMRepo{roles: []string{"member"}}
	svc := NewIAMAdminService(repo, authz.NewRBACAuthorizer())

	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(),
		Roles:  []string{"member"},
	})
	err := svc.RevokeRoleByName(ctx, uuid.New(), "admin")
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrForbidden.Code, ae.Code)
	assert.False(t, repo.revokeCalled, "authz must be checked before the repository is touched")
}

func TestIAMAdminService_AssignRole_AllowedForAdmin(t *testing.T) {
	repo := &stubIAMRepo{roles: []string{"admin"}}
	svc := NewIAMAdminService(repo, authz.NewRBACAuthorizer())

	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(),
		Roles:  []string{"admin"},
	})
	require.NoError(t, svc.AssignRoleByName(ctx, uuid.New(), "member"))
	assert.True(t, repo.assignCalled)
}

type stubIAMRepo struct {
	roles        []string
	assignCalled bool
	revokeCalled bool
}

func (s *stubIAMRepo) RoleByName(context.Context, string) (*domain.Role, error) {
	return &domain.Role{ID: uuid.New(), Name: "admin"}, nil
}

func (s *stubIAMRepo) AssignRole(context.Context, uuid.UUID, uuid.UUID) error {
	s.assignCalled = true
	return nil
}

func (s *stubIAMRepo) RevokeRole(context.Context, uuid.UUID, uuid.UUID) error {
	s.revokeCalled = true
	return nil
}
func (s *stubIAMRepo) RolesForUser(context.Context, uuid.UUID) ([]string, error) {
	return s.roles, nil
}
