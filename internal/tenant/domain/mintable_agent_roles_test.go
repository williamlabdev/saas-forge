package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-013 補裁 S-1 (ruled 2026-08-20). The table is the policy, so the test
// spells every cell out rather than deriving them: a test that computed the
// expected answer would be a second implementation and would agree with a
// wrong one.
func TestCanMintAgentRole(t *testing.T) {
	// Written as a full matrix over the four roles plus the two ways a role can
	// be absent, so that ADDING a role to the table without deciding this
	// question shows up here rather than silently defaulting to refuse.
	allow := map[string]map[string]bool{
		RoleOwner:  {RoleOwner: false, RoleAdmin: true, RoleEditor: true, RoleViewer: true},
		RoleAdmin:  {RoleOwner: false, RoleAdmin: false, RoleEditor: true, RoleViewer: true},
		RoleEditor: {RoleOwner: false, RoleAdmin: false, RoleEditor: false, RoleViewer: false},
		RoleViewer: {RoleOwner: false, RoleAdmin: false, RoleEditor: false, RoleViewer: false},
	}
	for minter, targets := range allow {
		for target, want := range targets {
			assert.Equalf(t, want, CanMintAgentRole(minter, target),
				"%s minting %s", minter, target)
		}
	}

	// Not in the table reads as no, on either side. This is an authorization
	// predicate: a typo must not be an escalation.
	assert.False(t, CanMintAgentRole("", RoleEditor))
	assert.False(t, CanMintAgentRole(RoleOwner, ""))
	assert.False(t, CanMintAgentRole("superuser", RoleEditor))
	assert.False(t, CanMintAgentRole(RoleOwner, "superuser"))
}

// The set exists so a UI can show the choices. If it disagreed with the
// predicate the console would offer an option the API refuses.
func TestMintableAgentRolesAgreesWithThePredicate(t *testing.T) {
	for _, minter := range []string{RoleOwner, RoleAdmin, RoleEditor, RoleViewer, "superuser"} {
		for _, target := range []string{RoleOwner, RoleAdmin, RoleEditor, RoleViewer} {
			listed := false
			for _, r := range MintableAgentRoles(minter) {
				if r == target {
					listed = true
				}
			}
			assert.Equalf(t, CanMintAgentRole(minter, target), listed,
				"%s -> %s: the list and the check must not disagree", minter, target)
		}
	}

	// The returned slice is a copy: a caller that sorts or truncates it in
	// place must not be editing the policy for everyone else.
	got := MintableAgentRoles(RoleOwner)
	require.NotEmpty(t, got)
	got[0] = "clobbered"
	assert.Equal(t, RoleAdmin, MintableAgentRoles(RoleOwner)[0])
}

// InvitableRole and the minting table are deliberately separate (補裁 S-1
// landing note a). They agree on nothing structural, and this test says so out
// loud: an owner may be INVITED as admin and may MINT an admin agent, but the
// two answers come from different questions and the sets are not equal.
func TestMintingTableIsNotInvitableRole(t *testing.T) {
	// InvitableRole does not depend on who is asking; the minting table does.
	assert.True(t, InvitableRole(RoleAdmin))
	assert.False(t, CanMintAgentRole(RoleAdmin, RoleAdmin),
		"if these two ever agree by construction, one of them has absorbed the other")
}
