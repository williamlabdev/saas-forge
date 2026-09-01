package authz

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

func TestRBACAuthorizer_SelfRead(t *testing.T) {
	id := uuid.New()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: id, Roles: []string{"member"}})
	err := NewRBACAuthorizer().Allow(ctx, Input{
		Action:   ActionUserRead,
		Resource: Resource{Type: "user", ID: id.String()},
	})
	require.NoError(t, err)
}

func TestRBACAuthorizer_AdminDelete(t *testing.T) {
	id := uuid.New()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New(), Roles: []string{"admin"}})
	err := NewRBACAuthorizer().Allow(ctx, Input{
		Action:   ActionUserDelete,
		Resource: Resource{Type: "user", ID: id.String()},
	})
	require.NoError(t, err)
}

func TestRBACAuthorizer_MemberCannotDelete(t *testing.T) {
	id := uuid.New()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: id, Roles: []string{"member"}})
	err := NewRBACAuthorizer().Allow(ctx, Input{
		Action:   ActionUserDelete,
		Resource: Resource{Type: "user", ID: id.String()},
	})
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrForbidden.Code, ae.Code)
}

func TestRBACAuthorizer_NoSubject(t *testing.T) {
	err := NewRBACAuthorizer().Allow(context.Background(), Input{
		Action:   ActionUserRead,
		Resource: Resource{Type: "user", ID: uuid.New().String()},
	})
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)
}

func TestRBACAuthorizer_ContentByTenantRole(t *testing.T) {
	cases := []struct {
		name       string
		tenantRole string
		action     string
		wantAllow  bool
	}{
		{"owner writes", "owner", ActionContentCreate, true},
		{"tenant admin deletes", "admin", ActionContentDelete, true},
		{"editor updates", "editor", ActionContentUpdate, true},
		{"viewer reads", "viewer", ActionContentRead, true},
		{"viewer lists", "viewer", ActionContentList, true},
		{"viewer cannot write", "viewer", ActionContentCreate, false},
		{"no membership rejected", "", ActionContentRead, false},
		{"unknown role rejected", "superuser", ActionContentRead, false},
		// Destructive schema verbs split away from the content verbs: an editor
		// keeps every content capability but cannot drop a collection.
		{"owner writes schema", "owner", ActionContentSchemaWrite, true},
		{"tenant admin writes schema", "admin", ActionContentSchemaWrite, true},
		{"editor cannot write schema", "editor", ActionContentSchemaWrite, false},
		{"viewer cannot write schema", "viewer", ActionContentSchemaWrite, false},
		{"editor keeps ordinary content verbs", "editor", ActionContentDelete, true},
		// Planning is its own verb (ADR-013 §3) and carries the CONTENT roles as
		// of 2026-08-30 (補裁 T) — it used to carry the write verb's owner/admin
		// pair. The editor line below is the one that flipped, and it is the
		// load-bearing one in the new direction too: it is what proves the verb
		// really reaches the switch rather than being caught by a leftover
		// branch above it.
		//
		// The VIEWER line is now what carries the old line's job. Widening to
		// editor and widening to "everyone with a membership" are one edit apart
		// — dropping the branch entirely would do both — and only this line can
		// tell the two apart.
		{"owner plans schema", "owner", ActionContentSchemaPlan, true},
		{"tenant admin plans schema", "admin", ActionContentSchemaPlan, true},
		{"editor plans schema", "editor", ActionContentSchemaPlan, true},
		{"viewer cannot plan schema", "viewer", ActionContentSchemaPlan, false},
		{"no membership cannot plan schema", "", ActionContentSchemaPlan, false},
		// Proposing carries the same roles again (ADR-013 §3 step 8, 補裁 T).
		// Plan and propose were opened in ONE edit on purpose — a proposer who
		// cannot read a plan cannot know what the proposal says — so the two
		// editor lines are a pair: either of them alone going red means the
		// verbs have drifted apart.
		{"owner proposes schema", "owner", ActionContentSchemaPropose, true},
		{"tenant admin proposes schema", "admin", ActionContentSchemaPropose, true},
		{"editor proposes schema", "editor", ActionContentSchemaPropose, true},
		{"viewer cannot propose schema", "viewer", ActionContentSchemaPropose, false},
		{"no membership cannot propose schema", "", ActionContentSchemaPropose, false},
		// Additive schema edits carry the CONTENT roles, not the owner/admin pair
		// — splitting them off content:update must not have cost an editor
		// anything (ADR-013 §B). The editor pair below is the whole point: the
		// same role that may amend still may not write destructively.
		{"owner amends schema", "owner", ActionContentSchemaAmend, true},
		{"tenant admin amends schema", "admin", ActionContentSchemaAmend, true},
		{"editor amends schema", "editor", ActionContentSchemaAmend, true},
		{"editor still cannot write schema", "editor", ActionContentSchemaWrite, false},
		{"viewer cannot amend schema", "viewer", ActionContentSchemaAmend, false},
		{"no membership cannot amend schema", "", ActionContentSchemaAmend, false},
		// Publishing carries the CONTENT roles, exactly as before the verb
		// existed (ruled 2026-08-06: whoever could update can publish). The
		// editor row is the one that would catch a split done by narrowing
		// people's access instead of by refusing agents.
		{"owner publishes", "owner", ActionContentPublish, true},
		{"tenant admin publishes", "admin", ActionContentPublish, true},
		{"editor publishes", "editor", ActionContentPublish, true},
		{"viewer cannot publish", "viewer", ActionContentPublish, false},
		{"no membership cannot publish", "", ActionContentPublish, false},
		// The activity stream and the release queue (ADR-014 §3/§2), ruled
		// 2026-08-06. THE VIEWER ROWS ARE THE RULING: both verbs shipped riding
		// content:list, which viewer holds, so the stream and the queue were
		// readable by every role in the tenant. Everything else here is a
		// no-change assertion — an editor losing either is how a narrowing done
		// by taking access away from PEOPLE would first show up.
		{"owner reads the activity stream", "owner", ActionContentActivityRead, true},
		{"tenant admin reads the activity stream", "admin", ActionContentActivityRead, true},
		{"editor reads the activity stream", "editor", ActionContentActivityRead, true},
		{"viewer cannot read the activity stream", "viewer", ActionContentActivityRead, false},
		{"no membership cannot read the activity stream", "", ActionContentActivityRead, false},
		{"owner reads the release queue", "owner", ActionContentReviewList, true},
		{"tenant admin reads the release queue", "admin", ActionContentReviewList, true},
		{"editor reads the release queue", "editor", ActionContentReviewList, true},
		{"viewer cannot read the release queue", "viewer", ActionContentReviewList, false},
		{"no membership cannot read the release queue", "", ActionContentReviewList, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := authn.WithSubject(context.Background(), authn.Subject{
				UserID: uuid.New(), TenantID: "t_x", TenantRole: tc.tenantRole,
			})
			err := NewRBACAuthorizer().Allow(ctx, Input{
				Action:   tc.action,
				Resource: Resource{Type: "content", ID: "collection"},
			})
			if tc.wantAllow {
				require.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, apperrors.ErrForbidden)
			}
		})
	}
}

