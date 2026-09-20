package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	notifdomain "github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// ADR-023's second trigger point: an edit to a PUBLISHED entry that leaves it
// with an unpublished draft is the moment owner/admin need to hear about,
// because that is the moment the release queue (ADR-014 §2) gained a row
// nobody was told about. See content_service.go's UpdateEntry (the
// wasPending/nowPending comparison) and proposal_notify.go's
// notifyEntryPendingReview.

func ctxTenantUser(tenant string, id uuid.UUID) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:   id,
		TenantID: tenant,
		Roles:    []string{"member"},
	})
}

// The FIRST save that turns a published entry's working copy into a draft
// pushes exactly one notice, excluding the editor who made it.
func TestUpdateEntry_NotifiesOnFirstPendingReviewTransition(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	editorID := uuid.New()
	ctx := ctxTenantUser("tenant-a", editorID)

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(ctx, "order", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)
	require.False(t, published.HasUnpublishedChanges, "freshly published, nothing pending yet")
	require.Empty(t, notifier.sent, "publishing itself is not a pending-review trigger")

	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2"}), published.Version)
	require.NoError(t, err)

	require.Len(t, notifier.sent, 1, "the first edit after publish must push exactly one notice")
	got := notifier.sent[0]
	assert.Equal(t, "tenant-a", got.tenantID)
	assert.ElementsMatch(t, []string{"owner", "admin"}, got.roles)
	assert.Equal(t, editorID, got.exclude, "the editor who made the save is excluded, not notified about their own edit")
	assert.Equal(t, notifdomain.KindEntryPendingReview, got.kind)
}

// Every subsequent save while the entry is STILL pending must not re-notify —
// debounced by the state transition itself (ADR-023), not by a timer.
func TestUpdateEntry_DoesNotRenotifyWhileStillPending(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	ctx := ctxTenant("tenant-a")

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(ctx, "order", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)

	first, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2"}), published.Version)
	require.NoError(t, err)
	require.Len(t, notifier.sent, 1)

	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T3"}), first.Version)
	require.NoError(t, err)

	assert.Len(t, notifier.sent, 1, "still pending after the second save — must not notify again")
}

// A draft entry that was never published has no "unpublished changes" to
// speak of (IsPublished() is false), so ordinary drafting work stays silent.
func TestUpdateEntry_DraftEntryNeverNotifies(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	ctx := ctxTenant("tenant-a")

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)

	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2"}), created.Version)
	require.NoError(t, err)

	assert.Empty(t, notifier.sent, "a draft that was never published has no review queue row to announce")
}

// ADR-014 Amendment: review decisions' own correction to wasPending. Without
// it, an entry sent back and then re-edited would read as wasPending==true
// both before and after the save — it never actually left HasUnpublishedChanges
// — and notifyEntryPendingReview's FALSE->TRUE gate would never re-fire, so
// owner/admin would never hear that the entry is, once again, somebody's turn
// to look at. See content_service.go's UpdateEntry, the block right after
// wasPending is computed.
func TestUpdateEntry_RenotifiesAfterBeingSentBackAndReEdited(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	ctx := ctxTenant("tenant-a")

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(ctx, "order", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)

	edited, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2"}), published.Version)
	require.NoError(t, err)
	require.Len(t, notifier.sent, 1, "first transition into the queue")

	_, err = svc.RequestChanges(ctx, "order", created.ID, RequestChangesInput{Reason: "fix the title"}, 0)
	require.NoError(t, err)

	// RequestChanges does not touch entries.version, so edited.Version is
	// still current — this save is exactly "the author acts on the decision",
	// the re-entry the queue rule and this hook must both recognise.
	reedited, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T3"}), edited.Version)
	require.NoError(t, err)
	assert.Len(t, notifier.sent, 2, "re-editing after a sent-back decision must notify again — the buggy wasPending derivation would leave this at 1")
	got := notifier.sent[len(notifier.sent)-1]
	assert.Equal(t, notifdomain.KindEntryPendingReview, got.kind)
	_ = reedited
}

// Publishing again resets HasUnpublishedChanges to false, so a LATER edit
// re-opens the false->true transition and notifies once more.
func TestUpdateEntry_RenotifiesAfterAFreshPublish(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	ctx := ctxTenant("tenant-a")

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(ctx, "order", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)

	edited, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2"}), published.Version)
	require.NoError(t, err)
	require.Len(t, notifier.sent, 1)

	republished, err := svc.SetEntryStatus(ctx, "order", created.ID, domain.StatusPublished, edited.Version)
	require.NoError(t, err)
	require.False(t, republished.HasUnpublishedChanges)

	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T3"}), republished.Version)
	require.NoError(t, err)

	assert.Len(t, notifier.sent, 2, "publishing cleared the pending state, so the next edit is a new transition")
}

// Same reasoning as TestProposalSurvivesANotifierFailure: the entry write is
// committed before the notice is attempted, so a notification-plane outage
// must not turn a landed edit into an error the editor reads as "not saved".
func TestUpdateEntry_SurvivesANotifierFailure(t *testing.T) {
	svc, _, notifier := newSvcWithNotifier()
	notifier.err = errors.New("notification plane is down")
	editorID := uuid.New()
	ctx := ctxTenantUser("tenant-a", editorID)

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "state": "new"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(ctx, "order", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)

	updated, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"title": "T2"}), published.Version)
	require.NoError(t, err, "a failed notification must not fail the entry update")

	// The write landed with the correct resulting state, not merely a green
	// error value: the version advanced past what was passed in, and the
	// pending-review transition this same edit triggers is still recorded.
	assert.Greater(t, updated.Version, published.Version)
	assert.True(t, updated.HasUnpublishedChanges)

	// The notifier was still called (and failed) — this is not a case where
	// the failure was silently skipped, only one where it was not allowed to
	// propagate.
	require.Len(t, notifier.sent, 1)
}
