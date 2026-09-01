package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ADR-013 §3 step 8 — the proposal flow at the service layer.
//
// These run under newSvc's AllowAllAuthorizer, so a refusal below is provably
// the CMS chokepoint rather than the RBAC matrix. Who holds
// content:schema:propose is settled in internal/pkg/authz's own suite, and the
// end-to-end claim — an agent credential over real HTTP — belongs in test/e2e.

func bodyField() domain.ArtifactField {
	return domain.ArtifactField{Key: "body", Type: domain.FieldTypeText, Label: "Body"}
}

// proposalID parses the DTO's id, failing loudly: a test that carried a zero
// UUID forward would exercise the not-found path while looking like it
// exercised the flow.
func proposalID(t *testing.T, dto SchemaProposalDTO) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(dto.ID)
	require.NoError(t, err)
	return id
}

// THE LOAD-BEARING TEST OF THE WHOLE STEP (william ruled 2026-08-06).
//
// An agent's plan is narrowed to its whitelist, so the plan it sees omits the
// delete step for every type it may not touch. If the row stored that view, the
// approver's re-run — which is not narrowed — would differ on the FIRST proposal
// in any tenant with a second content type, and the strict comparison the
// ruling asks for would make the feature unusable rather than safe.
//
// So this asserts all three halves at once: the proposer sees its own scope,
// the stored plan is the approver's, and the approval goes through.
func TestAgentProposalIsApprovableInATenantWithOtherTypes(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	// In scope, and a real change: add `body` to post.
	proposed := art(postType(titleField, bodyField()))

	filed, err := svc.ProposeSchema(agent, proposed, false)
	require.NoError(t, err)

	for _, step := range filed.Plan.Steps {
		require.NotEqual(t, "invoice", step.Type,
			"the proposer's copy named a type its credential may not see: %+v", step)
	}

	stored, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	var sawInvoice bool
	for _, step := range stored.Plan.Steps {
		if step.Type == "invoice" {
			sawInvoice = true
		}
	}
	require.True(t, sawInvoice,
		"the stored plan must be the APPROVER's view — without the invoice step this test cannot tell the two views apart")

	applied, err := svc.ApproveSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err, "an agent proposal in a tenant with a second type must be approvable; this is the ruling")
	require.NotZero(t, applied.Applicable)

	// The change actually landed, not merely a green error value.
	ct, err := svc.GetContentType(owner, "post")
	require.NoError(t, err)
	var keys []string
	for _, f := range ct.Fields {
		keys = append(keys, f.Key)
	}
	assert.Contains(t, keys, "body")

	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, repository.ProposalApproved, after.Status)
	require.NotNil(t, after.DecidedBy)
	assert.NotNil(t, after.DecidedAt)
}

// 驗證計畫第 5 條: the schema moved, so the approval must fail.
func TestApprovingAStaleProposalIsRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	// Somebody else changes the schema underneath the proposal.
	_, err = svc.AddField(owner, "post", FieldInput{Key: "summary", Type: domain.FieldTypeText, Label: "Summary"})
	require.NoError(t, err)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_STALE", codeOf(t, err))
	assert.Equal(t, 409, statusOf(t, err))

	// AND NOTHING WAS APPLIED. Without this the test would pass on an
	// implementation that applied the change and then reported the mismatch.
	ct, err := svc.GetContentType(owner, "post")
	require.NoError(t, err)
	for _, f := range ct.Fields {
		assert.NotEqual(t, "body", f.Key, "a refused approval must not have applied its artifact")
	}

	// The row stays pending: refusing a stale plan is not a decision, and
	// marking it decided would destroy the only record of what was asked.
	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, repository.ProposalPending, after.Status)
}

// The TTL (william ruled 2026-08-06): the row survives, the permission to act
// on it does not.
func TestExpiredProposalReadsExpiredAndCannotBeApproved(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	// Reach past the constant rather than sleeping for a week. The deadline is
	// what the code reads, so moving it is the same experiment.
	require.Len(t, repo.proposals, 1)
	repo.proposals[0].ExpiresAt = time.Now().UTC().Add(-time.Minute)

	seen, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, ProposalStatusExpired, seen.Status,
		"expiry is derived on read; a stored 'pending' that renders as pending would need a sweeper to become true")

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_EXPIRED", codeOf(t, err))

	err = svc.RejectSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_EXPIRED", codeOf(t, err),
		"rejecting an expired proposal must fail too, or the decision columns would say a person answered one they could not act on")

	// Still there. The TTL hides the button, not the history.
	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, ProposalStatusExpired, after.Status)
}

// A proposal is spent once. The second approval must not apply the artifact a
// second time, and must say why.
func TestAProposalCanOnlyBeDecidedOnce(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_DECIDED", codeOf(t, err))

	err = svc.RejectSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_DECIDED", codeOf(t, err),
		"an approved proposal must not be reopenable by rejecting it")
}