// F1 regression: the tenant plane must never satisfy platform-plane checks.
// A tenant "admin" (membership role) is not a platform admin.
func TestRBACAuthorizer_TenantAdminIsNotPlatformAdmin(t *testing.T) {
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t_x", TenantRole: "admin",
	})
	err := NewRBACAuthorizer().Allow(ctx, Input{
		Action:   ActionPlatformAppCreate,
		Resource: Resource{Type: "platform_app", ID: "x"},
	})
	assert.ErrorIs(t, err, apperrors.ErrForbidden)

	// And the platform user:delete admin bypass stays closed too.
	err = NewRBACAuthorizer().Allow(ctx, Input{
		Action:   ActionUserDelete,
		Resource: Resource{Type: "user", ID: uuid.NewString()},
	})
	assert.ErrorIs(t, err, apperrors.ErrForbidden)
}

func TestRBACAuthorizer_InviteActions(t *testing.T) {
	allow := func(role, activeTenant, targetTenant, action string) error {
		ctx := authn.WithSubject(context.Background(), authn.Subject{
			UserID: uuid.New(), TenantID: activeTenant, TenantRole: role,
		})
		return NewRBACAuthorizer().Allow(ctx, Input{
			Action: action, Resource: Resource{Type: "tenant", ID: targetTenant},
		})
	}

	require.NoError(t, allow("owner", "t_x", "t_x", ActionTenantInviteCreate))
	require.NoError(t, allow("admin", "t_x", "t_x", ActionTenantInviteCreate))
	assert.ErrorIs(t, allow("editor", "t_x", "t_x", ActionTenantInviteCreate), apperrors.ErrForbidden)
	assert.ErrorIs(t, allow("viewer", "t_x", "t_x", ActionTenantInviteCreate), apperrors.ErrForbidden)
	// Active-tenant binding: owner of t_x cannot mint for t_y.
	assert.ErrorIs(t, allow("owner", "t_x", "t_y", ActionTenantInviteCreate), apperrors.ErrForbidden)
	// Accepting only requires an authenticated subject.
	require.NoError(t, allow("", "", "invite", ActionTenantInviteAccept))
}

