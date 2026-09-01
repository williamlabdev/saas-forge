package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ADR-013 未解項「提案者看不到自己提案的後續」, landed as a narrowed single read
// (000038). Same newSvc AllowAllAuthorizer as the rest of this suite, so every
// refusal below is provably the credential-scope gate rather than the RBAC
// matrix.

// ctxAgentNamed is ctxAgent with the agent's NAME as a parameter, which is the
// axis these tests turn on: two agents minted by the same person share a
// principal id and differ only here.
func ctxAgentNamed(tenant, role string, principal uuid.UUID, agentID string, allowed []string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:       principal,
		TenantID:     tenant,
		TenantRole:   role,
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		AllowedTypes: allowed,
	})
}

// planMentions reports whether any step of the plan names the given type. The
// question the narrowing exists to answer is "can this credential learn that
// `invoice` exists", so the assertion is about REACH, not about counts.
func planMentions(p PlanResult, typeName string) bool {
	for _, s := range p.Steps {
		if s.Type == typeName {
			return true
		}
	}
	return false
}

// THE POINT OF THE ENDPOINT: the proposer gets its own view back, and that view
// is not the one stored for the approver.
//
// Both halves are asserted together on purpose. "The proposer sees a plan" is
// satisfied by handing back the stored one, which is precisely the leak §4
// forbids — the stored plan carries a delete step for every live type absent
// from the document, so it is the tenant's type list under another name.
func TestProposerReadsItsOwnProposalInItsOwnScope(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	principal := uuid.New()
	agent := ctxAgentNamed("t1", "editor", principal, "content-bot", []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
	id := proposalID(t, filed)

	own, err := svc.GetOwnSchemaProposal(agent, id)
	require.NoError(t, err)
	require.True(t, own.PlanRecorded, "a proposal filed after 000038 must carry the proposer's view")
	require.NotNil(t, own.Plan)
	assert.False(t, planMentions(*own.Plan, "invoice"),
		"the proposer's view must not name a type outside its whitelist")

	// The stored plan — what the approver reviews — does name it. Without this
	// half the test above would pass against an implementation that stored the
	// narrowed view, which is the change that makes agent proposals unapprovable.
	stored, err := svc.GetSchemaProposal(owner, id)
	require.NoError(t, err)
	assert.True(t, planMentions(stored.Plan, "invoice"),
		"the approver's stored plan must still be full-scope")
}

// THE LOAD-BEARING REFUSAL. proposed_by holds the PRINCIPAL, so every agent one
// person mints shares it. Matching ownership on the principal alone would let
// a credential scoped to `post` read a proposal filed by a sibling scoped to
// `invoice` — and read the type names in it.
//
// Both agents here are minted by the SAME principal and differ only in name,
// which is what makes this test able to fail: give them different principals
// and it passes against the broken implementation too.
func TestSiblingAgentOfTheSamePrincipalCannotReadTheProposal(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	principal := uuid.New()
	filer := ctxAgentNamed("t1", "editor", principal, "post-bot", []string{"post"})
	sibling := ctxAgentNamed("t1", "editor", principal, "invoice-bot", []string{"invoice"})

	filed, err := svc.ProposeSchema(filer, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
	id := proposalID(t, filed)

	_, err = svc.GetOwnSchemaProposal(sibling, id)
	assert.ErrorIs(t, err, apperrors.ErrNotFound,
		"a sibling credential of the same principal must not reach another agent's proposal")

	// And the filer still can — otherwise the assertion above would also hold
	// for an endpoint that refuses everybody.
	_, err = svc.GetOwnSchemaProposal(filer, id)
	require.NoError(t, err)
}

// A person's row carries a NULL agent name, and the match must still find it.
// This is the case `proposed_by_agent = $5` gets wrong: NULL = NULL is NULL, so
// a human would be unable to read the proposal they filed themselves.
func TestHumanProposerCanReadItsOwnProposal(t *testing.T) {
	svc, _ := newSvc()
	ownerID := uuid.New()
	owner := ctxRoleUser("t1", "owner", ownerID)
	seedPostType(t, svc, owner, "post")

	filed, err := svc.ProposeSchema(owner, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	own, err := svc.GetOwnSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.True(t, own.PlanRecorded)
	assert.Equal(t, repository.ProposalPending, own.Status)

	// A different person does not get it, even holding the same verbs.
	other := ctxRoleUser("t1", "owner", uuid.New())
	_, err = svc.GetOwnSchemaProposal(other, proposalID(t, filed))
	assert.ErrorIs(t, err, apperrors.ErrNotFound)
}

// §4 is re-checked against the CURRENT whitelist, not the one in force when the
// row was filed. A credential re-minted under the same agent name with a
// narrower scope must not read back the wider proposal its predecessor filed —
// the row matches on identity, so nothing but this second check stops it.
func TestReMintedNarrowerCredentialCannotReadTheWiderProposal(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	principal := uuid.New()
	wide := ctxAgentNamed("t1", "editor", principal, "content-bot", []string{"post", "invoice"})

	filed, err := svc.ProposeSchema(wide, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
	id := proposalID(t, filed)

	// Same tenant, same principal, same agent name — only the scope shrank, and
	// the document it filed names a type the new scope excludes.
	narrowed := ctxAgentNamed("t1", "editor", principal, "content-bot", []string{"invoice"})
	_, err = svc.GetOwnSchemaProposal(narrowed, id)
	require.Error(t, err)
	assert.NotErrorIs(t, err, apperrors.ErrNotFound,
		"the row was found; the refusal must come from the scope check, not from the lookup")
}

// A NULL plan_proposer means NOT RECORDED — an agent row filed before 000038,
// whose narrowed view is unreconstructible. It must not render as an empty plan:
// "no plan recorded" and "this proposal would change nothing" are different
// claims, and only one of them is true.
func TestOwnProposalWithNoRecordedPlanSaysSoRatherThanShowingAnEmptyOne(t *testing.T) {
	svc, repo := newSvc()
	principal := uuid.New()
	agentID := "content-bot"
	agent := ctxAgentNamed("t1", "editor", principal, agentID, []string{"post"})

	artJSON, err := json.Marshal(art(postType(titleField)))
	require.NoError(t, err)
	planJSON, err := json.Marshal(PlanResult{})
	require.NoError(t, err)
	// Filed the way 000037 wrote rows: an approver plan and no proposer view.
	rec := &repository.SchemaProposal{
		TenantID: "t1", Artifact: artJSON, Plan: planJSON, PlanProposer: nil,
		ProposedBy: principal, ProposedByKind: "agent", ProposedByAgent: &agentID,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, repo.CreateSchemaProposal(context.Background(), rec))

	own, err := svc.GetOwnSchemaProposal(agent, rec.ID)
	require.NoError(t, err)
	assert.False(t, own.PlanRecorded, "a row with no stored proposer view must say so")
	assert.Nil(t, own.Plan, "and must not substitute an empty plan for the missing one")
}

// Expiry is derived on this surface too, from the same rule the queue uses. A
// proposer that read `pending` on a row nobody may act on any more would be told
// its proposal was still waiting for an answer it can no longer get.
func TestOwnProposalRendersTheDerivedExpiredStatus(t *testing.T) {
	svc, repo := newSvc()
	principal := uuid.New()
	agentID := "content-bot"
	agent := ctxAgentNamed("t1", "editor", principal, agentID, []string{"post"})

	artJSON, err := json.Marshal(art(postType(titleField)))
	require.NoError(t, err)
	planJSON, err := json.Marshal(PlanResult{})
	require.NoError(t, err)
	rec := &repository.SchemaProposal{
		TenantID: "t1", Artifact: artJSON, Plan: planJSON, PlanProposer: planJSON,
		ProposedBy: principal, ProposedByKind: "agent", ProposedByAgent: &agentID,
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, repo.CreateSchemaProposal(context.Background(), rec))

	own, err := svc.GetOwnSchemaProposal(agent, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, ProposalStatusExpired, own.Status)
}

// The narrowed read is an addition, not a widening: the queue and the approver's
// single read stay shut to agents. Asserted here rather than trusted, because
// this change is exactly the kind that opens them by accident.
func TestNarrowedReadDoesNotOpenTheQueueToAgents(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	agent := ctxAgentNamed("t1", "editor", uuid.New(), "content-bot", []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	_, err = svc.ListSchemaProposals(agent)
	assert.Error(t, err, "the queue's stored plans are full-scope; agents stay out")
	_, err = svc.GetSchemaProposal(agent, proposalID(t, filed))
	assert.Error(t, err, "the approver's read returns the stored plan; agents stay out")
}

// The activity stream gets its own verb for this read (ActivityEntryAttribution's
// precedent): filed under schema.propose, an operator reading the log would be
// told a proposal was FILED, and a refused one would look like an agent trying
// to change the schema.
func TestOwnProposalReadIsRecordedUnderItsOwnVerb(t *testing.T) {
	assert.True(t, domain.ValidActivityAction(domain.ActivitySchemaProposalRead))
	assert.NotEqual(t, domain.ActivitySchemaPropose, domain.ActivitySchemaProposalRead)
}

// --- the proposer's LIST (ADR-013 補裁 T) -------------------------------------

// ownListIDs reads the ids out of the proposer's list, so the assertions below
// are about WHICH rows came back rather than how many — a count assertion goes
// green on a list that returned the right number of the wrong rows.
func ownListIDs(t *testing.T, svc ContentService, ctx context.Context) []string {
	t.Helper()
	list, err := svc.ListOwnSchemaProposals(ctx)
	require.NoError(t, err)
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

// THE POINT OF THE ENDPOINT: without it a proposer holds an id only for as long
// as it holds the response that filed it, and 補裁 T opened proposing to a role
// that can reach no other proposal surface at all.
//
// The sibling and the human are here because `proposed_by` holds the PRINCIPAL:
// a WHERE clause matching on it alone would list all four rows, and would pass
// every assertion a single-credential test could make.
func TestProposerListsOnlyItsOwnRowsNewestFirst(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRoleUser("t1", "owner", uuid.New())
	seedPostType(t, svc, owner, "post", "invoice")
	principal := uuid.New()
	filer := ctxAgentNamed("t1", "editor", principal, "post-bot", []string{"post"})
	sibling := ctxAgentNamed("t1", "editor", principal, "invoice-bot", []string{"invoice"})

	first, err := svc.ProposeSchema(filer, art(postType(titleField)), false)
	require.NoError(t, err)
	second, err := svc.ProposeSchema(filer, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
	// Same person, different credential.
	theirs, err := svc.ProposeSchema(sibling, art(invoiceType()), false)
	require.NoError(t, err)
	// The same person again, this time as themselves: a human row carries a NULL
	// agent name, so it must not fall into an agent's list either.
	mine, err := svc.ProposeSchema(owner, art(postType(titleField)), false)
	require.NoError(t, err)

	got := ownListIDs(t, svc, filer)
	assert.Equal(t, []string{second.ID, first.ID}, got,
		"the proposer's list must be its own rows, newest first")
	assert.NotContains(t, got, theirs.ID,
		"a sibling credential's proposal reached this list — ownership matched on the principal alone")
	assert.NotContains(t, got, mine.ID,
		"the person's own row reached an agent's list; IS NOT DISTINCT FROM matched a NULL agent name to a name")

	// And the human's list is theirs, which is what stops the assertions above
	// from also holding for an endpoint that returns nothing.
	assert.Equal(t, []string{mine.ID}, ownListIDs(t, svc, owner))
}

// THE PLAN IS THE PROPOSER'S VIEW, NEVER THE STORED ONE (補裁 Q-2). The stored
// plan carries a step for every live type absent from the document, so handing
// it over is handing over the tenant's type list — the leak the single read was
// narrowed to close, reopened in bulk by a list that took the shorter field.
func TestProposerListReturnsTheProposerPlanAndNotTheStoredApproverPlan(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	agent := ctxAgentNamed("t1", "editor", uuid.New(), "content-bot", []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	list, err := svc.ListOwnSchemaProposals(agent)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, list[0].PlanRecorded, "a proposal filed after 000038 carries the proposer's view")
	require.NotNil(t, list[0].Plan)
	assert.False(t, planMentions(*list[0].Plan, "invoice"),
		"the list named a type outside the caller's whitelist")

	// The control. Without it the assertion above would also pass against an
	// implementation that stored the narrowed view — which is the change that
	// makes agent proposals unapprovable (see ProposeSchema).
	stored, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.True(t, planMentions(stored.Plan, "invoice"),
		"the approver's stored plan must still be full-scope")
}

// THE RULING §4 FORCED ON THE LIST: a row the current whitelist no longer
// covers is OMITTED, and the rest of the page still answers.
//
// The single read refuses the same row outright, and both are right. One row
// asked about has one answer; a page has many, and refusing the page because a
// credential was re-minted with one type fewer would take away the proposals
// still inside its scope — turning the endpoint off rather than narrowing it.
func TestNarrowedCredentialLosesTheOutOfScopeRowAndKeepsTheRest(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	principal := uuid.New()
	wide := ctxAgentNamed("t1", "editor", principal, "content-bot", []string{"post", "invoice"})

	onPost, err := svc.ProposeSchema(wide, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
	onInvoice, err := svc.ProposeSchema(wide, art(invoiceType()), false)
	require.NoError(t, err)

	// Same tenant, same principal, same agent name — only the scope shrank.
	narrowed := ctxAgentNamed("t1", "editor", principal, "content-bot", []string{"post"})

	got := ownListIDs(t, svc, narrowed)
	assert.Equal(t, []string{onPost.ID}, got,
		"the in-scope row must survive a narrowing that only the other row fails")
	assert.NotContains(t, got, onInvoice.ID,
		"a proposal naming a type outside the current whitelist was listed")

	// The row is still there and still owned — it is the SCOPE that hides it,
	// which is what the single read says when asked about it directly.
	_, err = svc.GetOwnSchemaProposal(narrowed, proposalID(t, onInvoice))
	require.Error(t, err)
	assert.NotErrorIs(t, err, apperrors.ErrNotFound,
		"the row was found; the refusal must come from the scope check")

	// And the wider credential still sees both, or the omission above would be
	// indistinguishable from a list that simply loses rows.
	assert.ElementsMatch(t, []string{onPost.ID, onInvoice.ID}, ownListIDs(t, svc, wide))
}

// A credential with NO whitelist is refused for the whole request, not handed
// an empty page. Both are "fail closed", and only one of them is honest: an
// empty list says this credential filed nothing, and the difference matters to
// an agent deciding whether to file again.
func TestUnscopedAgentCredentialIsRefusedRatherThanListedAsEmpty(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	unscoped := ctxAgentNamed("t1", "editor", uuid.New(), "content-bot", nil)

	_, err := svc.ListOwnSchemaProposals(unscoped)
	require.Error(t, err, "an agent credential with no whitelist was handed a list")
	assert.NotErrorIs(t, err, apperrors.ErrNotFound)
}

// The list is an addition, not a widening: the approvers' queue stays shut to
// agents. Asserted next to the new endpoint because this is exactly the change
// that opens it by accident — both are lists of the same table.
func TestTheProposerListDoesNotOpenTheQueue(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgentNamed("t1", "editor", uuid.New(), "content-bot", []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	_, err = svc.ListOwnSchemaProposals(agent)
	require.NoError(t, err, "the proposer's own list is the door this ruling opened")
	_, err = svc.ListSchemaProposals(agent)
	assert.Error(t, err, "the queue's stored plans are full-scope; agents stay out")
}

// A caller who passes the verb check but has NOBODY answerable for it is
// refused, rather than handed somebody else's list — or a panic.
//
// This is the `by == nil` line, and it is reachable: actor() returns nil both
// for a public-delivery subject and for any subject whose responsible user id
// is the zero UUID. Neither has filed anything, and the query that would run
// without this guard dereferences the nil pointer it was handed.
//
// The test exists because removing that guard was, until it was written, a
// mutation the suite did not notice.
func TestOwnProposalListRefusesACallerWithNobodyAnswerableForIt(t *testing.T) {
	svc, _ := newSvc()

	// A tenant role good enough for the verb — newSvc's AllowAllAuthorizer would
	// pass anything, which is exactly why the refusal below has to come from the
	// provenance check and nowhere else — and no responsible user behind it.
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID:     uuid.Nil,
		TenantID:   "t1",
		TenantRole: "editor",
	})

	_, err := svc.ListOwnSchemaProposals(ctx)
	require.ErrorIs(t, err, apperrors.ErrForbidden)

	// The single read carries the identical line and was ALSO unguarded by any
	// test until now — found by running the same mutation against it. Asserted
	// here rather than in a file of its own because the two guards are one rule
	// and a reader who deletes one will look for the test beside it.
	_, err = svc.GetOwnSchemaProposal(ctx, uuid.New())
	require.ErrorIs(t, err, apperrors.ErrForbidden)
}
