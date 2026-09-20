package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// entry_review_decisions against real Postgres (ADR-014 Amendment: review
// decisions, migration 000051).
//
// Two things here have no Go analogue and so cannot be proved by a memRepo
// test alone:
//
//   - reviewDecisionNotSentBackExpr's correlated subquery, which is what
//     actually removes a sent-back entry from ListPendingReview — the
//     service-level fake reimplements the SAME RULE in Go, which proves the
//     fake agrees with itself, not that the SQL does;
//   - ListLatestEntryReviewDecisions' DISTINCT ON batch read, which has to
//     pick the same "newest" row per entry that the single-row
//     GetLatestEntryReviewDecision would, for every id in a page at once and
//     without an N+1.
func decisionFixture(t *testing.T, dbName string) (context.Context, *PostgresContentRepository, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx, pool, _ := startContentDB(t, dbName)
	repo := NewPostgresContentRepository(pool, nil)
	ct := mkType(t, ctx, repo, "t1", "order", [2]string{"title", domain.FieldTypeString})
	ids := seedEntries(t, ctx, pool, "t1", ct.ID,
		entrySeed{payload: `{"title":"edited"}`, version: 2, publishedPayload: `{"title":"a"}`, publishedVersion: 1})
	return ctx, repo, ct.ID, ids[0]
}

func mkDecision(tenant string, entryID uuid.UUID, entryVersion int, decidedAt time.Time) *domain.EntryReviewDecision {
	return &domain.EntryReviewDecision{
		ID: uuid.New(), TenantID: tenant, EntryID: entryID,
		Decision: domain.ReviewDecisionChangesRequested, Reason: "fix the title",
		EntryVersion: entryVersion, DecidedBy: uuid.New(), DecidedAt: decidedAt,
	}
}

// The queue rule, end to end: sent-back-and-not-yet-re-edited leaves
// ListPendingReview, and a later save (which bumps entries.version) brings it
// straight back with no code path that has to notice and clear anything.
func TestReviewDecision_SentBackLeavesTheQueueUntilReEdited(t *testing.T) {
	ctx, repo, ctID, entryID := decisionFixture(t, "revdec_queue")

	rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the fixture entry has unpublished changes, so it starts in the queue")
	assert.Equal(t, entryID, rows[0].ID)

	// entry_version == the entry's CURRENT version (2): sent back, not yet
	// re-edited.
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, mkDecision("t1", entryID, 2, baseTime)))

	rows, err = repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)
	assert.Empty(t, rows, "a decision made against the entry's current version must pull it out of the queue")

	// The author saves again — the one write that matters here is
	// entries.version moving past what the decision named, which is exactly
	// what UpdateEntry does; a direct version bump stands in for it since this
	// file's fixture writes bypass the service layer everywhere else too.
	e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
	require.NoError(t, err)
	e.Payload = []byte(`{"title":"edited again"}`)
	e.UpdatedAt = baseTime.Add(time.Hour)
	require.NoError(t, repo.UpdateEntry(ctx, e))
	require.Equal(t, 3, e.Version, "UpdateEntry bumps the version it just wrote back onto e")

	rows, err = repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the decision now names a stale version, so the entry is the reviewer's business again")
	assert.Equal(t, entryID, rows[0].ID)
}

// A STALE decision — one whose entry_version no longer matches — must not
// hide the entry, even though it is still that entry's newest row. The
// comparison in reviewDecisionNotSentBackExpr has to be against the CURRENT
// version, not merely "does a decision exist".
func TestReviewDecision_StaleDecisionDoesNotHideTheEntry(t *testing.T) {
	ctx, repo, _, entryID := decisionFixture(t, "revdec_stale")

	// Decided against version 1 — the entry is already at version 2, so this
	// decision is stale from the moment it is written.
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, mkDecision("t1", entryID, 1, baseTime)))

	rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "a decision that named an earlier version is history, not a hold")
	assert.Equal(t, entryID, rows[0].ID)
}

// Two decisions on the same entry: the NEWER one is what the queue rule
// looks at, so sending it back a second time (against the version now
// current) must re-hide it even though an older, stale decision also exists.
func TestReviewDecision_QueueRuleUsesTheNewestDecisionOnly(t *testing.T) {
	ctx, repo, ctID, entryID := decisionFixture(t, "revdec_newest")

	// First decision, against version 2 — immediately superseded by the
	// author's own save, so it is stale by the time the second is written.
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, mkDecision("t1", entryID, 2, baseTime)))

	e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
	require.NoError(t, err)
	e.Payload = []byte(`{"title":"round two"}`)
	e.UpdatedAt = baseTime.Add(time.Hour)
	require.NoError(t, repo.UpdateEntry(ctx, e))
	require.Equal(t, 3, e.Version)

	rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the first decision is stale after the re-edit")

	// Second decision, against the now-current version 3.
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, mkDecision("t1", entryID, 3, baseTime.Add(2*time.Hour))))

	rows, err = repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)
	assert.Empty(t, rows, "the newest decision names the entry's current version — sent back again")
}

