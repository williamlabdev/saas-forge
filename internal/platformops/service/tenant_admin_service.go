package service

import (
	"context"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// TenantPlanWriter is the tenant-side capability this service needs — defined
// here (consumer) so platformops doesn't depend on the tenant repository's
// concrete type. The tenant repository implements it.
type TenantPlanWriter interface {
	SetTenantPlan(ctx context.Context, slug, plan string) error
}

// TenantAdminService is the platform-operator surface for tenant billing
// administration (TKT-R4b PR3 / D7): a platform admin changes a tenant's plan.
// Not a tenant-plane action — it is gated by the platform authorizer
// (is_admin), like the platform_apps console.
type TenantAdminService interface {
	SetPlan(ctx context.Context, slug, plan string) (TenantPlanDTO, error)
}

// TenantPlanDTO echoes the applied binding.
type TenantPlanDTO struct {
	TenantID string `json:"tenant_id"`
	Plan     string `json:"plan"`
}

type tenantAdminService struct {
	tenants TenantPlanWriter
	authz   authz.Authorizer
}

func NewTenantAdminService(tenants TenantPlanWriter, authorizer authz.Authorizer) TenantAdminService {
	return &tenantAdminService{tenants: tenants, authz: authorizer}
}

func (s *tenantAdminService) SetPlan(ctx context.Context, slug, plan string) (TenantPlanDTO, error) {
	if _, ok := authn.SubjectFromContext(ctx); !ok {
		return TenantPlanDTO{}, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionPlatformTenantPlanSet,
		Resource: authz.Resource{Type: "tenant", ID: slug},
	}); err != nil {
		return TenantPlanDTO{}, err
	}
	if slug == "" || plan == "" {
		return TenantPlanDTO{}, apperrors.New("VALIDATION_FAILED", "tenant and plan are required", 400)
	}
	if err := s.tenants.SetTenantPlan(ctx, slug, plan); err != nil {
		return TenantPlanDTO{}, err
	}
	return TenantPlanDTO{TenantID: slug, Plan: plan}, nil
}
