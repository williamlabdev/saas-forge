package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-014 §2 — the release queue, at the service layer.
//
// 驗證計畫第 9 條 asks for one thing: entries with a working-copy difference
// appear, entries without one do not, and the expected value is written down
// rather than read back out of the queue's own query.
//
// Taken literally that clause is satisfiable by a fixture made entirely of
// published entries — and such a fixture would pass with every draft missing
// from the queue, which is half of what §2's table puts there. So the fixture
// below carries BOTH halves, and the draft half has its own named test so a
// mutation that drops it is attributable rather than merely red somewhere.

// titlesOf reads the labels the console would render. Titles rather than ids
// because the expected values are then legible in the assertion, and because it
// exercises the derivation on the way past.
func titlesOf(rows []PendingEntryDTO) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

// seedQueueFixture builds one of each state §2 can produce and returns the
// owner context.
//
// The four states are deliberate and each is load-bearing:
//
//	live-edited   — published, then edited: the half unpublishedChangesExpr finds
//	live-clean    — published, untouched since: the row that must NOT appear
//	fresh-draft   — never published: the half unpublishedChangesExpr drops
//	retracted     — published, then taken down: a draft holding a snapshot
func seedQueueFixture(t *testing.T, svc ContentService) context.Context {
	t.Helper()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	mk := func(title string) *EntryDTO {
		e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": title}))
		require.NoError(t, err)
		return &e
	}
	publish := func(e *EntryDTO) {
		_, err := svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
		require.NoError(t, err)
	}

	liveEdited := mk("live-edited")
	publish(liveEdited)
	_, err := svc.UpdateEntry(owner, "post", liveEdited.ID,
		mustJSON(t, map[string]any{"title": "live-edited", "body": "a change nobody has released"}), 0)
	require.NoError(t, err)

	liveClean := mk("live-clean")
	publish(liveClean)

	mk("fresh-draft")

	retracted := mk("retracted")
	publish(retracted)
	_, err = svc.SetEntryStatus(owner, "post", retracted.ID, domain.StatusDraft, 0)
	require.NoError(t, err)

	return owner
}

// TestPendingReviewHoldsExactlyWhatIsNotLive is 驗證計畫第 9 條.
//
// The expected set is WRITTEN OUT, not filtered from the fixture and not read
// back from the queue. An assertion built by asking the query which rows it
// considers pending would agree with any criterion at all, including one that
// returns everything.
func TestPendingReviewHoldsExactlyWhatIsNotLive(t *testing.T) {
	svc, _ := newSvc()
	owner := seedQueueFixture(t, svc)

	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]string{"live-edited", "fresh-draft", "retracted"},
		titlesOf(rows),
		"the queue is everything whose working copy is not what the public sees")

	// The negative half, stated on its own so the reason a row is absent is not
	// left implicit in a set comparison: a published entry nobody has edited is
	// not waiting on anybody.
	assert.NotContains(t, titlesOf(rows), "live-clean",
		"a live entry with no unreleased edits must not appear — that is what "+
			"makes this a queue rather than a list of all content")
}

// TestPendingReviewIncludesDraftsThatWereNeverLive is the mutation target for
// the criterion's second half.
//
// ADR-014 §2 says the new query "need not rewrite what counts as an unpublished
// change" because unpublishedChangesExpr is already an independent predicate.
// Reusing that predicate ALONE is the tempting reading and it is wrong: it opens
// with `status = 'published'`, so it answers false for every draft — including
// the drafts §2's own table puts in the queue. This test is what turns red then;
// TestPendingReviewHoldsExactlyWhatIsNotLive would too, but this one names the
// failure.
func TestPendingReviewIncludesDraftsThatWereNeverLive(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "never released"}))
	require.NoError(t, err)

	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)
	require.Len(t, rows, 1, "a draft is waiting on somebody even though it has no snapshot to differ from")
	assert.Equal(t, "never released", rows[0].Title)
	assert.False(t, rows[0].HasUnpublishedChanges,
		"the flag is false here — which is precisely why it cannot be the queue's criterion")
	assert.False(t, rows[0].HasPublishedSnapshot,
		"never released, so there is nothing to separate it from a retracted entry but this")
}

// TestPendingReviewSeparatesRetractedFromNeverPublished. Both are non-published,
// and merging them tells a reviewer an entry has never been released when in
// fact somebody took it down. Only the snapshot tells them apart, which is why
// HasPublishedSnapshot is on the row at all.
func TestPendingReviewSeparatesRetractedFromNeverPublished(t *testing.T) {
	svc, _ := newSvc()
	owner := seedQueueFixture(t, svc)

	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)

	byTitle := map[string]PendingEntryDTO{}
	for _, r := range rows {
		byTitle[r.Title] = r
	}
	require.Contains(t, byTitle, "retracted")
	require.Contains(t, byTitle, "fresh-draft")

	assert.Equal(t, domain.StatusDraft, byTitle["retracted"].Status)
	assert.True(t, byTitle["retracted"].HasPublishedSnapshot,
		"ADR-014 §5.1 keeps the snapshot through a retract; without it the console "+
			"cannot say this was ever live")
	assert.False(t, byTitle["fresh-draft"].HasPublishedSnapshot)
}

