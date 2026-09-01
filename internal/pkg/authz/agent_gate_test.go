package authz

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// agentSubject is a credential minted from an OWNER's login — the widest
// tenant role there is, plus the platform admin role. Testing with the widest
// possible minter is the point: everything refused below is refused because the
// bearer is an agent, not because the underlying role was too weak.
func agentSubject(t *testing.T) authn.Subject {
	t.Helper()
	principal := uuid.New()
	agentID := "content-bot"
	return authn.Subject{
		UserID:       principal,
		Roles:        []string{"admin"},
		TenantID:     "tenant-a",
		TenantRole:   "owner",
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		AllowedTypes: []string{"post"},
	}
}

// The same person WITHOUT the agent credential. Every refusal below is paired
// with this control: without it, an authorizer that refused everything would
// look like a correctly enforced enumeration.
func humanSubject(sub authn.Subject) authn.Subject {
	human := sub
	human.Kind = ""
	human.AgentID = nil
	human.PrincipalID = nil
	human.AllowedTypes = nil
	return human
}

// The expected answers are written out here, hard-coded, rather than derived
// from refuseUnlistedAgentAction's switch — a test that asks the code under
// test what it permits proves only that it agrees with itself.
//
// ADR-013: §5's tools need list/read/create/update, §3 gives agents plan but
// never write/apply, §B keeps amend away from entry writers, and §5 declines
// delete outright.
var agentActionExpectations = map[string]bool{
	ActionContentList:       true,
	ActionContentRead:       true,
	ActionContentCreate:     true,
	ActionContentUpdate:     true,
	ActionContentSchemaPlan: true,
	// §3 step 8: an agent may FILE a schema change for a person to approve. The
	// pair with schema:write below is the whole design — proposing is how an
	// agent asks, applying is how a person answers, and an agent that held both
	// would be approving its own work with an audit trail attached.
	ActionContentSchemaPropose: true,
	ActionContentDelete:        false,
	// ADR-014 §1: the gate. Writing the working copy is content:update, above;
	// releasing it is this one, and it belongs to a person. content:update
	// staying true is what keeps RETRACT available to the agent — that is the
	// stop-the-bleeding action, and refusing it would mean bad content stays up
	// until someone logs in.
	ActionContentPublish: false,
	// ADR-014 §2/§3, ruled 2026-08-06. An agent is refused these at the CMS
	// chokepoint too, by §4's untyped rule, and that layer fires FIRST — which
	// is exactly why these two lines need a white-box test rather than a service
	// one. A black-box call cannot distinguish the layer that answered, so the
	// second layer could be deleted with every end-to-end test still green.
	//
	// Single-entry attribution is deliberately NOT here: it rides the ordinary
	// content read verbs, because "who wrote this entry" travels with the entry
	// and "what happened in this tenant" does not.
	ActionContentActivityRead: false,
	ActionContentReviewList:   false,
	ActionContentSchemaWrite:  false,
	ActionContentSchemaAmend:  false,
	ActionTenantInviteCreate:  false,
	ActionTenantInviteAccept:  false,
	// An agent may not mint, list or revoke agent credentials. Laundering one
	// credential into another with a wider whitelist is the failure IssueAgentToken
	// already refuses at the signer (`minter.Kind != ""`), so this is the SECOND
	// layer — but it is a reachable one, not decoration: the HTTP endpoint asks
	// the authorizer first and never reaches the signer, so without these three
	// lines the refusal would arrive as a 500-shaped ErrNotMintable instead of a
	// 403, and `agent:credential:list` — which does not touch the signer at all —
	// would simply be ALLOWED.
	ActionAgentCredentialIssue:  false,
	ActionAgentCredentialList:   false,
	ActionAgentCredentialRevoke: false,
	ActionUserList:              false,
	ActionUserRead:              false,
	ActionUserUpdate:            false,
	ActionUserDelete:            false,
	ActionUserPreferencesUpdate: false,
	ActionIAMRoleRead:           false,
	ActionIAMRoleAssign:         false,
	ActionIAMRoleRevoke:         false,
	ActionNotificationRead:      false,
	ActionNotificationCreate:    false,
	ActionPlatformAppList:       false,
	ActionPlatformAppCreate:     false,
	ActionPlatformAppUpdate:     false,
	ActionPlatformTenantPlanSet: false,
	"content:something:new":     false,
	"billing:invoice:read":      false,
}

