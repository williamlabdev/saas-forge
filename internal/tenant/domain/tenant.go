package domain

import (
	"time"

	"github.com/google/uuid"
)

// Tenant-scoped roles (D3). Issuance validates against this set; the
// memberships table enforces it again with a CHECK constraint.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

func ValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleEditor, RoleViewer:
		return true
	}
	return false
}

type Tenant struct {
	ID        uuid.UUID
	Slug      string
	Name      string
	CreatedAt time.Time
}

// UserMembership is one row of "which tenants can this user act in":
// membership joined with its tenant, ordered by membership creation
// (earliest first — the default-tenant semantics in plan section 6).
type UserMembership struct {
	TenantID  uuid.UUID
	Slug      string
	Name      string
	Role      string
	CreatedAt time.Time
}

// InvitableRole reports whether a role may be granted through an invite:
// the D3 set minus owner (ownership moves only via an explicit transfer flow).
func InvitableRole(role string) bool {
	switch role {
	case RoleAdmin, RoleEditor, RoleViewer:
		return true
	}
	return false
}

// mintableAgentRoles maps a MINTER'S tenant role to the roles it may give an
// agent credential (ADR-013 補裁 S-1, ruled 2026-08-20).
//
// WHY A TABLE AND NOT AN ORDERING. The obvious implementation is rank(owner) >
// rank(admin) > rank(editor) > rank(viewer) and a `>` comparison. That was
// refused deliberately: a general rank() would be reachable from every other
// authorization decision in the repo, and each one that used it would be
// betting that "greater than" means the same thing there as it does here. The
// question this table answers is only "who may hand out what", and it answers
// it without asserting an ordering the rest of the code can borrow.
//
// WHY THIS IS NOT InvitableRole, AND WHY THEY MUST NOT BE MERGED. InvitableRole
// looks like the same shape — a subset of the four roles — but it answers a
// question about the INVITEE of a human membership, and its content is "the D3
// set minus owner" for a reason specific to ownership transfer. This table
// answers a question about a NON-HUMAN credential and is keyed by the minter.
// Nothing requires the two to stay equal, and folding them together would make
// a future change to either one silently change the other.
//
// A MINTER MAY NOT MINT ITS OWN ROLE. owner is absent from owner's set and
// admin from admin's, which is the whole content of the change: before this,
// every credential copied its minter's role, so every agent in existence
// carried owner or admin and own_only_roles naming only "editor" could not
// confine any agent at all.
var mintableAgentRoles = map[string][]string{
	RoleOwner: {RoleAdmin, RoleEditor, RoleViewer},
	RoleAdmin: {RoleEditor, RoleViewer},
}

// CanMintAgentRole reports whether a caller holding minterRole may mint an
// agent credential carrying target. An unknown role on either side is false:
// this is an authorization predicate, so "not in the table" must read as no.
func CanMintAgentRole(minterRole, target string) bool {
	for _, r := range mintableAgentRoles[minterRole] {
		if r == target {
			return true
		}
	}
	return false
}

// MintableAgentRoles returns the roles minterRole may mint, for callers that
// need to SHOW the set rather than check one member (the console's role
// picker). The copy is defensive: the table is package state and a caller that
// sorted or appended to the returned slice in place would edit the policy.
func MintableAgentRoles(minterRole string) []string {
	src := mintableAgentRoles[minterRole]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// Invite is a pending membership: a single-use token bound to an email
// (blind-index hash — no plaintext PII at rest) granting Role in TenantID.
type Invite struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	EmailLookupHash []byte
	Role            string
	TokenHash       string
	InvitedBy       uuid.UUID
	CreatedAt       time.Time
	ExpiresAt       time.Time
	AcceptedAt      *time.Time
	AcceptedBy      *uuid.UUID
}

// AcceptedInvite is what an accepted invite resolves to.
type AcceptedInvite struct {
	TenantSlug string
	TenantName string
	Role       string
}

// DefaultPlanName is the plan a tenant falls back to when none resolves
// (also the tenants.plan column default). See plan-metering plan D5.
const DefaultPlanName = "free"

// Plan is a metering tier (TKT-R4b): per-tenant content limits plus the soft
// threshold. Each Max* is 0 = unlimited for that dimension (D5), mirroring the
// R4a quota semantics.
type Plan struct {
	Name             string
	MaxTypes         int
	MaxEntries       int
	MaxFieldsPerType int
	MaxEntryBytes    int
	SoftThresholdPct int
}