// TestPendingReviewCarriesNoFieldValues.
//
// The queue spans every content type, so it cannot apply the per-type read rule
// or §6's field-level mask — there is no single type to check against. The
// protection is therefore structural: no payload goes out at all. Asserted
// against the MARSHALLED JSON rather than the struct, because a field that never
// reaches the wire is the claim, and a struct assertion would pass on a DTO that
// grows an embedded payload later.
func TestPendingReviewCarriesNoFieldValues(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{
		"title": "a post",
		"body":  "SENTINEL-BODY-VALUE",
	}))
	require.NoError(t, err)

	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)
	require.Len(t, rows, 1)

	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "SENTINEL-BODY-VALUE",
		"the queue says WHICH entries are waiting; the per-entry diff — which has "+
			"both fences on — is what says what changed")
}

// TestPendingReviewTitleSkipsReadRestrictedFields. The label is derived by
// domain.TitleFor, the same one fence the activity stream's denormalised title
// goes through, and it must stay that one implementation: a title sourced from a
// restricted field would copy that value into a response the field's own read
// rule does not govern.
func TestPendingReviewTitleSkipsReadRestrictedFields(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:  "secretive",
		Label: "secretive",
		Fields: []FieldInput{
			{Key: "codename", Type: domain.FieldTypeString, Required: true, ReadRoles: []string{"owner"}},
			{Key: "public_name", Type: domain.FieldTypeString},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "secretive", mustJSON(t, map[string]any{
		"codename":    "RESTRICTED-CODENAME",
		"public_name": "the safe label",
	}))
	require.NoError(t, err)

	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "the safe label", rows[0].Title,
		"the first field is restricted, so the label comes from the next one that is not")
}

// TestPendingReviewPutsAgentWorkFirst is §2's ordering. The queue answers "what
// needs me", and work a person did themselves needs them least.
func TestPendingReviewPutsAgentWorkFirst(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})

	byAgent, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "agent touched"}))
	require.NoError(t, err)
	// Written by a person LAST, so insertion order alone would put it first —
	// otherwise this passes on a queue that does not sort at all.
	byHuman, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "human only"}))
	require.NoError(t, err)
	_ = byHuman

	_, err = svc.UpdateEntry(agent, "post", byAgent.ID,
		mustJSON(t, map[string]any{"title": "agent touched", "body": "written by a bot"}), 0)
	require.NoError(t, err)

	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "agent touched", rows[0].Title, "agent-touched work sorts ahead of a person's own")
	require.NotNil(t, rows[0].ActorKind)
	assert.Equal(t, domain.ActorKindAgent, *rows[0].ActorKind)
	assert.Equal(t, &principal, rows[0].ActorUserID,
		"the row names who ANSWERS for the agent, not the agent alone")
}

// TestAgentCannotReadTheReleaseQueue. Same mechanism as the activity stream and
// the same reason it is the right answer: the queue is the list of what an
// agent's output is waiting on a person to approve, and an agent that could read
// it could watch how long its own work sits unreviewed.
func TestAgentCannotReadTheReleaseQueue(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "waiting"}))
	require.NoError(t, err)

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})
	_, err = svc.ListPendingReview(agent, ListPendingReviewInput{})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err))

	// Control: a person in the same tenant reads it, or the refusal above would
	// also hold for a queue nobody can reach.
	rows, err := svc.ListPendingReview(owner, ListPendingReviewInput{})
	require.NoError(t, err)
	assert.NotEmpty(t, rows)
}

// TestDeliveryCredentialCannotReadTheReleaseQueue. content:list is a read
// action, so the chokepoint's delivery rule would let it past; the explicit
// refusal is what stops it. This response is a list of what is NOT yet public,
// handed to the one audience that lives outside the platform.
func TestDeliveryCredentialCannotReadTheReleaseQueue(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.ListPendingReview(ctxDelivery("t1"), ListPendingReviewInput{})
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))
}

// TestReleaseQueueIsTenantScoped. The fake enforces nothing the database does
// not; this asserts the SERVICE never asks for another tenant's rows — it takes
// the tenant from the subject and there is no parameter to override it.
func TestReleaseQueueIsTenantScoped(t *testing.T) {
	svc, _ := newSvc()
	one := ctxRole("t1", "owner")
	two := ctxRole("t2", "owner")
	seedTitledType(t, svc, one, "post")
	seedTitledType(t, svc, two, "post")

	_, err := svc.CreateEntry(one, "post", mustJSON(t, map[string]any{"title": "tenant one only"}))
	require.NoError(t, err)

	rows, err := svc.ListPendingReview(two, ListPendingReviewInput{})
	require.NoError(t, err)
	assert.Empty(t, rows, "tenant two has nothing waiting; tenant one's draft is not its business")
}