// ADR-013 §1's third invariant, at the layer that can enforce it for EVERY
// plane. An agent token is a downgraded tenant credential — it carries the
// minter's TenantRole — so an enumeration living only in the CMS service would
// leave the same token acting as an ordinary member at tenant:, user:, iam: and
// the rest (ruled 2026-08-05).
func TestAgentCredentialIsRefusedUnlistedActions(t *testing.T) {
	agent := agentSubject(t)
	agentCtx := authn.WithSubject(context.Background(), agent)

	for action, allowed := range agentActionExpectations {
		if allowed {
			continue
		}
		t.Run(action, func(t *testing.T) {
			err := refuseUnlistedAgentAction(agent, action)
			require.ErrorIs(t, err, apperrors.ErrForbidden)

			// Through the real authorizers, not only the helper: the gate is
			// worth nothing if a call site forgets it.
			rbac := NewRBACAuthorizer()
			require.ErrorIs(t, rbac.Allow(agentCtx, Input{Action: action, Resource: Resource{Type: "content", ID: uuid.NewString()}}), apperrors.ErrForbidden)

			opa, err := NewOPAAuthorizer(nil)
			require.NoError(t, err)
			require.ErrorIs(t, opa.Allow(agentCtx, Input{Action: action, Resource: Resource{Type: "content", ID: uuid.NewString()}}), apperrors.ErrForbidden)
		})
	}
}

// The other half. Without it the refusals above are satisfied by a gate that
// blocks everything, which would also be a broken agent interface.
func TestAgentCredentialKeepsItsEnumeratedActions(t *testing.T) {
	agent := agentSubject(t)
	agentCtx := authn.WithSubject(context.Background(), agent)

	for action, allowed := range agentActionExpectations {
		if !allowed {
			continue
		}
		t.Run(action, func(t *testing.T) {
			require.NoError(t, refuseUnlistedAgentAction(agent, action))

			rbac := NewRBACAuthorizer()
			require.NoError(t, rbac.Allow(agentCtx, Input{Action: action, Resource: Resource{Type: "content", ID: "collection"}}))

			opa, err := NewOPAAuthorizer(nil)
			require.NoError(t, err)
			require.NoError(t, opa.Allow(agentCtx, Input{Action: action, Resource: Resource{Type: "content", ID: "collection"}}))
		})
	}
}

// The control that makes the refusals mean something: the SAME role, without
// the agent kind, is allowed the very actions the agent was refused.
func TestTheSameRoleWithoutTheAgentKindIsUnaffected(t *testing.T) {
	human := humanSubject(agentSubject(t))
	humanCtx := authn.WithSubject(context.Background(), human)
	rbac := NewRBACAuthorizer()

	require.NoError(t, rbac.Allow(humanCtx, Input{Action: ActionContentDelete, Resource: Resource{Type: "content", ID: uuid.NewString()}}),
		"an owner may delete content — the agent's refusal must come from the credential, not the role")
	require.NoError(t, rbac.Allow(humanCtx, Input{Action: ActionContentSchemaWrite, Resource: Resource{Type: "content", ID: "collection"}}))
	require.NoError(t, rbac.Allow(humanCtx, Input{Action: ActionTenantInviteCreate, Resource: Resource{Type: "tenant", ID: "tenant-a"}}))
	require.NoError(t, rbac.Allow(humanCtx, Input{Action: ActionAgentCredentialIssue, Resource: Resource{Type: "agent_credential", ID: ""}}),
		"the owner who minted the agent may mint another — the agent's refusal is about the bearer, not the role")
	require.NoError(t, rbac.Allow(humanCtx, Input{Action: ActionUserDelete, Resource: Resource{Type: "user", ID: uuid.NewString()}}),
		"the platform admin role still works for the person")
}

// The gate sits ABOVE the platform-admin bypass on purpose. A minter who is a
// platform admin must not mint an agent that inherits the bypass — and the
// bypass is the branch that returns nil for every non-tenant-plane action.
func TestAgentGatePrecedesThePlatformAdminBypass(t *testing.T) {
	agent := agentSubject(t)
	require.Contains(t, agent.Roles, "admin", "the fixture must actually carry the role this test is about")

	ctx := authn.WithSubject(context.Background(), agent)
	rbac := NewRBACAuthorizer()
	require.ErrorIs(t, rbac.Allow(ctx, Input{Action: ActionUserDelete, Resource: Resource{Type: "user", ID: uuid.NewString()}}), apperrors.ErrForbidden)
	require.ErrorIs(t, rbac.Allow(ctx, Input{Action: ActionPlatformAppCreate, Resource: Resource{Type: "platform", ID: "app"}}), apperrors.ErrForbidden)
}