func TestRejectedProposalAppliesNothing(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	require.NoError(t, svc.RejectSchemaProposal(owner, proposalID(t, filed)))

	ct, err := svc.GetContentType(owner, "post")
	require.NoError(t, err)
	for _, f := range ct.Fields {
		assert.NotEqual(t, "body", f.Key)
	}

	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, repository.ProposalRejected, after.Status)
	require.NotNil(t, after.DecidedBy, "who said no is the point of recording a rejection at all")
}

// THE GATE. An agent files; a person answers. Reading the queue is refused for
// the same reason approving is — and both refusals are asserted, because "may
// not approve" with a readable queue would still hand an agent the full-scope
// plan that names every type in the tenant.
func TestAgentCanProposeButNotReadOrDecide(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err, "the positive control: without it every assertion below passes on a service that refuses agents everything")

	id := proposalID(t, filed)

	_, err = svc.ListSchemaProposals(agent)
	require.Error(t, err)
	assert.Equal(t, 403, statusOf(t, err))

	_, err = svc.GetSchemaProposal(agent, id)
	require.Error(t, err)
	assert.Equal(t, 403, statusOf(t, err))

	_, err = svc.ApproveSchemaProposal(agent, id)
	require.Error(t, err)
	assert.Equal(t, 403, statusOf(t, err))

	require.Error(t, svc.RejectSchemaProposal(agent, id))

	// And the person can do all of it — the control that keeps the four
	// assertions above from passing on a broken endpoint.
	list, err := svc.ListSchemaProposals(owner)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, filed.ID, list[0].ID)
	assert.Equal(t, domain.ActorKindAgent, list[0].ProposedByKind)
	require.NotNil(t, list[0].ProposedByAgent)
}

// denyVerb allows everything except one action, so a test can ask WHICH verb a
// method authorizes rather than merely whether it refuses somebody.
type denyVerb struct{ action string }

func (d denyVerb) Allow(_ context.Context, in authz.Input) error {
	if in.Action == d.action {
		return apperrors.ErrForbidden
	}
	return nil
}

// WHICH VERB EACH SIDE ASKS FOR, pinned white-box.
//
// This exists because a mutation proved the obvious test does not cover it:
// swapping the approver methods to authorize content:schema:propose left every
// other test in this file green. Agents are refused the queue by §4's untyped
// rule — the call names no content type — which fires whatever the verb is, so
// a black-box test cannot see the verb at all.
//
// It matters for PEOPLE, and specifically for the widening the ADR invites: if
// propose is ever granted to editors so they can ask for schema changes, a
// queue guarded by propose would hand them the approvers' worklist and the
// approve button along with it. The two sides ask for different verbs, and that
// is the thing to keep true.
func TestProposalSidesAuthorizeDifferentVerbs(t *testing.T) {
	seed := func(a authz.Authorizer) (ContentService, *memRepo) {
		repo := &memRepo{}
		svc := NewContentService(repo, a, staticPlan(Quota{}))
		seedPostType(t, svc, ctxRole("t1", "owner"), "post")
		return svc, repo
	}
	owner := ctxRole("t1", "owner")
	document := art(postType(titleField, bodyField()))

	// Deny propose: filing is refused, and the approver side is untouched.
	svc, _ := seed(denyVerb{action: authz.ActionContentSchemaPropose})
	_, err := svc.ProposeSchema(owner, document, false)
	assert.Equal(t, 403, statusOf(t, err), "ProposeSchema must authorize content:schema:propose")
	_, err = svc.ListSchemaProposals(owner)
	require.NoError(t, err, "the queue must not be gated on the proposer's verb")

	// Deny write: every approver-side call is refused, and filing still works —
	// which is the whole point of the split. An agent holds propose and not
	// write, so this is the same asymmetry seen from the human side.
	svc, _ = seed(denyVerb{action: authz.ActionContentSchemaWrite})
	filed, err := svc.ProposeSchema(owner, document, false)
	require.NoError(t, err, "filing must not require the apply verb, or an agent could never propose")

	_, err = svc.ListSchemaProposals(owner)
	assert.Equal(t, 403, statusOf(t, err), "the queue must authorize content:schema:write")
	_, err = svc.GetSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, 403, statusOf(t, err))
	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, 403, statusOf(t, err), "approving IS applying and must ask for the apply verb")
	err = svc.RejectSchemaProposal(owner, proposalID(t, filed))
	assert.Equal(t, 403, statusOf(t, err))
}

// The artifact an agent may not touch is refused at the door, not stored and
// refused later — the same gate PlanSchema has (補裁 E), which this path would
// otherwise be a way around.
func TestProposingAnArtifactOutsideTheWhitelistIsRefused(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField), invoiceType()), false)
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", codeOf(t, err))
	assert.Empty(t, repo.proposals,
		"a refused proposal must not leave a row; the queue is a worklist and this one would be work nobody asked for")
}
