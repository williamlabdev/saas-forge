package domain

import apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"

// Invite acceptance errors. Distinct codes are deliberate: the token holder
// was invited, so telling them why acceptance failed is UX, not a leak —
// tokens are 256-bit random values, unguessable by construction.
var (
	ErrInviteNotFound      = apperrors.New("INVITE_NOT_FOUND", "invite not found", 404)
	ErrInviteExpired       = apperrors.New("INVITE_EXPIRED", "invite has expired", 410)
	ErrInviteUsed          = apperrors.New("INVITE_USED", "invite has already been used", 410)
	ErrInviteEmailMismatch = apperrors.New("INVITE_EMAIL_MISMATCH", "invite was issued for a different email address", 403)
	ErrAlreadyMember       = apperrors.New("TENANT_ALREADY_MEMBER", "already a member of this tenant", 409)
	// ErrPlanUnknown rejects setting a tenant to a plan not in the plans table.
	ErrPlanUnknown = apperrors.New("PLAN_UNKNOWN", "unknown plan", 422)
	// ErrInviteInviterRevoked kills pending invites whose creator lost their
	// owner/admin membership: without this, a demoted admin's residual JWT
	// (≤15 min, D6) could mint a 7-day escalation artifact. The check runs at
	// accept time, so demotion retroactively voids everything they minted.
	ErrInviteInviterRevoked = apperrors.New("INVITE_INVITER_REVOKED", "invite is no longer valid; the inviter's privileges were revoked", 410)
)
