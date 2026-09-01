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

func TestOPAAuthorizer_SelfRead(t *testing.T) {
	auth, err := NewOPAAuthorizer(nil)
	require.NoError(t, err)

	id := uuid.New()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: id, Roles: []string{"member"}})
	err = auth.Allow(ctx, Input{
		Action:   ActionUserRead,
		Resource: Resource{Type: "user", ID: id.String()},
	})
	require.NoError(t, err)
}

func TestOPAAuthorizer_AdminDeleteOther(t *testing.T) {
	auth, err := NewOPAAuthorizer(nil)
	require.NoError(t, err)

	target := uuid.New()
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(),
		Roles:  []string{"admin"},
	})
	err = auth.Allow(ctx, Input{
		Action:   ActionUserDelete,
		Resource: Resource{Type: "user", ID: target.String()},
	})
	require.NoError(t, err)
}

func TestOPAAuthorizer_MemberCannotDelete(t *testing.T) {
	auth, err := NewOPAAuthorizer(nil)
	require.NoError(t, err)

	id := uuid.New()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: id, Roles: []string{"member"}})
	err = auth.Allow(ctx, Input{
		Action:   ActionUserDelete,
		Resource: Resource{Type: "user", ID: id.String()},
	})
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrForbidden.Code, ae.Code)
}

func TestOPAAuthorizer_IAMFactsMerged(t *testing.T) {
	id := uuid.New()
	auth, err := NewOPAAuthorizer(stubFacts{roles: map[uuid.UUID][]string{
		id: {"admin"},
	}})
	require.NoError(t, err)

	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: id, Roles: []string{"member"}})
	err = auth.Allow(ctx, Input{
		Action:   ActionUserDelete,
		Resource: Resource{Type: "user", ID: uuid.New().String()},
	})
	require.NoError(t, err)
}

type stubFacts struct {
	roles map[uuid.UUID][]string
}

func (s stubFacts) RolesForUser(_ context.Context, userID uuid.UUID) ([]string, error) {
	return s.roles[userID], nil
}

