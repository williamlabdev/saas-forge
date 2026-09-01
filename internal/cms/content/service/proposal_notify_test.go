package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// ADR-013 §3 step 8, the notification half (william ruled 2026-08-07).

type sentNotice struct {
	userID uuid.UUID
	title  string
	body   string
}

type fakeNotifier struct {
	sent []sentNotice
	err  error
}

func (f *fakeNotifier) NotifyUser(_ context.Context, userID uuid.UUID, title, body string) error {
	f.sent = append(f.sent, sentNotice{userID: userID, title: title, body: body})
	return f.err
}

func newSvcWithNotifier() (ContentService, *memRepo, *fakeNotifier) {
	svc, repo := newSvc()
	n := &fakeNotifier{}
	return WithProposalNotifier(svc, n), repo, n
}

// ctxAgentSplit builds an agent whose UserID and PrincipalID DIFFER.
//
// ctxAgent sets both to the same uuid, which is faithful to minting (the token's
// sub is the principal) but leaves this test unable to say anything: "notify
// sub.UserID" and "notify the principal" produce identical rows under that
// fixture. Same blind spot step 3 hit — a fixture that makes two values equal
// hides the rule that says which one to read — and the same answer: build the
// subject the minting path cannot produce.
//
// MEASURED, not assumed (2026-08-07). Swapping this fixture back to equal ids
// makes the test fail BOTH with and without the mutation, so it stops being
// able to tell them apart. The NotEqual assertion below is what converts that
// into a loud failure instead of a vacuous pass; it is load-bearing, not
// decoration.
func ctxAgentSplit(tenant, role string, user, principal uuid.UUID, allowed []string) context.Context {
	agentID := "content-bot"
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:       user,
		TenantID:     tenant,
		TenantRole:   role,
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		AllowedTypes: allowed,
	})
}

// The notice goes to the PRINCIPAL — the person who minted the credential and,
// by 補裁 O, someone who holds the verb that decides this proposal.
func TestAgentProposalNotifiesItsPrincipal(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal, operator := uuid.New(), uuid.New()
	agent := ctxAgentSplit("t1", "editor", operator, principal, []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	require.Len(t, notifier.sent, 1, "an agent proposal must push a notice; the queue is pull-only otherwise")
	assert.Equal(t, principal, notifier.sent[0].userID,
		"the recipient must be the principal, not whoever the token's sub happens to be")
	assert.NotEqual(t, operator, notifier.sent[0].userID)
}

// COUNTS, NOT CONTENT. The body must not name types: notifications have no
// field-level masking and are written by one role and read by another, which is
// the trap step 3's activity titles hit from the write side.
func TestProposalNoticeNamesNoContentTypes(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")

	principal := uuid.New()
	agent := ctxAgentSplit("t1", "editor", uuid.New(), principal, []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
	require.Len(t, notifier.sent, 1)

	got := notifier.sent[0].title + " " + notifier.sent[0].body
	for _, name := range []string{"post", "invoice"} {
		assert.NotContains(t, strings.ToLower(got), name,
			"the notice leaked a content type name: %q", got)
	}
}

// An OWNER who proposes can already approve, so a notice would be telling them
// what they just did. This is the half that says the rule is "agents", not
// "proposals".
//
// The subject here is an owner ON PURPOSE, and it stopped being interchangeable
// with "a human" on 2026-08-30: 補裁 T opened content:schema:propose to editor,
// so "whoever may propose may decide" is no longer true of people. The rule this
// test pins (notify agents, not humans) is unchanged and still right for an
// owner; what changed is that it now has a case it answers WRONGLY, pinned
// directly below.
func TestHumanProposalNotifiesNobody(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	_, err := svc.ProposeSchema(owner, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	assert.Empty(t, notifier.sent,
		"a human proposer is already an approver; notifying them is noise")
}

// 🔴 THE KNOWN GAP, pinned as it actually behaves rather than as it should
// (ADR-013 補裁 T 未解項, opened 2026-08-30). An EDITOR may now file a proposal
// and may not decide one — so this proposal sits in a queue that nobody was told
// about, for the whole 7-day TTL, until an owner or admin happens to look.
//
// This test asserts the WRONG behaviour on purpose. Deleting it would leave the
// gap invisible; asserting the right behaviour would leave the suite red on a
// defect that needs a ruling and not a patch (the fix has to enumerate the
// tenant's owner/admin memberships — a repository this service does not hold —
// and decide how many of them to wake). When that ruling lands, THIS is the test
// that must go red, and its name says so.
//
// Not caught by the agent tests above: an agent notifies its principal, and a
// principal is a minter, and minting is owner/admin — so every agent path
// already reaches someone who can decide. Only a human editor falls through.
func TestEditorProposalNotifiesNobodyYet(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	editor := ctxRole("t1", "editor")
	_, err := svc.ProposeSchema(editor, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	assert.Empty(t, notifier.sent,
		"an editor proposer now reaches somebody — 補裁 T's 未解項 was closed and this test must be rewritten, not deleted")
}

// The proposal is committed before the notice is attempted, so a notification
// outage must not turn a landed proposal into an error the agent reads as "not
// filed".
func TestProposalSurvivesANotifierFailure(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	notifier.err = errors.New("notification plane is down")
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	agent := ctxAgentSplit("t1", "editor", uuid.New(), principal, []string{"post"})

	filed, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err, "a failed notification must not fail the proposal")

	// Committed, not merely a green error value: the approver can still find it.
	stored, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, filed.ID, stored.ID)
}

// failingProposalRepo makes the one write this flow depends on fail, and
// nothing else.
type failingProposalRepo struct {
	repository.ContentRepository
	err error
}

func (f *failingProposalRepo) CreateSchemaProposal(context.Context, *repository.SchemaProposal) error {
	return f.err
}

// ORDER IS THE ASSERTION. Notifying before the insert would announce a proposal
// that does not exist — the approver opens an empty queue, and the agent got an
// error saying its proposal did not land. Nothing else in this file catches
// that: moving the call above CreateSchemaProposal leaves every other test in
// the package green (measured 2026-08-07), because they all run against a
// repository whose write succeeds.
func TestNoNoticeWhenTheProposalFailsToLand(t *testing.T) {
	repo := &failingProposalRepo{
		ContentRepository: &memRepo{},
		err:               errors.New("insert failed"),
	}
	notifier := &fakeNotifier{}
	svc := WithProposalNotifier(
		NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{})),
		notifier,
	)
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgentSplit("t1", "editor", uuid.New(), uuid.New(), []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.Error(t, err, "the repository refused the insert; the call must not report success")
	assert.Empty(t, notifier.sent,
		"a notice went out for a proposal that was never stored")
}

// No notifier wired is the pre-existing deployment shape, not a crash.
func TestProposalWithoutANotifierStillWorks(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgentSplit("t1", "editor", uuid.New(), uuid.New(), []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)
}
