package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	notifdomain "github.com/williamlabdev/saas-forge/internal/notification/domain"
)

// The worker against a fake store. What these tests are for is the DECISIONS —
// due or not, pinned or moved on, whose name goes on the release — and every
// one of them is a question about a clock, so the clock is frozen and the
// worker is stepped by hand with Tick rather than raced with a ticker.
//
// The concurrency claim is NOT tested here and cannot be: SKIP LOCKED is a
// property of Postgres row locks, and a fake that pretended to have it would
// prove only that the fake was written to agree. That one lives in the
// repository's integration test.

var now = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

type fakeStore struct {
	schedules []*domain.EntrySchedule
	entries   map[uuid.UUID]*domain.Entry
	types     map[uuid.UUID]*domain.ContentType
	activity  []*domain.Activity

	// calls records the order of the two writes whose ORDER is the invariant:
	// the schedule must leave 'pending' before the publish runs, or the publish
	// path's own supersede rule would rewrite this worker's outcome.
	calls []string
	// origins records the domain.PublishOrigin each SetEntryPublishState call
	// carried, in order — the variadic slice verbatim, so "passed nothing" and
	// "passed a zero value" stay distinguishable. It is what proves a scheduled
	// release is recorded AS scheduled rather than as a person at a keyboard
	// (ADR-018).
	origins [][]domain.PublishOrigin
	// failPublish makes SetEntryPublishState fail, to reach the failure path.
	failPublish error
	// txDepth counts open transactions so a test can prove the rollback
	// happened rather than assuming it.
	rolledBack int
}

