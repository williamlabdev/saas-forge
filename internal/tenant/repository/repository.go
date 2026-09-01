package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// TenantRepository reads tenant/membership state and provisions the
// self-serve owner tenant inside a caller-owned transaction (D8).
type TenantRepository interface {
	// MembershipsForUser lists the user's memberships joined with tenants,
	// earliest membership first (default-tenant order, plan section 6).
	MembershipsForUser(ctx context.Context, userID uuid.UUID) ([]domain.UserMembership, error)
	// MembershipRole resolves the user's role in the tenant identified by slug.
	// Returns apperrors.ErrNotFound when no membership exists.
	MembershipRole(ctx context.Context, userID uuid.UUID, slug string) (string, error)
	// TenantBySlug returns apperrors.ErrNotFound when the slug is unknown.
	TenantBySlug(ctx context.Context, slug string) (domain.Tenant, error)
	// ProvisionOwnerTx creates a fresh tenant (opaque random slug, D10) plus an
	// owner membership for userID, inside the caller's transaction — same shape
	// as auth's InsertCredentialsTx so user registration stays atomic (D8).
	ProvisionOwnerTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (slug string, err error)
	// CreateInvite stores a pending invite (PR-invite). The raw token is never
	// stored — only its hash arrives here.
	CreateInvite(ctx context.Context, inv domain.Invite) error
	// AcceptInvite atomically consumes the invite identified by tokenHash for
	// userID: validates expiry/single-use/email binding (against the user's
	// stored email blind index) under a row lock, inserts the membership, and
	// marks the invite accepted. Returns the domain.ErrInvite* / ErrAlreadyMember
	// sentinels on rejection; nothing is consumed on failure.
	AcceptInvite(ctx context.Context, tokenHash string, userID uuid.UUID) (domain.AcceptedInvite, error)
	// PlanForTenant resolves the metering plan for the tenant identified by
	// slug (TKT-R4b). An unknown slug degrades to the default (free) plan
	// rather than erroring — fail-safe to the tightest tier.
	PlanForTenant(ctx context.Context, slug string) (domain.Plan, error)
	// SetTenantPlan points a tenant at a plan (TKT-R4b PR3, platform admin).
	// Returns apperrors.ErrNotFound for an unknown slug and
	// domain.ErrPlanUnknown for a plan name not in the plans table.
	SetTenantPlan(ctx context.Context, slug, plan string) error
}
