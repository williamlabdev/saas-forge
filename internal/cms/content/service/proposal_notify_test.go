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
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// ADR-013 §3 step 8, the notification half (william ruled 2026-08-07);
// rewritten for the owner/admin fan-out ADR-023 (2026-09-09) introduced.

type sentTenantNotice struct {
	tenantID string
	roles    []string
	exclude  uuid.UUID
	kind     string
	title    string
	body     string
	link     string
}

type sentUserNotice struct {
	userID   uuid.UUID
	tenantID string
	kind     string
	title    string
	body     string
	link     string
}

type fakeNotifier struct {
	sent     []sentTenantNotice
	sentUser []sentUserNotice
	err      error
}

func (f *fakeNotifier) NotifyTenantRoles(_ context.Context, tenantID string, roles []string, exclude uuid.UUID, kind, title, body, link string) error {
	f.sent = append(f.sent, sentTenantNotice{
		tenantID: tenantID, roles: roles, exclude: exclude, kind: kind, title: title, body: body, link: link,
	})
	return f.err
}

func (f *fakeNotifier) NotifyUser(_ context.Context, userID uuid.UUID, tenantID, kind, title, body, link string) error {
	f.sentUser = append(f.sentUser, sentUserNotice{
		userID: userID, tenantID: tenantID, kind: kind, title: title, body: body, link: link,
	})
	return f.err
}

func newSvcWithNotifier() (ContentService, *memRepo, *fakeNotifier) {
	svc, repo := newSvc()
	n := &fakeNotifier{}
	return WithNotifier(svc, n), repo, n
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

// The notice EXCLUDES the PRINCIPAL — the person who minted the credential
// and, by 補裁 O, someone who holds the verb that decides this proposal, so
// notifying them their own agent's proposal is theirs to decide would be
// telling them what they can already see. Every OTHER owner/admin still
// hears about it — that fan-out is what NotifyTenantRoles(roles) exists for;
// this test only pins the one recipient the content service is responsible
// for naming (exclude), not the roster NotifyTenantRoles resolves.
func TestAgentProposalExcludesItsPrincipal(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal, operator := uuid.New(), uuid.New()
	agent := ctxAgentSplit("t1", "editor", operator, principal, []string{"post"})

	_, err := svc.ProposeSchema(agent, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	require.Len(t, notifier.sent, 1, "an agent proposal must push a notice; the queue is pull-only otherwise")
	got := notifier.sent[0]
	assert.Equal(t, "t1", got.tenantID)
	assert.ElementsMatch(t, []string{"owner", "admin"}, got.roles)
	assert.Equal(t, principal, got.exclude,
		"the excluded recipient must be the principal, not whoever the token's sub happens to be")
	assert.NotEqual(t, operator, got.exclude)
	assert.Equal(t, domain.KindSchemaProposal, got.kind)
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

// FIXED 2026-09-09 (ADR-023): an OWNER who proposes now still reaches every
// OTHER owner/admin of the tenant — only the proposer themselves is excluded.
// Before this fix the code hard-coded "notify nobody for any human proposer",
// reasoning that a lone owner was the only member who could decide; that
// reasoning was always wrong the moment a tenant had a SECOND owner or an
// admin, and 補裁 T's editor-can-propose widening made the single-decider
// assumption wrong for every tenant. This test now pins the call happening
// with the proposer excluded, not the call being skipped.
func TestHumanProposalNotifiesTenantRolesExcludingSelf(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	ownerID := uuid.New()
	owner := ctxRoleUser("t1", "owner", ownerID)
	seedPostType(t, svc, owner, "post")

	_, err := svc.ProposeSchema(owner, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	require.Len(t, notifier.sent, 1)
	assert.Equal(t, ownerID, notifier.sent[0].exclude,
		"the proposer is excluded, not the whole tenant — a co-owner or admin must still hear about it")
}

// FIXED 2026-09-09 (ADR-023), CLOSING THE GAP THIS TEST USED TO PIN AS
// KNOWINGLY WRONG. 補裁 T (2026-08-30) opened content:schema:propose to
// editor, and until this fix an editor's proposal reached nobody: the old
// code notified only agent proposals, on the expired reasoning that any
// human proposer was already an approver. It no longer is. An editor now
// excludes only themselves, exactly like a human owner/admin proposer does.
func TestEditorProposalNotifiesTenantRolesExcludingSelf(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	editorID := uuid.New()
	editor := ctxRoleUser("t1", "editor", editorID)
	_, err := svc.ProposeSchema(editor, art(postType(titleField, bodyField())), false)
	require.NoError(t, err)

	require.Len(t, notifier.sent, 1, "an editor's proposal must now reach the tenant's owner/admin")
	assert.Equal(t, editorID, notifier.sent[0].exclude)
	assert.ElementsMatch(t, []string{"owner", "admin"}, notifier.sent[0].roles)
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
	svc := WithNotifier(
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