// Plan §7 #4/#5, settled D12: the two planes never cross in OPA mode.
func TestOPAAuthorizer_TenantPlane(t *testing.T) {
	auth, err := NewOPAAuthorizer(nil)
	require.NoError(t, err)

	editor := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t_a", TenantRole: "editor",
	})
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentCreate, Resource: Resource{Type: "content", ID: "collection"},
	}))

	viewer := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t_a", TenantRole: "viewer",
	})
	require.NoError(t, auth.Allow(viewer, Input{
		Action: ActionContentRead, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentCreate, Resource: Resource{Type: "content", ID: "collection"},
	}))

	// The schema split, exercised through the real policy rather than only the
	// hand-written matcher: an editor keeps every content verb and loses the
	// destructive schema one. This is the assertion that would go green by
	// accident if content:schema:write were ever added to content_actions.
	require.Error(t, auth.Allow(editor, Input{
		Action: ActionContentSchemaWrite, Resource: Resource{Type: "content", ID: "article"},
	}))

	// The plan verb through the real policy too. It carries the CONTENT roles as
	// of 2026-08-30 (ADR-013 補裁 T), so the editor line asserts the opposite of
	// what it used to — and the assertion it protects moved to the viewer line
	// below rather than disappearing.
	//
	// WHY THE VIEWER LINE IS NOW THE LOAD-BEARING ONE: the tempting way to
	// implement this widening is to drop the separate rego rule and add the verb
	// to content_actions. That produces the editor line for free AND hands the
	// dry run to viewer through the read rule beneath it. Only the viewer
	// assertion can tell "widened to editor" from "widened to everybody".
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentSchemaPlan, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentSchemaPlan, Resource: Resource{Type: "content", ID: "article"},
	}))
	schemaOwner := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t_a", TenantRole: "owner",
	})
	require.NoError(t, auth.Allow(schemaOwner, Input{
		Action: ActionContentSchemaPlan, Resource: Resource{Type: "content", ID: "article"},
	}))

	// Propose through the real policy, same shape as plan and opened in the same
	// edit — a proposer who cannot read a plan cannot know what the proposal
	// says, so the two editor lines stand or fall together.
	//
	// What still bounds an editor here is not this rule but the one two
	// assertions above: ListSchemaProposals, GetSchemaProposal and both decide
	// methods all authorize content:schema:WRITE (schema_proposal.go:214,336,
	// 364,431), which stays owner/admin. So an editor may file into the queue
	// and may not read it or decide on it.
	//
	// NOT content:review:list — that is ADR-014 §2's release queue, a different
	// queue that editors have always held. Naming it here would read as a bound
	// that does not exist.
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentSchemaPropose, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentSchemaPropose, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.NoError(t, auth.Allow(schemaOwner, Input{
		Action: ActionContentSchemaPropose, Resource: Resource{Type: "content", ID: "article"},
	}))

	// Amend through the real policy: an editor keeps it (nothing was taken away
	// when it moved off content:update) while still losing the destructive verb
	// two assertions above. A viewer must not reach it through the read rule.
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentSchemaAmend, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentSchemaAmend, Resource: Resource{Type: "content", ID: "article"},
	}))

	// Publish through the REAL policy, not the Go matrix. This is the assertion
	// that catches the failure the code comments warn about: the authorizer
	// fails closed on unknown actions, so a verb that lands in Go without a rego
	// rule refuses owner and admin too — the feature would look like a
	// permissions bug rather than a missing policy. The viewer line keeps the
	// new rule from being reachable through the read rule.
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentPublish, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentPublish, Resource: Resource{Type: "content", ID: "article"},
	}))

	// The activity stream and the release queue through the REAL policy (ADR-014
	// §3/§2, ruled 2026-08-06). Both halves matter and for different reasons:
	//
	//   - the EDITOR lines catch the same failure the publish block warns about —
	//     a verb that lands in Go with no rego rule fails closed for everyone, so
	//     the ruling would present as the console being broken for admins.
	//   - the VIEWER lines are the ruling itself. These two verbs exist ONLY to
	//     take the stream and the queue away from viewer, who reached both
	//     through content:list. If either verb were ever folded into
	//     content_actions, the editor lines would still pass and only these would
	//     notice.
	//
	// authz_test.rego states the same four expectations and IS run in CI —
	// `make opa-test`, the workflow's `opa` job, in an OPA container over the
	// policies directory. This is not a duplicate of it. That suite evaluates the
	// policy in isolation, with input handed to it directly; this one goes
	// through NewOPAAuthorizer, so it also covers the wiring the rego suite
	// cannot see — that the Go side builds the input this policy expects and
	// that the verb survives the agent gate on the way in. A rule can be
	// perfectly correct in authz_test.rego and unreachable in the product.
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentActivityRead, Resource: Resource{Type: "content", ID: "activity"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentActivityRead, Resource: Resource{Type: "content", ID: "activity"},
	}))
	require.NoError(t, auth.Allow(editor, Input{
		Action: ActionContentReviewList, Resource: Resource{Type: "content", ID: "pending-review"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionContentReviewList, Resource: Resource{Type: "content", ID: "pending-review"},
	}))

	// §7 #4: tenant admin must not trip is_admin (F1 regression lock).
	tenantAdmin := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t_a", TenantRole: "admin",
	})
	require.NoError(t, auth.Allow(tenantAdmin, Input{
		Action: ActionContentDelete, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.NoError(t, auth.Allow(tenantAdmin, Input{
		Action: ActionContentSchemaWrite, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(tenantAdmin, Input{
		Action: ActionPlatformAppCreate, Resource: Resource{Type: "platform_app", ID: "x"},
	}))
	require.Error(t, auth.Allow(tenantAdmin, Input{
		Action: ActionUserDelete, Resource: Resource{Type: "user", ID: uuid.NewString()},
	}))

	// §7 #5 / D12: platform admin gets no blanket over tenant-plane actions,
	// with or without an active tenant, but keeps the platform plane.
	platformAdmin := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), Roles: []string{"admin"}, TenantID: "t_a",
	})
	require.Error(t, auth.Allow(platformAdmin, Input{
		Action: ActionContentRead, Resource: Resource{Type: "content", ID: "article"},
	}))
	require.Error(t, auth.Allow(platformAdmin, Input{
		Action: ActionTenantInviteCreate, Resource: Resource{Type: "tenant", ID: "t_a"},
	}))
	require.NoError(t, auth.Allow(platformAdmin, Input{
		Action: ActionPlatformAppCreate, Resource: Resource{Type: "platform_app", ID: "x"},
	}))

	// Invites in OPA mode mirror the facade: owner creates, editor cannot,
	// any authenticated subject accepts.
	owner := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t_a", TenantRole: "owner",
	})
	require.NoError(t, auth.Allow(owner, Input{
		Action: ActionTenantInviteCreate, Resource: Resource{Type: "tenant", ID: "t_a"},
	}))
	require.Error(t, auth.Allow(editor, Input{
		Action: ActionTenantInviteCreate, Resource: Resource{Type: "tenant", ID: "t_a"},
	}))
	require.NoError(t, auth.Allow(viewer, Input{
		Action: ActionTenantInviteAccept, Resource: Resource{Type: "tenant", ID: "invite"},
	}))

	// Agent credentials in OPA mode mirror the facade (ruled 2026-08-06): the
	// two membership-administration roles hold the verbs and the content-write
	// roles do not. The rego suite covers the policy in isolation; this covers
	// the same rules THROUGH the Go authorizer, which is where a mismatch
	// between the two modes would actually be felt.
	require.NoError(t, auth.Allow(owner, Input{
		Action: ActionAgentCredentialIssue, Resource: Resource{Type: "agent_credential", ID: ""},
	}))
	require.Error(t, auth.Allow(editor, Input{
		Action: ActionAgentCredentialIssue, Resource: Resource{Type: "agent_credential", ID: ""},
	}))
	require.NoError(t, auth.Allow(owner, Input{
		Action: ActionAgentCredentialRevoke, Resource: Resource{Type: "agent_credential", ID: "cred-1"},
	}))
	require.Error(t, auth.Allow(viewer, Input{
		Action: ActionAgentCredentialList, Resource: Resource{Type: "agent_credential", ID: ""},
	}))
	require.Error(t, auth.Allow(platformAdmin, Input{
		Action: ActionAgentCredentialIssue, Resource: Resource{Type: "agent_credential", ID: ""},
	}))
}