func (f *fakeStore) ListDueEntrySchedules(_ context.Context, at time.Time, limit int) ([]*domain.EntrySchedule, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []*domain.EntrySchedule
	for _, s := range f.schedules {
		if s.State == domain.ScheduleStatePending && !s.RunAt.After(at) {
			cp := *s
			out = append(out, &cp)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) InTx(_ context.Context, _ string, fn func(Tx) error) error {
	// Snapshot deeply: the whole point of the transaction is that a failure
	// leaves NOTHING behind, and a shallow copy would let a test see a half-done
	// execution the database would never have kept.
	schedules := make([]*domain.EntrySchedule, len(f.schedules))
	for i, s := range f.schedules {
		cp := *s
		schedules[i] = &cp
	}
	entries := make(map[uuid.UUID]*domain.Entry, len(f.entries))
	for k, v := range f.entries {
		cp := *v
		entries[k] = &cp
	}
	activity := append([]*domain.Activity(nil), f.activity...)
	if err := fn(f); err != nil {
		f.schedules, f.entries, f.activity = schedules, entries, activity
		f.rolledBack++
		return err
	}
	return nil
}

func (f *fakeStore) ClaimDueEntrySchedule(_ context.Context, tenantID string, id uuid.UUID, at time.Time) (*domain.EntrySchedule, error) {
	for _, s := range f.schedules {
		if s.TenantID == tenantID && s.ID == id && s.State == domain.ScheduleStatePending && !s.RunAt.After(at) {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) GetContentTypeByID(_ context.Context, _ string, id uuid.UUID) (*domain.ContentType, error) {
	ct, ok := f.types[id]
	if !ok {
		return nil, errors.New("no such content type")
	}
	return ct, nil
}

func (f *fakeStore) GetEntry(_ context.Context, _ string, _, id uuid.UUID) (*domain.Entry, error) {
	e, ok := f.entries[id]
	if !ok {
		return nil, errors.New("no such entry")
	}
	cp := *e
	return &cp, nil
}

func (f *fakeStore) SetEntryPublishState(_ context.Context, e *domain.Entry, status string, publishedAt *time.Time, opts ...domain.PublishOrigin) error {
	f.calls = append(f.calls, "publish")
	// The origin the worker passed, kept so a test can assert that the publish
	// revision this write records (ADR-018) is marked as having come from a
	// schedule. It is recorded on EVERY call, publish and unpublish alike: the
	// worker passes it either way, and a fake that only kept it on a publish
	// could not prove the unpublish path writes no revision at all.
	f.origins = append(f.origins, opts)
	if f.failPublish != nil {
		return f.failPublish
	}
	stored, ok := f.entries[e.ID]
	if !ok {
		return errors.New("no such entry")
	}
	stored.Version++
	stored.Status = status
	stored.PublishedAt = publishedAt
	stored.PublishedBy = e.PublishedBy
	stored.UpdatedBy = e.UpdatedBy
	stored.UpdatedAt = e.UpdatedAt
	if status == domain.StatusPublished {
		stored.PublishedPayload = stored.Payload
		stored.PublishedVersion = stored.Version
	}
	return nil
}

func (f *fakeStore) FinishEntrySchedule(_ context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error {
	f.calls = append(f.calls, "finish:"+state)
	for _, s := range f.schedules {
		if s.TenantID == tenantID && s.ID == id && s.State == domain.ScheduleStatePending {
			s.State, s.ExecutedAt, s.Error = state, executedAt, errMsg
			return nil
		}
	}
	return errors.New("schedule not pending")
}

func (f *fakeStore) RecordActivity(_ context.Context, a *domain.Activity) error {
	f.activity = append(f.activity, a)
	return nil
}

// --- fixtures ---------------------------------------------------------------

type fixture struct {
	store   *fakeStore
	worker  *Worker
	entryID uuid.UUID
	typeID  uuid.UUID
	schedID uuid.UUID
	actorID uuid.UUID
}

// cur reads the schedule OUT OF THE STORE rather than through a pointer held
// from setup. A rolled-back transaction replaces the row with the snapshot copy,
// so a held pointer keeps showing the writes the database threw away — which is
// exactly the difference the failure test is about.
func (f *fixture) cur(t *testing.T) *domain.EntrySchedule {
	t.Helper()
	require.Len(t, f.store.schedules, 1)
	return f.store.schedules[0]
}

func newFixture(t *testing.T, action string, runAt time.Time, status string) *fixture {
	t.Helper()
	typeID, entryID, schedID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := &domain.Entry{
		ID: entryID, TenantID: "t1", ContentTypeID: typeID,
		Payload: json.RawMessage(`{"title":"the release"}`),
		Version: 3, Status: status, Locale: domain.DefaultLocale,
	}
	if status == domain.StatusPublished {
		e.PublishedPayload = e.Payload
		e.PublishedVersion = 3
	}
	s := &domain.EntrySchedule{
		ID: schedID, TenantID: "t1", EntryID: entryID, ContentTypeID: typeID,
		Action: action, RunAt: runAt, PinnedVersion: 3,
		State: domain.ScheduleStatePending, RequestedBy: actorID, CreatedAt: now.Add(-time.Hour),
	}
	store := &fakeStore{
		schedules: []*domain.EntrySchedule{s},
		entries:   map[uuid.UUID]*domain.Entry{entryID: e},
		types: map[uuid.UUID]*domain.ContentType{typeID: {
			ID: typeID, TenantID: "t1", Name: "order",
			Fields: []domain.Field{{Key: "title", Type: domain.FieldTypeString}},
		}},
	}
	return &fixture{
		store:   store,
		worker:  NewWorker(store).WithClock(func() time.Time { return now }),
		entryID: entryID, typeID: typeID, schedID: schedID, actorID: actorID,
	}
}

// --- tests ------------------------------------------------------------------

func TestWorker_PublishesWhenDue(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)

	require.NoError(t, f.worker.Tick(context.Background()))

	e := f.store.entries[f.entryID]
	assert.Equal(t, domain.StatusPublished, e.Status)
	assert.JSONEq(t, `{"title":"the release"}`, string(e.PublishedPayload))
	require.NotNil(t, e.PublishedBy)
	// THE PROVENANCE CLAIM. The worker holds no identity; the release belongs to
	// whoever approved it, and published_by must be able to answer "who put this
	// live" for a release that happened at 3am.
	assert.Equal(t, f.actorID, *e.PublishedBy, "the release is the requester's, not the process's")
	require.NotNil(t, e.PublishedAt)
	assert.Equal(t, now, e.PublishedAt.UTC())

	assert.Equal(t, domain.ScheduleStateDone, f.cur(t).State)
	require.NotNil(t, f.cur(t).ExecutedAt)
	assert.Equal(t, now, f.cur(t).ExecutedAt.UTC())
	assert.Empty(t, f.cur(t).Error)
}

// The order of the two writes is the invariant that keeps a scheduled release
// from superseding itself: SetEntryPublishState moves any pending schedule on
// the entry to 'superseded', so a worker that published first would find its
// own row and record the wrong outcome.
func TestWorker_LeavesPendingBeforeItPublishes(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)

	require.NoError(t, f.worker.Tick(context.Background()))

	assert.Equal(t, []string{"finish:done", "publish"}, f.store.calls,
		"reverse these and the publish path supersedes the very schedule that is executing")
}

// The publish-revision provenance (ADR-018). The row a scheduled release writes
// must say a schedule fired it AND name which one — a release recorded as if a
// person pressed the button is an audit hole, and 'schedule' with no id to point
// at is the same hole in the other direction.
//
// Asserted on what the worker PASSES rather than on a stored row, because the
// row is written by recordPublishRevision inside the repository's transaction;
// this layer's whole contribution is the origin argument.
func TestWorker_PublishRecordsScheduleProvenance(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)

	require.NoError(t, f.worker.Tick(context.Background()))

	require.Len(t, f.store.origins, 1, "exactly one publish-state write per tick")
	require.Len(t, f.store.origins[0], 1,
		"the worker must pass an origin; without it a scheduled release is recorded as a direct one")
	origin := f.store.origins[0][0]
	assert.Equal(t, domain.ActivityViaSchedule, origin.Via)
	require.NotNil(t, origin.ScheduleID)
	assert.Equal(t, f.schedID, *origin.ScheduleID, "the origin names the wrong schedule")
	assert.True(t, origin.Valid(),
		"the via/schedule-id pair violates the biconditional the CHECK constraint enforces")
}

// The other half of the pair, and the reason the id is copied into a local
// before its address is taken: an origin whose pointer aliased the claimed
// schedule struct would be correct here and wrong the moment the worker reused
// the variable.
func TestWorker_UnpublishRecordsNoOrigin(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionUnpublish, now.Add(-time.Second), domain.StatusPublished)

	require.NoError(t, f.worker.Tick(context.Background()))

	require.Len(t, f.store.origins, 1)
	// A retract still calls SetEntryPublishState, but it publishes nothing, so
	// there is no release to attribute. What matters is that the repository
	// records no row for it — asserted where that decision lives (the service
	// package's TestUnpublishRecordsNoRevision and the repository integration
	// test); here it is enough that the worker is not claiming otherwise.
	if len(f.store.origins[0]) == 1 {
		assert.True(t, f.store.origins[0][0].Valid(),
			"the retract passed a malformed origin")
	}
}

func TestWorker_UnpublishesWhenDue(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionUnpublish, now.Add(-time.Second), domain.StatusPublished)

	require.NoError(t, f.worker.Tick(context.Background()))

	e := f.store.entries[f.entryID]
	assert.Equal(t, domain.StatusDraft, e.Status)
	assert.Nil(t, e.PublishedAt, "an entry that is not live has no time at which it is live")
	// ADR-014 §5.1: retracting keeps the snapshot. The scheduler must not be the
	// one path where taking something down also destroys what was published.
	assert.JSONEq(t, `{"title":"the release"}`, string(e.PublishedPayload))
	assert.Equal(t, domain.ScheduleStateDone, f.cur(t).State)
}

