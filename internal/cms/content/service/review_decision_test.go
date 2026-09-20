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
	notifdomain "github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// RequestChanges and ListEntryReviewDecisions, service side (ADR-014
// Amendment: review decisions). See review_decision.go's package comment for
// the "verdict, not a state" design this file's tests hold to.

// seedPendingEntry publishes a fresh entry and then edits it once, the
// shortest path onto the release queue (pendingReviewExpr's live-and-edited
// half) — the same setup TestUpdateEntry_NotifiesOnFirstPendingReviewTransition
// uses, reused here so a review-decision test starts from an entry a reviewer
// could actually be looking at, not a virgin draft.
func seedPendingEntry(t *testing.T, svc ContentService, editorCtx context.Context) EntryDTO {
	t.Helper()
	_, err := svc.CreateContentType(editorCtx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(editorCtx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(editorCtx, "order", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)
	edited, err := svc.UpdateEntry(editorCtx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2", "state": "new"}), published.Version)
	require.NoError(t, err)
	return edited
}

// Happy path exercises every observable effect the spec lists in one place:
// the returned DTO, the activity line (with the reason in its details), the
// notice to the last writer, the entry's exit from the queue, and the
// decision showing up on a direct GetEntry.
func TestRequestChanges_HappyPath(t *testing.T) {
	svc, repo, notifier := newSvcWithNotifier()
	editorID := uuid.New()
	editorCtx := ctxTenantUser("t1", editorID)
	edited := seedPendingEntry(t, svc, editorCtx)

	reviewerID := uuid.New()
	reviewerCtx := ctxTenantUser("t1", reviewerID)
	dec, err := svc.RequestChanges(reviewerCtx, "order", edited.ID, RequestChangesInput{Reason: "please fix the title"}, 0)
	require.NoError(t, err)

	assert.Equal(t, domain.ReviewDecisionChangesRequested, dec.Decision)
	assert.Equal(t, "please fix the title", dec.Reason)
	assert.Equal(t, edited.Version, dec.EntryVersion, "the decision must pin the version the reviewer was looking at")
	assert.Equal(t, reviewerID, dec.DecidedBy)
	assert.False(t, dec.DecidedAt.IsZero())
	require.Len(t, repo.reviewDecisions, 1)

	var recorded *domain.Activity
	for _, a := range repo.activity {
		if a.Action == domain.ActivityEntryReviewChangesRequested {
			recorded = a
		}
	}
	require.NotNil(t, recorded, "sending an entry back is an editorial event and belongs in the stream")
	require.NotNil(t, recorded.TargetEntryID)
	assert.Equal(t, edited.ID, *recorded.TargetEntryID)
	var details map[string]any
	require.NoError(t, json.Unmarshal(recorded.Details, &details))
	assert.Equal(t, "please fix the title", details["reason"], "the reason must be readable off the activity, not just the decision row")

	require.Len(t, notifier.sentUser, 1, "the last writer must hear about it")
	got := notifier.sentUser[0]
	assert.Equal(t, editorID, got.userID)
	assert.Equal(t, "t1", got.tenantID)
	assert.Equal(t, notifdomain.KindEntryChangesRequested, got.kind)
	assert.Contains(t, got.body, "please fix the title", "the author's next move depends on reading the reason, not just the fact of a refusal")
	assert.Contains(t, got.link, edited.ID.String())

	pending, err := svc.ListPendingReview(reviewerCtx, ListPendingReviewInput{})
	require.NoError(t, err)
	for _, p := range pending {
		assert.NotEqual(t, edited.ID, p.ID, "a sent-back entry must leave the queue immediately")
	}

	got2, err := svc.GetEntry(reviewerCtx, "order", edited.ID, GetEntryInput{})
	require.NoError(t, err)
	require.NotNil(t, got2.ReviewDecision, "the decision must be visible on a direct read")
	assert.Equal(t, "please fix the title", got2.ReviewDecision.Reason)
	assert.Equal(t, dec.ID, got2.ReviewDecision.ID)
}

func TestRequestChanges_EmptyReason(t *testing.T) {
	for name, reason := range map[string]string{"empty": "", "blank": "   \t\n"} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newSvc()
			ctx := ctxTenant("t1")
			edited := seedPendingEntry(t, svc, ctx)

			_, err := svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: reason}, 0)
			requireCodeStatus(t, err, "CONTENT_REVIEW_REASON_REQUIRED", 422)
		})
	}
}

func TestRequestChanges_ReasonTooLong(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	edited := seedPendingEntry(t, svc, ctx)

	reason := make([]byte, 2001)
	for i := range reason {
		reason[i] = 'a'
	}
	_, err := svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: string(reason)}, 0)
	requireCodeStatus(t, err, "CONTENT_REVIEW_REASON_TOO_LONG", 422)
}

// An entry that is not in the queue at all cannot be sent back. Publishing
// with nothing since outstanding is the ordinary "nothing to review" state
// every entry sits in between edits.
func TestRequestChanges_NotPending(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID
	_, err := svc.SetEntryStatus(ctx, "order", id, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, err = svc.RequestChanges(ctx, "order", id, RequestChangesInput{Reason: "no"}, 0)
	requireCodeStatus(t, err, "CONTENT_ENTRY_NOT_PENDING", 409)
}

// An entry already sent back, and not yet re-edited, is ALSO not pending —
// the queue rule (reviewDecisionNotSentBackExpr) and this guard are meant to
// agree: a second decision at the same version would have nothing new to
// pin, since the author has not moved the version on.
func TestRequestChanges_AlreadySentBackIsNotPending(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	edited := seedPendingEntry(t, svc, ctx)

	_, err := svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "first"}, 0)
	require.NoError(t, err)

	_, err = svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "again"}, 0)
	requireCodeStatus(t, err, "CONTENT_ENTRY_NOT_PENDING", 409)
}

