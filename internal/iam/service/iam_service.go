package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/iam/repository"
)

// IAMService manages role assignments (facts only).
type IAMService interface {
	AssignRoleByName(ctx context.Context, userID uuid.UUID, roleName string) error
	RevokeRoleByName(ctx context.Context, userID uuid.UUID, roleName string) error
	RolesForUser(ctx context.Context, userID uuid.UUID) ([]string, error)
}

type iamService struct {
	repo repository.IAMRepository
}

func NewIAMService(repo repository.IAMRepository) IAMService {
	return &iamService{repo: repo}
}

func (s *iamService) AssignRoleByName(ctx context.Context, userID uuid.UUID, roleName string) error {
	role, err := s.repo.RoleByName(ctx, roleName)
	if err != nil {
		return err
	}
	return s.repo.AssignRole(ctx, userID, role.ID)
}

func (s *iamService) RevokeRoleByName(ctx context.Context, userID uuid.UUID, roleName string) error {
	role, err := s.repo.RoleByName(ctx, roleName)
	if err != nil {
		return err
	}
	return s.repo.RevokeRole(ctx, userID, role.ID)
}

func (s *iamService) RolesForUser(ctx context.Context, userID uuid.UUID) ([]string, error) {
	return s.repo.RolesForUser(ctx, userID)
}

// FactsLoader adapts IAMService to authz.RoleFactsLoader.
type FactsLoader struct {
	Svc IAMService
}

func (f FactsLoader) RolesForUser(ctx context.Context, userID uuid.UUID) ([]string, error) {
	return f.Svc.RolesForUser(ctx, userID)
}