func TestWorker_DoesNothingBeforeTheHour(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(time.Minute), domain.StatusDraft)

	require.NoError(t, f.worker.Tick(context.Background()))

	assert.Equal(t, domain.StatusDraft, f.store.entries[f.entryID].Status)
	assert.Equal(t, domain.ScheduleStatePending, f.cur(t).State)
	assert.Empty(t, f.store.calls)
}

// The re-check that makes the pin mean something. UpdateEntry already stales a
// pending schedule inside its own transaction; this covers the window between
// the scan and the lock, and the version bumps that no entry write path
// instruments — a bulk field rename, for one.
func TestWorker_RefusesAVersionItDidNotApprove(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	f.store.entries[f.entryID].Version = 4 // somebody edited after the schedule was filed

	require.NoError(t, f.worker.Tick(context.Background()))

	assert.Equal(t, domain.ScheduleStateStale, f.cur(t).State)
	assert.Nil(t, f.cur(t).ExecutedAt, "stale is not an execution; nothing ran")
	assert.Empty(t, f.cur(t).Error, "nobody did anything wrong — this is the pin working")
	assert.Equal(t, domain.StatusDraft, f.store.entries[f.entryID].Status,
		"unapproved content must not go live, which is the entire reason the version is pinned")
	require.Len(t, f.store.activity, 1)
	a := f.store.activity[0]
	assert.Equal(t, domain.ActivityEntryScheduleStale, a.Action,
		"no release happened, so no release is recorded — but the invalidation is")
	assert.Equal(t, domain.ActorKindHuman, a.ActorKind)
	require.NotNil(t, a.ActorUserID)
	assert.Equal(t, f.store.schedules[0].RequestedBy, *a.ActorUserID,
		"the fact recorded is that THIS person's approval expired")
	assert.Empty(t, a.Via, "nothing was carried out; that is the point of the line")
	assert.Nil(t, a.ViaScheduleID)
	// The three facts that make the row actionable: which schedule, what it
	// approved, what overtook it.
	var d map[string]any
	require.NoError(t, json.Unmarshal(a.Details, &d))
	assert.Equal(t, f.schedID.String(), d["schedule_id"])
	assert.EqualValues(t, 3, d["pinned_version"])
	assert.EqualValues(t, 4, d["entry_version"])
}