func TestRequestChanges_VersionConflict(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	edited := seedPendingEntry(t, svc, ctx)

	_, err := svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "no"}, edited.Version-1)
	require.ErrorIs(t, err, repository.ErrVersionConflict,
		"a reviewer must not be able to send back a version they no longer see")
}

// A reviewer who is also the entry's own last writer — a solo tenant catching
// their own mistake — must not be told about their own decision.
func TestRequestChanges_SelfReviewSkipsNotification(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	editorID := uuid.New()
	ctx := ctxTenantUser("t1", editorID)
	edited := seedPendingEntry(t, svc, ctx)

	_, err := svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "self-caught typo"}, 0)
	require.NoError(t, err)
	assert.Empty(t, notifier.sentUser, "a reviewer must not be notified about their own decision")
}

// The core lifecycle property: a decision applies only to the version it was
// made against. The moment the author saves again, the old verdict is
// history and the entry is, once more, somebody's turn to look at.
func TestRequestChanges_DecisionIsStaleAfterTheAuthorEditsAgain(t *testing.T) {
	svc, _ := newSvc()
	editorID := uuid.New()
	editorCtx := ctxTenantUser("t1", editorID)
	edited := seedPendingEntry(t, svc, editorCtx)

	reviewerCtx := ctxTenant("t1")
	_, err := svc.RequestChanges(reviewerCtx, "order", edited.ID, RequestChangesInput{Reason: "fix the title"}, 0)
	require.NoError(t, err)

	pending, err := svc.ListPendingReview(reviewerCtx, ListPendingReviewInput{})
	require.NoError(t, err)
	for _, p := range pending {
		assert.NotEqual(t, edited.ID, p.ID, "sent back — must be out of the queue right after the decision")
	}

	reedited, err := svc.UpdateEntry(editorCtx, "order", edited.ID, mustJSON(t, map[string]any{"title": "T3", "state": "new"}), edited.Version)
	require.NoError(t, err)
	assert.Greater(t, reedited.Version, edited.Version)

	got, err := svc.GetEntry(reviewerCtx, "order", edited.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.Nil(t, got.ReviewDecision, "a decision whose EntryVersion has fallen behind must not read as current")

	pending, err = svc.ListPendingReview(reviewerCtx, ListPendingReviewInput{})
	require.NoError(t, err)
	var back bool
	for _, p := range pending {
		if p.ID == edited.ID {
			back = true
		}
	}
	assert.True(t, back, "re-editing after being sent back must return the entry to the queue with no extra step")
}

// steppingReviewClock guarantees two decisions in the same test get distinct,
// increasing DecidedAt values — the real clock almost always would too, but
// "almost always" is exactly the kind of thing that turns into a once-a-month
// CI flake, and the newest-first ordering this test pins is the one property
// that would silently break if two rows ever landed in the same instant.
func steppingReviewClock(start time.Time) func() time.Time {
	cur := start
	return func() time.Time {
		cur = cur.Add(time.Minute)
		return cur
	}
}

func TestListEntryReviewDecisions_NewestFirst(t *testing.T) {
	svc, _ := newSvc()
	svc.(*contentService).WithClock(steppingReviewClock(schedNow))
	ctx := ctxTenant("t1")
	edited := seedPendingEntry(t, svc, ctx)

	_, err := svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "first"}, 0)
	require.NoError(t, err)
	reedited, err := svc.UpdateEntry(ctx, "order", edited.ID, mustJSON(t, map[string]any{"title": "T3", "state": "new"}), edited.Version)
	require.NoError(t, err)
	_, err = svc.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "second"}, 0)
	require.NoError(t, err)
	_ = reedited

	list, err := svc.ListEntryReviewDecisions(ctx, "order", edited.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "second", list[0].Reason, "newest first")
	assert.Equal(t, "first", list[1].Reason)
}

func TestListEntryReviewDecisions_EmptyWhenNeverSentBack(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID

	list, err := svc.ListEntryReviewDecisions(ctx, "order", id)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// WHICH VERB THIS ASKS FOR, pinned white-box — the same reasoning
// TestScheduleEntry_AsksForTheVerbItIsDeferring gives: sending an entry back
// is the other outcome of the SAME human gate publishing is, so it must be
// refused to exactly the callers /publish refuses, not a wider or narrower
// set. This is also what gives an agent credential its 403 here for free —
// no agent ever holds content:publish (agent_gate.go) — without any
// agent-specific code in this feature at all.
func TestRequestChanges_AsksForContentPublish(t *testing.T) {
	repo := &memRepo{}
	svc := NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	ctx := ctxTenant("t1")
	edited := seedPendingEntry(t, svc, ctx)

	limited := NewContentService(repo, denyVerb{action: authz.ActionContentPublish}, staticPlan(Quota{}))
	_, err := limited.RequestChanges(ctx, "order", edited.ID, RequestChangesInput{Reason: "no"}, 0)
	require.ErrorIs(t, err, apperrors.ErrForbidden)
	assert.Empty(t, repo.reviewDecisions, "a refused request must not leave a decision behind")
}
