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

	// --- 2.2c: member management (ADR-022) ------------------------------

	// Members lists a tenant's roster ordered by membership creation
	// (earliest first), capped at memberListCap (no pagination convention
	// exists elsewhere in this repository yet — see ADR-022). Email is
	// resolved by the caller (service layer) via a narrow cross-module
	// interface; this repository stays free of a dependency on the user
	// module, per this repo's module-boundary rule.
	Members(ctx context.Context, tenantID uuid.UUID) ([]domain.Member, error)
	// UpdateMemberRole atomically changes a member's role, protecting the
	// tenant's last owner: demoting the sole remaining owner is refused with
	// domain.ErrLastOwner under a row lock (COUNT over a locked subquery,
	// same shape as elsewhere in this codebase — FOR UPDATE cannot combine
	// directly with an aggregate), so two concurrent demotions cannot both
	// "succeed". Returns apperrors.ErrNotFound if targetUserID has no
	// membership in tenantID. Caller-side authorization (who may call this
	// at all, self-operation, admin-cannot-touch-owner) lives in the tenant
	// service — this method trusts its caller on those questions and
	// enforces only the invariant a race could otherwise defeat.
	UpdateMemberRole(ctx context.Context, tenantID, targetUserID uuid.UUID, newRole string) (domain.Member, error)
	// RemoveMember atomically evicts a member, with the same last-owner
	// protection and error as UpdateMemberRole.
	RemoveMember(ctx context.Context, tenantID, targetUserID uuid.UUID) error
	// ListInvites returns a tenant's PENDING invites (neither accepted nor
	// revoked), most recent first, capped at memberListCap. The raw token
	// never appears on this or any other read path — only its hash is ever
	// persisted.
	ListInvites(ctx context.Context, tenantID uuid.UUID) ([]domain.Invite, error)
	// RevokeInvite marks a pending invite cancelled. Returns
	// domain.ErrInviteNotFound for an unknown id (scoped to tenantID) and
	// domain.ErrInviteUsed if it was already accepted. Revoking an
	// already-revoked invite is a no-op success — idempotent, so a second
	// click of "revoke" in the console does not surface an error.
	RevokeInvite(ctx context.Context, tenantID, inviteID, revokedBy uuid.UUID) error
}
