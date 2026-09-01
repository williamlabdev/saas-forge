package platform

import (
	"context"

	contentservice "github.com/williamlabdev/saas-forge/internal/cms/content/service"
	tenantrepo "github.com/williamlabdev/saas-forge/internal/tenant/repository"
)

// contentPlanResolver adapts the tenant repository's PlanForTenant into the
// content service's PlanResolver (TKT-R4b), converting a tenant.Plan into the
// content Quota. Lives here — the composition layer — so the content and
// tenant modules stay decoupled. Shared by both composition roots (cmd/server
// providers + BuildApp).
func contentPlanResolver(tr *tenantrepo.PostgresTenantRepository) contentservice.PlanFunc {
	return func(ctx context.Context, slug string) (contentservice.Quota, error) {
		p, err := tr.PlanForTenant(ctx, slug)
		if err != nil {
			return contentservice.Quota{}, err
		}
		return contentservice.Quota{
			PlanName:            p.Name,
			MaxTypesPerTenant:   p.MaxTypes,
			MaxEntriesPerTenant: p.MaxEntries,
			MaxFieldsPerType:    p.MaxFieldsPerType,
			MaxEntryBytes:       p.MaxEntryBytes,
			SoftThresholdPct:    p.SoftThresholdPct,
		}, nil
	}
}

// ContentPlanResolver is the exported form for the cmd/server wire providers.
func ContentPlanResolver(tr *tenantrepo.PostgresTenantRepository) contentservice.PlanResolver {
	return contentPlanResolver(tr)
}