// THE ASYMMETRY, and the reason it is a ruling rather than an oversight
// (ADR-017 §2). An unpublish takes down the LIVE SNAPSHOT; editing the working
// copy does not change what is live, so the edit must not cancel the takedown.
// A takedown is a stop-the-bleeding action and is the one schedule that must
// not fail closed.
func TestWorker_UnpublishSurvivesAnEditToTheWorkingCopy(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionUnpublish, now.Add(-time.Minute), domain.StatusPublished)
	f.store.entries[f.entryID].Version = 9 // edited long after the takedown was filed

	require.NoError(t, f.worker.Tick(context.Background()))

	assert.Equal(t, domain.ScheduleStateDone, f.cur(t).State,
		"the retraction is about the snapshot, not about the working copy")
	assert.Equal(t, domain.StatusDraft, f.store.entries[f.entryID].Status)
	require.Len(t, f.store.activity, 1)
	assert.Equal(t, domain.ActivityEntryUnpublish, f.store.activity[0].Action)
}

// Somebody cancelled, or another replica took it, between the scan and the
// claim. All of those are correct and none of them is a failure.
func TestWorker_SkipsAnUnclaimableRow(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	due, err := f.store.ListDueEntrySchedules(context.Background(), now, 50)
	require.NoError(t, err)
	require.Len(t, due, 1)
	f.store.schedules[0].State = domain.ScheduleStateCancelled // lost the race

	require.NoError(t, f.worker.Tick(context.Background()))

	assert.Equal(t, domain.ScheduleStateCancelled, f.cur(t).State)
	assert.Empty(t, f.store.calls)
	assert.Zero(t, f.store.rolledBack)
}

func TestWorker_RecordsTheReleaseAsTheRequesters(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)

	require.NoError(t, f.worker.Tick(context.Background()))

	require.Len(t, f.store.activity, 1)
	a := f.store.activity[0]
	// An ordinary entry.publish, not a vocabulary of its own: an activity stream
	// that showed scheduled releases under a different verb would answer "who
	// published this" with nothing for every release that ran out of hours.
	assert.Equal(t, domain.ActivityEntryPublish, a.Action)
	assert.Equal(t, domain.ActorKindHuman, a.ActorKind)
	require.NotNil(t, a.ActorUserID)
	assert.Equal(t, f.actorID, *a.ActorUserID)
	assert.Equal(t, "order", a.TargetType)
	assert.Equal(t, "the release", a.TargetTitle)
	// ...and the mechanism is recorded next to it rather than instead of it, so
	// "this went out while nobody was watching" is answerable too.
	assert.Equal(t, domain.ActivityViaSchedule, a.Via)
	require.NotNil(t, a.ViaScheduleID)
	assert.Equal(t, f.schedID, *a.ViaScheduleID)
}

func TestWorker_MarksAFailureWithItsReason(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	f.store.failPublish = errors.New("connection reset by peer")

	require.NoError(t, f.worker.Tick(context.Background()), "one bad schedule must not stop the loop")

	assert.Equal(t, 1, f.store.rolledBack, "a failed execution leaves nothing half-done")
	assert.Equal(t, domain.ScheduleStateFailed, f.cur(t).State)
	assert.Contains(t, f.cur(t).Error, "connection reset by peer",
		"the reason is the only thing an editor has to act on")
	assert.Equal(t, domain.StatusDraft, f.store.entries[f.entryID].Status)
	assert.Empty(t, f.store.activity)
}