// D12 parity in the RBAC facade: platform admin gets no blanket over the
// tenant plane (settled in PR4 together with the rego rules).
func TestRBACAuthorizer_PlatformAdminNoTenantPlaneBlanket(t *testing.T) {
	platformAdmin := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), Roles: []string{"admin"}, TenantID: "t_a",
	})
	err := NewRBACAuthorizer().Allow(platformAdmin, Input{
		Action: ActionContentCreate, Resource: Resource{Type: "content", ID: "collection"},
	})
	assert.ErrorIs(t, err, apperrors.ErrForbidden)
	err = NewRBACAuthorizer().Allow(platformAdmin, Input{
		Action: ActionTenantInviteCreate, Resource: Resource{Type: "tenant", ID: "t_a"},
	})
	assert.ErrorIs(t, err, apperrors.ErrForbidden)
	// Platform plane unaffected.
	require.NoError(t, NewRBACAuthorizer().Allow(platformAdmin, Input{
		Action: ActionPlatformAppCreate, Resource: Resource{Type: "platform_app", ID: "x"},
	}))
	// A platform admin who ALSO holds a membership uses that membership's role.
	adminWithViewer := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), Roles: []string{"admin"}, TenantID: "t_a", TenantRole: "viewer",
	})
	require.NoError(t, NewRBACAuthorizer().Allow(adminWithViewer, Input{
		Action: ActionContentRead, Resource: Resource{Type: "content", ID: "article"},
	}))
	assert.ErrorIs(t, NewRBACAuthorizer().Allow(adminWithViewer, Input{
		Action: ActionContentCreate, Resource: Resource{Type: "content", ID: "collection"},
	}), apperrors.ErrForbidden)
}

// Parity with rego: tenant-plane verbs without a rule fail CLOSED even for
// platform admin — the D12 blanket cannot resurrect via a new constant.
func TestRBACAuthorizer_UnknownTenantPlaneVerbFailsClosed(t *testing.T) {
	platformAdmin := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), Roles: []string{"admin"}, TenantID: "t_a", TenantRole: "owner",
	})
	// These are placeholders for "a verb nobody wrote a rule for", so they have
	// to be verbs that genuinely do not exist. content:publish stood here until
	// 2026-08-06, when ADR-014 §1 made it real — a sample that becomes a
	// shipped feature stops testing fail-closed and starts testing the feature,
	// which is why it was swapped rather than deleted. If either of these is
	// ever implemented, swap it too; do not drop the case.
	for _, action := range []string{"content:archive", "tenant:invite:revoke"} {
		err := NewRBACAuthorizer().Allow(platformAdmin, Input{
			Action: action, Resource: Resource{Type: "content", ID: "x"},
		})
		assert.ErrorIs(t, err, apperrors.ErrForbidden, action)
	}
	// Empty active tenant denies content even with a (stale/forged) role.
	noTenant := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantRole: "editor",
	})
	assert.ErrorIs(t, NewRBACAuthorizer().Allow(noTenant, Input{
		Action: ActionContentCreate, Resource: Resource{Type: "content", ID: "collection"},
	}), apperrors.ErrForbidden)
}

// ADR-013 agent-credential lifecycle, ruled 2026-08-06: owner/admin of the
// active tenant hold all three verbs, and the content-write roles do not.
func TestRBACAuthorizer_AgentCredentialActions(t *testing.T) {
	allow := func(role, activeTenant, action string) error {
		ctx := authn.WithSubject(context.Background(), authn.Subject{
			UserID: uuid.New(), TenantID: activeTenant, TenantRole: role,
		})
		return NewRBACAuthorizer().Allow(ctx, Input{
			Action: action, Resource: Resource{Type: "agent_credential", ID: "cred-1"},
		})
	}

	for _, action := range []string{
		ActionAgentCredentialIssue, ActionAgentCredentialList, ActionAgentCredentialRevoke,
	} {
		t.Run(action, func(t *testing.T) {
			require.NoError(t, allow("owner", "t_x", action))
			require.NoError(t, allow("admin", "t_x", action))
			// An editor may write every entry an agent could and still may not
			// decide that something unattended gets to.
			assert.ErrorIs(t, allow("editor", "t_x", action), apperrors.ErrForbidden)
			assert.ErrorIs(t, allow("viewer", "t_x", action), apperrors.ErrForbidden)
			// A role claim without an active tenant does not stand in for one.
			assert.ErrorIs(t, allow("owner", "", action), apperrors.ErrForbidden)
		})
	}

	// D12 over the new prefix, in the facade: platform admin gets no blanket.
	platformAdmin := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), Roles: []string{"admin"}, TenantID: "t_a",
	})
	assert.ErrorIs(t, NewRBACAuthorizer().Allow(platformAdmin, Input{
		Action: ActionAgentCredentialIssue, Resource: Resource{Type: "agent_credential", ID: ""},
	}), apperrors.ErrForbidden)

	// An agent:* verb with no rule fails CLOSED — the property the prefix buys.
	// If agent:credential:rotate is ever implemented, swap the sample rather
	// than deleting the case (see TestRBACAuthorizer_UnknownTenantPlaneVerbFailsClosed).
	assert.ErrorIs(t, allow("owner", "t_x", "agent:credential:rotate"), apperrors.ErrForbidden)
}