// GetLatestEntryReviewDecision picks the newest row by decided_at (ties
// broken by id), not insertion order — the two decisions here are written
// out of chronological order to prove the ORDER BY is doing the work rather
// than "last inserted".
func TestReviewDecision_GetLatestPicksNewestByDecidedAt(t *testing.T) {
	ctx, repo, _, entryID := decisionFixture(t, "revdec_latest_single")

	newer := mkDecision("t1", entryID, 2, baseTime.Add(time.Hour))
	older := mkDecision("t1", entryID, 2, baseTime)
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, newer))
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, older))

	got, err := repo.GetLatestEntryReviewDecision(ctx, "t1", entryID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, newer.ID, got.ID, "decided_at DESC must win over insertion order")
}

func TestReviewDecision_GetLatestIsNilWhenNeverSentBack(t *testing.T) {
	ctx, repo, _, entryID := decisionFixture(t, "revdec_latest_none")

	got, err := repo.GetLatestEntryReviewDecision(ctx, "t1", entryID)
	require.NoError(t, err)
	assert.Nil(t, got, "no decision at all is the ordinary state of almost every entry, not an error")
}

// The batched read behind EntryDTO's decoration (mirrors
// ListActionableEntrySchedules for schedules): ONE query answers for a whole
// page of entries, picks the same row GetLatestEntryReviewDecision would pick
// for each, and simply omits an id with nothing to say rather than returning
// a nil entry for it.
func TestReviewDecision_ListLatestMatchesTheSingleRowReadAndOmitsAbsent(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "revdec_list_latest")
	repo := NewPostgresContentRepository(pool, nil)
	ct := mkType(t, ctx, repo, "t1", "order", [2]string{"title", domain.FieldTypeString})
	ids := seedEntries(t, ctx, pool, "t1", ct.ID,
		entrySeed{payload: `{"title":"a"}`, version: 2, publishedPayload: `{"title":"a0"}`, publishedVersion: 1},
		entrySeed{payload: `{"title":"b"}`, version: 5, publishedPayload: `{"title":"b0"}`, publishedVersion: 1},
		entrySeed{payload: `{"title":"c"}`, version: 1})
	sentBack, hasHistory, plain := ids[0], ids[1], ids[2]
	neverSeeded := uuid.New() // not even a real entry — must simply be absent, not present-with-nil

	require.NoError(t, repo.CreateEntryReviewDecision(ctx, mkDecision("t1", sentBack, 2, baseTime)))

	older := mkDecision("t1", hasHistory, 3, baseTime)
	newer := mkDecision("t1", hasHistory, 5, baseTime.Add(time.Hour))
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, older))
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, newer))

	got, err := repo.ListLatestEntryReviewDecisions(ctx, "t1", []uuid.UUID{sentBack, hasHistory, plain, neverSeeded})
	require.NoError(t, err)
	require.Len(t, got, 2, "plain and neverSeeded have nothing outstanding and must be absent, not present-with-nil")

	require.Contains(t, got, sentBack)
	assert.Equal(t, 2, got[sentBack].EntryVersion)

	require.Contains(t, got, hasHistory)
	assert.Equal(t, newer.ID, got[hasHistory].ID, "the batched read must pick the same row GetLatestEntryReviewDecision would")

	single, err := repo.GetLatestEntryReviewDecision(ctx, "t1", hasHistory)
	require.NoError(t, err)
	require.NotNil(t, single)
	assert.Equal(t, single.ID, got[hasHistory].ID, "batch and single-row reads must agree on which row is newest")

	assert.NotContains(t, got, plain)
	assert.NotContains(t, got, neverSeeded)

	empty, err := repo.ListLatestEntryReviewDecisions(ctx, "t1", nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "an empty page of entries is the ordinary shape, not a query to send")
}

// ListEntryReviewDecisions is the history read behind GET
// .../review-decisions: newest first, and every decision the entry has ever
// collected — unlike the queue rule, a stale decision does not drop out of
// this list.
func TestReviewDecision_ListHistoryIsNewestFirstAndIncludesStaleRows(t *testing.T) {
	ctx, repo, _, entryID := decisionFixture(t, "revdec_history")

	first := mkDecision("t1", entryID, 2, baseTime)
	second := mkDecision("t1", entryID, 2, baseTime.Add(time.Hour))
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, first))
	require.NoError(t, repo.CreateEntryReviewDecision(ctx, second))

	list, err := repo.ListEntryReviewDecisions(ctx, "t1", entryID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, second.ID, list[0].ID, "newest first")
	assert.Equal(t, first.ID, list[1].ID)
}

func TestReviewDecision_ListHistoryEmptyWhenNeverSentBack(t *testing.T) {
	ctx, repo, _, entryID := decisionFixture(t, "revdec_history_none")

	list, err := repo.ListEntryReviewDecisions(ctx, "t1", entryID)
	require.NoError(t, err)
	assert.Empty(t, list)
}