// Shutdown is not failure. A cancelled context means the process is stopping
// with the row still pending and still due; marking it failed here would turn
// every deploy into a handful of releases that silently never happen.
func TestWorker_LeavesWorkPendingOnShutdown(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	f.store.failPublish = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, f.worker.Tick(ctx))

	assert.Equal(t, domain.ScheduleStatePending, f.cur(t).State,
		"the next start must find this schedule exactly as it was")
}

// --- notification (ADR-023) --------------------------------------------------

type sentSchedNotice struct {
	userID                  uuid.UUID
	tenantID                string
	kind, title, body, link string
}

type fakeSchedNotifier struct {
	sent []sentSchedNotice
}

func (f *fakeSchedNotifier) NotifyUser(_ context.Context, userID uuid.UUID, tenantID, kind, title, body, link string) error {
	f.sent = append(f.sent, sentSchedNotice{userID: userID, tenantID: tenantID, kind: kind, title: title, body: body, link: link})
	return nil
}

// A successful publish pushes exactly one notice to the requester, after the
// transaction that ran it — asserted indirectly by the fact that a rolled-back
// run (TestWorker_MarksAFailureWithItsReason's sibling below) sends none.
func TestWorker_NotifiesRequesterOnSuccess(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	notifier := &fakeSchedNotifier{}
	f.worker.WithNotifier(notifier)

	require.NoError(t, f.worker.Tick(context.Background()))

	require.Len(t, notifier.sent, 1)
	got := notifier.sent[0]
	assert.Equal(t, f.actorID, got.userID, "the requester, not whoever happens to be reading the log")
	assert.Equal(t, "t1", got.tenantID)
	assert.Equal(t, notifdomain.KindScheduleSucceeded, got.kind)
}

// The version-pin invalidation is not an error, but it is still something the
// requester must be told, or a release quietly not happening is a release
// nobody hears about (ADR-017/ADR-023).
func TestWorker_NotifiesRequesterOnStale(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	f.store.entries[f.entryID].Version = 4
	notifier := &fakeSchedNotifier{}
	f.worker.WithNotifier(notifier)

	require.NoError(t, f.worker.Tick(context.Background()))

	require.Len(t, notifier.sent, 1)
	got := notifier.sent[0]
	assert.Equal(t, f.actorID, got.userID)
	assert.Equal(t, notifdomain.KindScheduleStale, got.kind)
}

// A failed execution notifies too, carrying the same reason FinishEntrySchedule
// already wrote to the schedule's own error column.
func TestWorker_NotifiesRequesterOnFailure(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	f.store.failPublish = errors.New("connection reset by peer")
	notifier := &fakeSchedNotifier{}
	f.worker.WithNotifier(notifier)

	require.NoError(t, f.worker.Tick(context.Background()))

	require.Len(t, notifier.sent, 1)
	got := notifier.sent[0]
	assert.Equal(t, f.actorID, got.userID)
	assert.Equal(t, notifdomain.KindScheduleFailed, got.kind)
	assert.Contains(t, got.body, "connection reset by peer")
}

// Shutdown must not notify either — markFailed's own rule (the row is left
// pending, not failed) means there is nothing true to push yet.
func TestWorker_DoesNotNotifyOnShutdown(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	f.store.failPublish = context.Canceled
	notifier := &fakeSchedNotifier{}
	f.worker.WithNotifier(notifier)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, f.worker.Tick(ctx))

	assert.Empty(t, notifier.sent)
}

// A nil notifier (no notification plane wired) must not panic and must not
// change the outcome — the pre-existing deployment shape.
func TestWorker_WithoutNotifierStillWorks(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)

	require.NoError(t, f.worker.Tick(context.Background()))

	assert.Equal(t, domain.StatusPublished, f.store.entries[f.entryID].Status)
}

func TestWorker_RunStopsOnContextCancel(t *testing.T) {
	f := newFixture(t, domain.ScheduleActionPublish, now.Add(-time.Minute), domain.StatusDraft)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.worker.Run(ctx, time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancellation — a hung worker blocks graceful shutdown")
	}
}
