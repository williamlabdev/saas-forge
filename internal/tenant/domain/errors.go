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
	// ErrInviteRevoked rejects acceptance of an invite an owner/admin cancelled
	// before it was used (2.2c). A DISTINCT code from ErrInviteInviterRevoked:
	// that one fires because the INVITER lost their privileges (an incidental,
	// system-detected consequence); this one fires because a still-privileged
	// owner/admin made a deliberate choice to cancel THIS invite. Conflating
	// them would make the accept-side error message lie about which happened,
	// and a future audit trail could not tell "revoked by policy drift" from
	// "revoked on purpose" without a second signal.
	ErrInviteRevoked = apperrors.New("INVITE_REVOKED", "invite was revoked by the inviting tenant", 410)
)

// Member management errors (2.2c: list/update-role/remove).
var (
	// ErrLastOwner protects a tenant from ever reaching zero owners — a
	// tenant with no owner has no one who can transfer ownership, promote a
	// new owner, or otherwise recover, so both demoting AND removing the last
	// owner are refused. Enforced by an atomic, row-locked COUNT in the
	// repository (see PostgresTenantRepository.UpdateMemberRole/RemoveMember)
	// so two concurrent requests cannot both "successfully" strip the last
	// owner in the gap between a read and a write.
	ErrLastOwner = apperrors.New("TENANT_LAST_OWNER", "cannot demote or remove the tenant's last owner", 409)
	// ErrSelfRoleChange refuses PATCH …/members/{self}. 409, not 403: the
	// caller IS authorized to change member roles in general (they hold
	// owner/admin) — what is refused is the specific TARGET, to close a
	// self-lockout / self-escalation path (an admin demoting themself to
	// viewer mid-incident, or "promoting" themself to owner without the
	// transfer flow's explicit intent). Leaving the tenant is a distinct,
	// out-of-scope feature (see handler doc).
	ErrSelfRoleChange = apperrors.New("TENANT_SELF_ROLE_CHANGE", "cannot change your own role", 409)
	// ErrSelfRemove refuses DELETE …/members/{self}, same 409-not-403
	// reasoning as ErrSelfRoleChange: the caller CAN remove members, just not
	// themself through this endpoint. "Leave this tenant" is a genuinely
	// different feature (no actor left to authorize removing YOU) and is out
	// of scope here.
	ErrSelfRemove = apperrors.New("TENANT_SELF_REMOVE", "cannot remove yourself from the tenant; leaving a tenant is not yet supported", 409)
	// ErrCannotAssignOwner refuses an ADMIN (not owner) from touching the
	// owner role on either side of a member-role change: assigning it to
	// someone else, or changing an existing owner away from it. Ownership
	// moves only through an OWNER acting on this same endpoint (there is no
	// separate "transfer" endpoint — see ADR-022). 403, unlike the two
	// self-operation errors above: this is a genuine authorization refusal,
	// not a policy 409 — the admin was never allowed to do this, regardless
	// of which member they targeted.
	ErrCannotAssignOwner = apperrors.New("TENANT_CANNOT_ASSIGN_OWNER", "only an owner may assign or change the owner role", 403)
	// ErrRoleInvalid rejects an UpdateMemberRole request whose requested role
	// is not one of the four known roles. 422 (semantically invalid request
	// body), not 400 (malformed JSON) — the JSON decodes fine, the value is
	// just not a role this system knows about.
	ErrRoleInvalid = apperrors.New("TENANT_ROLE_INVALID", "role must be one of owner, admin, editor, viewer", 422)
)
