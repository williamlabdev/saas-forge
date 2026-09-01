package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/iam/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// IAMAdminService exposes role admin APIs with authorization in the service layer.
//
// Authorization scope: every method calls authz.Allow with an explicit action
// (iam:role:read|assign|revoke) before touching the repository — there is no
// handler-level "if admin" bypass. Under AUTHZ_MODE=rbac the built-in "admin"
// role is intentionally all-powerful for these role-admin operations (broad, by
// design, for the MVP template). For finer-grained scoping in production, run
// AUTHZ_MODE=opa and encode per-tenant / per-action limits in the Rego policies;
// this interface does not change. See docs/CODE_REVIEW_BASELINE.md item 6.
type IAMAdminService interface {
	ListRolesForUser(ctx context.Context, targetUserID uuid.UUID) ([]string, error)
	AssignRoleByName(ctx context.Context, targetUserID uuid.UUID, roleName string) error
	RevokeRoleByName(ctx context.Context, targetUserID uuid.UUID, roleName string) error
}

type iamAdminService struct {
	repo  repository.IAMRepository
	authz authz.Authorizer
}

func NewIAMAdminService(repo repository.IAMRepository, auth authz.Authorizer) IAMAdminService {
	return &iamAdminService{repo: repo, authz: auth}
}

func (s *iamAdminService) authorize(ctx context.Context, action string, userID uuid.UUID) error {
	return s.authz.Allow(ctx, authz.Input{
		Action: action,
		Resource: authz.Resource{
			Type: "user",
			ID:   userID.String(),
		},
	})
}

func (s *iamAdminService) ListRolesForUser(ctx context.Context, targetUserID uuid.UUID) ([]string, error) {
	if err := s.authorize(ctx, authz.ActionIAMRoleRead, targetUserID); err != nil {
		return nil, err
	}
	return s.repo.RolesForUser(ctx, targetUserID)
}

func (s *iamAdminService) AssignRoleByName(ctx context.Context, targetUserID uuid.UUID, roleName string) error {
	if err := s.authorize(ctx, authz.ActionIAMRoleAssign, targetUserID); err != nil {
		return err
	}
	role, err := s.repo.RoleByName(ctx, roleName)
	if err != nil {
		return err
	}
	return s.repo.AssignRole(ctx, targetUserID, role.ID)
}

func (s *iamAdminService) RevokeRoleByName(ctx context.Context, targetUserID uuid.UUID, roleName string) error {
	if err := s.authorize(ctx, authz.ActionIAMRoleRevoke, targetUserID); err != nil {
		return err
	}
	role, err := s.repo.RoleByName(ctx, roleName)
	if err != nil {
		return err
	}
	return s.repo.RevokeRole(ctx, targetUserID, role.ID)
}
