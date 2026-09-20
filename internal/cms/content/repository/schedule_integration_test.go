package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// content_entry_schedules against real Postgres (ADR-017, migration 000041).
//
// Everything here is a property of the DATABASE and cannot be shown anywhere
// else:
//
//   - the partial unique index that makes "one pending schedule per entry" an
//     invariant rather than a service-layer convention;
//   - FOR UPDATE SKIP LOCKED, which is the entire multi-replica safety story
//     and has no Go analogue — a fake that implemented it would only prove the
//     fake agrees with itself;
//   - the RLS arrangement for a CROSS-TENANT worker scan, which every other
//     test in this package is exempt from because the superuser role those
//     tests connect as bypasses RLS entirely;
//   - ON DELETE CASCADE from entries.

func schedNow() time.Time { return baseTime.Add(24 * time.Hour) }

func newSchedule(tenant string, entryID, ctID uuid.UUID, action string, runAt time.Time, pinned int) *domain.EntrySchedule {
	return &domain.EntrySchedule{
		ID: uuid.New(), TenantID: tenant, EntryID: entryID, ContentTypeID: ctID,
		Action: action, RunAt: runAt, PinnedVersion: pinned,
		State: domain.ScheduleStatePending, RequestedBy: uuid.New(),
		CreatedAt: baseTime,
	}
}

// scheduleFixture: one tenant, one type, one draft entry at version 1.
func scheduleFixture(t *testing.T, dbName string) (context.Context, *pgxpool.Pool, *PostgresContentRepository, uuid.UUID, uuid.UUID, func() (string, string)) {
	t.Helper()
	ctx, pool, container := startContentDB(t, dbName)
	repo := NewPostgresContentRepository(pool, nil)
	ct := mkType(t, ctx, repo, "t1", "order", [2]string{"title", domain.FieldTypeString})
	ids := seedEntries(t, ctx, pool, "t1", ct.ID, entrySeed{payload: `{"title":"a"}`, version: 1})
	addr := func() (string, string) {
		host, _ := container.Host(ctx)
		port, _ := container.MappedPort(ctx, "5432")
		return host, port.Port()
	}
	return ctx, pool, repo, ct.ID, ids[0], addr
}

// One pending schedule per entry, enforced by uq_content_entry_schedules_pending
// rather than by a read-then-write in Go. The service DOES check first, and that
// check is a better error message — but two requests a millisecond apart both
// pass it, and only the index refuses the second.
func TestSchedule_OnePendingPerEntry(t *testing.T) {
	ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_unique")

	first := newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, first))

	second := newSchedule("t1", entryID, ctID, domain.ScheduleActionUnpublish, schedNow().Add(time.Hour), 1)
	err := repo.CreateEntrySchedule(ctx, second)
	require.ErrorIs(t, err, ErrSchedulePending,
		"the unique violation must arrive as the 409 sentinel, not as a raw pg error the handler renders as 500")

	// ...and the index is PARTIAL: once the first is out of the way, the entry
	// can be scheduled again. A plain unique index would make an entry
	// schedulable exactly once in its life.
	require.NoError(t, repo.FinishEntrySchedule(ctx, "t1", first.ID, domain.ScheduleStateDone, ptrTime(schedNow()), ""))
	require.NoError(t, repo.CreateEntrySchedule(ctx, second))
}

func ptrTime(t time.Time) *time.Time { return &t }

// TWO WORKERS, ONE ROW. This is the whole multi-replica story: the scan is
// unlocked and both replicas see the same candidate, so the claim has to be the
// thing that decides, and SKIP LOCKED is what makes the loser move on instead of
// blocking behind the winner.
func TestSchedule_TwoWorkersCannotClaimTheSameRow(t *testing.T) {
	ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_skiplocked")
	s := newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow().Add(-time.Minute), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, s))

	due, err := repo.ListDueEntrySchedules(ctx, schedNow(), 10)
	require.NoError(t, err)
	require.Len(t, due, 1, "the scan must offer the row to both replicas — that is why the claim has to arbitrate")

	locked := make(chan struct{})
	loserDone := make(chan struct{})
	var loser *domain.EntrySchedule
	var loserErr error

	go func() {
		defer close(loserDone)
		loserErr = repo.WithTx(ctx, "t1", func(r ContentRepository) error {
			<-locked
			var err error
			loser, err = r.ClaimDueEntrySchedule(ctx, "t1", s.ID, schedNow())
			return err
		})
	}()

	var winner *domain.EntrySchedule
	require.NoError(t, repo.WithTx(ctx, "t1", func(r ContentRepository) error {
		var err error
		winner, err = r.ClaimDueEntrySchedule(ctx, "t1", s.ID, schedNow())
		if err != nil {
			return err
		}
		// The lock is held for the length of THIS transaction. Releasing it
		// before the second attempt would make the test pass for the wrong
		// reason — the two claims would simply be sequential.
		close(locked)
		select {
		case <-loserDone:
		case <-time.After(10 * time.Second):
			t.Error("the second claim blocked instead of skipping — SKIP LOCKED is missing")
		}
		return nil
	}))

	require.NoError(t, loserErr)
	require.NotNil(t, winner, "one replica must get the row")
	assert.Nil(t, loser, "the other must get nothing and move on, not wait and then double-publish")
}

// The claim is a LOCK, not a column, so a worker that dies mid-flight leaves the
// row exactly as it found it. That is why there is no `running` state and no
// reclaim job — the outbox needs both, and this table needs neither.
func TestSchedule_ARolledBackClaimStaysPending(t *testing.T) {
	ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_rollback")
	s := newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow().Add(-time.Minute), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, s))

	boom := assert.AnError
	err := repo.WithTx(ctx, "t1", func(r ContentRepository) error {
		claimed, err := r.ClaimDueEntrySchedule(ctx, "t1", s.ID, schedNow())
		require.NoError(t, err)
		require.NotNil(t, claimed)
		require.NoError(t, r.FinishEntrySchedule(ctx, "t1", s.ID, domain.ScheduleStateDone, ptrTime(schedNow()), ""))
		return boom
	})
	require.ErrorIs(t, err, boom)

	after, err := repo.GetPendingEntrySchedule(ctx, "t1", entryID)
	require.NoError(t, err)
	require.NotNil(t, after, "a crashed execution must leave work to do, not a row marked done")
	assert.Equal(t, domain.ScheduleStatePending, after.State)
}

// THE CROSS-TENANT SCAN UNDER REAL RLS.
//
// Every other integration test in this package connects as the superuser, which
// bypasses RLS outright — so a policy mistake on this table would pass all of
// them and then return zero rows in production, and scheduled publishing would
// simply never fire with nothing in the logs. The worker's scan therefore has to
// be proved against a NOSUPERUSER role, which is what production uses.
func TestSchedule_WorkerScanCrossesTenantsOnlyWithItsGUC(t *testing.T) {
	ctx, pool, repo, ctID, entryID, addr := scheduleFixture(t, "sched_rls")

	otherType := mkType(t, ctx, repo, "t2", "order", [2]string{"title", domain.FieldTypeString})
	otherEntry := seedEntries(t, ctx, pool, "t2", otherType.ID, entrySeed{payload: `{"title":"theirs"}`, version: 1})[0]
	require.NoError(t, repo.CreateEntrySchedule(ctx,
		newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow().Add(-time.Minute), 1)))
	require.NoError(t, repo.CreateEntrySchedule(ctx,
		newSchedule("t2", otherEntry, otherType.ID, domain.ScheduleActionPublish, schedNow().Add(-time.Minute), 1)))

	if _, err := pool.Exec(ctx, `
		CREATE ROLE schedapp LOGIN PASSWORD 'schedpw' NOSUPERUSER;
		GRANT USAGE ON SCHEMA public TO schedapp;
		GRANT SELECT, INSERT, UPDATE, DELETE ON
			content_types, content_type_fields, entries, content_entry_schedules TO schedapp;
	`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	host, port := addr()
	app, err := pgxpool.New(ctx, "postgres://schedapp:schedpw@"+host+":"+port+"/sched_rls?sslmode=disable")
	require.NoError(t, err)
	defer app.Close()

	// Without the GUC, a cross-tenant SELECT sees NOTHING. app_current_tenant()
	// is NULL when app.tenant_id is unset, so the ordinary tenant policy fails
	// closed — which is correct, and is exactly why the worker cannot just run
	// the query.
	var bare int
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM content_entry_schedules`).Scan(&bare))
	_ = tx.Rollback(ctx)
	assert.Zero(t, bare, "an unscoped read must see nothing; if this is ever non-zero the tenant policy is gone")

	// With it, and only with it, the scan sees every tenant's due rows.
	appRepo := NewPostgresContentRepository(app, nil)
	due, err := appRepo.ListDueEntrySchedules(ctx, schedNow(), 50)
	require.NoError(t, err)
	assert.Len(t, due, 2, "the worker serves every tenant; a scan that sees one is a scheduler that never fires for the rest")

	// The escape hatch is SELECT-only and reaches ONE table. It carries no
	// content — an entry id and a timestamp — and the claim that follows runs
	// under ordinary tenant scoping, so nothing here can read or write a
	// tenant's payload.
	tx, err = app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.scheduler_scan', 'on', true)`)
	require.NoError(t, err)
	var entriesSeen int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&entriesSeen))
	assert.Zero(t, entriesSeen, "the scanner GUC must not open anything but the schedule table")
	_, err = tx.Exec(ctx, `UPDATE content_entry_schedules SET state = 'cancelled'`)
	require.NoError(t, err, "an UPDATE is allowed to run")
	var cancelled int
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM content_entry_schedules WHERE state = 'cancelled'`).Scan(&cancelled))
	assert.Zero(t, cancelled, "...but with no tenant set it must match no rows: the scan policy is SELECT-only")
	_ = tx.Rollback(ctx)
}

// Deleting an entry takes its schedules with it. Without the cascade the worker
// would wake up holding a pointer to a row that no longer exists and record a
// failure for something nobody did wrong.
func TestSchedule_CascadesWithTheEntry(t *testing.T) {
	ctx, pool, repo, ctID, entryID, _ := scheduleFixture(t, "sched_cascade")
	require.NoError(t, repo.CreateEntrySchedule(ctx,
		newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)))

	_, err := pool.Exec(ctx, `DELETE FROM entries WHERE id = $1`, entryID)
	require.NoError(t, err)

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM content_entry_schedules WHERE entry_id = $1`, entryID).Scan(&n))
	assert.Zero(t, n)
}

// The two transitions that OTHER writes make, in the SQL that makes them. Both
// run inside the caller's transaction, so an entry write and the invalidation of
// its schedule are one atomic fact rather than two that can come apart.
func TestSchedule_InvalidatedByEntryWrites(t *testing.T) {
	t.Run("updating the working copy makes it stale", func(t *testing.T) {
		ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_stale")
		require.NoError(t, repo.CreateEntrySchedule(ctx,
			newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)))

		e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
		require.NoError(t, err)
		e.Payload = json.RawMessage(`{"title":"edited"}`)
		e.UpdatedAt = schedNow()
		require.NoError(t, repo.UpdateEntry(ctx, e))

		got, err := repo.GetLatestEntrySchedule(ctx, "t1", entryID)
		require.NoError(t, err)
		assert.Equal(t, domain.ScheduleStateStale, got.State,
			"the content moved past what was approved; publishing it on Friday would release something nobody signed off")
	})

	// The asymmetry, in the SQL that implements it (ADR-017 §2). An unpublish
	// retracts the LIVE SNAPSHOT; the working copy is not the snapshot, so an
	// edit says nothing about whether the takedown should still happen. Drop the
	// `action = 'publish'` predicate from markSchedulesStale and this test fails
	// — which is the point: a page booked to come down at midnight must not stay
	// up because somebody fixed a typo at 23:50.
	t.Run("updating the working copy leaves an unpublish alone", func(t *testing.T) {
		ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_unpub_survives")
		require.NoError(t, repo.CreateEntrySchedule(ctx,
			newSchedule("t1", entryID, ctID, domain.ScheduleActionUnpublish, schedNow(), 1)))

		e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
		require.NoError(t, err)
		e.Payload = json.RawMessage(`{"title":"edited"}`)
		e.UpdatedAt = schedNow()
		require.NoError(t, repo.UpdateEntry(ctx, e))

		got, err := repo.GetLatestEntrySchedule(ctx, "t1", entryID)
		require.NoError(t, err)
		assert.Equal(t, domain.ScheduleStatePending, got.State)

		act, err := repo.ListActivity(ctx, ActivityFilter{TenantID: "t1", EntryID: &entryID})
		require.NoError(t, err)
		for _, a := range act {
			assert.NotEqual(t, domain.ActivityEntryScheduleStale, a.Action,
				"nothing was invalidated, so nothing may claim it was")
		}
	})

	t.Run("publishing by hand supersedes it", func(t *testing.T) {
		ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_superseded")
		require.NoError(t, repo.CreateEntrySchedule(ctx,
			newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)))

		e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
		require.NoError(t, err)
		e.UpdatedAt = schedNow()
		at := schedNow()
		require.NoError(t, repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &at))

		got, err := repo.GetLatestEntrySchedule(ctx, "t1", entryID)
		require.NoError(t, err)
		assert.Equal(t, domain.ScheduleStateSuperseded, got.State)
	})
}

func TestSchedule_GetLatestIsNotFoundWhenThereIsNone(t *testing.T) {
	ctx, _, repo, _, entryID, _ := scheduleFixture(t, "sched_none")

	_, err := repo.GetLatestEntrySchedule(ctx, "t1", entryID)
	require.ErrorIs(t, err, apperrors.ErrNotFound)

	pending, err := repo.GetPendingEntrySchedule(ctx, "t1", entryID)
	require.NoError(t, err, "the decoration path asks this on every entry read; absence is not an error there")
	assert.Nil(t, pending)
}

// The invalidation is written down, IN THE SAME TRANSACTION as the edit that
// caused it — a rolled-back update cannot leave a row claiming a release was
// invalidated. That forces the stale line to be built by jsonb_build_object in
// SQL while the worker's identical line is built by domain.ScheduleStaleDetails
// in Go, and two spellings of the same fact drift. This test is the seam: it
// compares the keys the database actually wrote against the keys the Go helper
// produces, so a rename on either side is a red test rather than a dashboard
// that silently stops finding half the invalidations.
func TestSchedule_StaleActivityKeysMatchTheGoSpelling(t *testing.T) {
	ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_stale_activity")
	sched := newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, sched))

	e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
	require.NoError(t, err)
	e.Payload = json.RawMessage(`{"title":"edited"}`)
	e.UpdatedAt = schedNow()
	require.NoError(t, repo.UpdateEntry(ctx, e))

	act, err := repo.ListActivity(ctx, ActivityFilter{TenantID: "t1", EntryID: &entryID})
	require.NoError(t, err)
	var stale *domain.Activity
	for _, a := range act {
		if a.Action == domain.ActivityEntryScheduleStale {
			stale = a
		}
	}
	require.NotNil(t, stale, "an editor whose release quietly stopped being a release needs to find out somewhere")
	assert.Equal(t, "order", stale.TargetType)
	require.NotNil(t, stale.TargetEntryID)
	assert.Equal(t, entryID, *stale.TargetEntryID)
	assert.Empty(t, stale.Via, "nothing was carried out through a schedule; the schedule is what was lost")
	assert.Nil(t, stale.ViaScheduleID)

	fromSQL := map[string]any{}
	require.NoError(t, json.Unmarshal(stale.Details, &fromSQL))
	fromGo := map[string]any{}
	require.NoError(t, json.Unmarshal(domain.ScheduleStaleDetails(sched.ID, 1, e.Version), &fromGo))
	assert.Equal(t, fromGo, fromSQL,
		"the worker writes this line in Go and UpdateEntry writes it in SQL; they have to be the same line")
	assert.EqualValues(t, 1, fromSQL["pinned_version"])
	assert.EqualValues(t, 2, fromSQL["entry_version"], "the edit that overtook the approval")
}

// The read behind the admin DTO's `schedule` field. Pending AND stale, newest
// first: stale is an actionable state — somebody has to re-file the release —
// and a field that vanished the moment the schedule broke would make the
// release calendar lie by omission at the worst possible moment.
func TestSchedule_ActionableReadCoversPendingAndStale(t *testing.T) {
	ctx, _, repo, ctID, entryID, _ := scheduleFixture(t, "sched_actionable")

	got, err := repo.GetActionableEntrySchedule(ctx, "t1", entryID)
	require.NoError(t, err, "absence is not an error; every entry read asks this")
	assert.Nil(t, got)

	first := newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, first))
	got, err = repo.GetActionableEntrySchedule(ctx, "t1", entryID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.ScheduleStatePending, got.State)

	// Terminal states drop out — that is what makes the field mean "outstanding"
	// rather than "here is some history".
	require.NoError(t, repo.FinishEntrySchedule(ctx, "t1", first.ID, domain.ScheduleStateCancelled, nil, ""))
	got, err = repo.GetActionableEntrySchedule(ctx, "t1", entryID)
	require.NoError(t, err)
	assert.Nil(t, got)

	// Stale does not. It is a to-do: somebody has to look at the edit and
	// re-file the release, and a schedule that disappeared from the entry the
	// moment it broke would make the release calendar lie by omission at exactly
	// the moment it matters.
	second := newSchedule("t1", entryID, ctID, domain.ScheduleActionPublish, schedNow(), 1)
	second.CreatedAt = baseTime.Add(time.Hour)
	require.NoError(t, repo.CreateEntrySchedule(ctx, second))

	e, err := repo.GetEntry(ctx, "t1", ctID, entryID)
	require.NoError(t, err)
	e.Payload = json.RawMessage(`{"title":"edited"}`)
	e.UpdatedAt = schedNow()
	require.NoError(t, repo.UpdateEntry(ctx, e))

	got, err = repo.GetActionableEntrySchedule(ctx, "t1", entryID)
	require.NoError(t, err)
	require.NotNil(t, got, "stale is a to-do, not a tombstone")
	assert.Equal(t, domain.ScheduleStateStale, got.State)
	assert.Equal(t, second.ID, got.ID, "newest first: a re-filed schedule shadows the one it replaced")
}

// TestSchedule_ListActionableMatchesTheSingleRowRead pins that the batched
// list read (the one ListEntries now uses to decorate a whole page in one
// round trip) picks the SAME row per entry that GetActionableEntrySchedule
// would — pending-or-stale, newest first — for every id in the slice at once,
// and simply omits an id with nothing outstanding rather than returning a nil
// entry for it.
func TestSchedule_ListActionableMatchesTheSingleRowRead(t *testing.T) {
	ctx, pool, repo, ctID, _, _ := scheduleFixture(t, "sched_actionable_list")
	// A second and third entry in the same tenant/type, alongside the fixture's own.
	ids := seedEntries(t, ctx, pool, "t1", ctID,
		entrySeed{payload: `{"title":"b"}`, version: 1},
		entrySeed{payload: `{"title":"c"}`, version: 1})
	pending := ids[0]
	staleEntry := ids[1]
	plain := uuid.New() // never scheduled, and not even a real entry id

	pendingSched := newSchedule("t1", pending, ctID, domain.ScheduleActionPublish, schedNow(), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, pendingSched))

	// The lone schedule flips pending -> stale the moment the working copy
	// moves past the version it was pinned to (TestSchedule_InvalidatedByEntryWrites
	// shows this needs no second, cancelled row) — the same transition the
	// single-row TestSchedule_ActionableReadCoversPendingAndStale exercises.
	staleSched := newSchedule("t1", staleEntry, ctID, domain.ScheduleActionPublish, schedNow(), 1)
	require.NoError(t, repo.CreateEntrySchedule(ctx, staleSched))
	e, err := repo.GetEntry(ctx, "t1", ctID, staleEntry)
	require.NoError(t, err)
	e.Payload = json.RawMessage(`{"title":"edited"}`)
	e.UpdatedAt = schedNow()
	require.NoError(t, repo.UpdateEntry(ctx, e))

	got, err := repo.ListActionableEntrySchedules(ctx, "t1", []uuid.UUID{pending, staleEntry, plain})
	require.NoError(t, err)
	require.Len(t, got, 2, "plain has nothing outstanding and must be absent, not present-with-nil")

	require.Contains(t, got, pending)
	assert.Equal(t, domain.ScheduleStatePending, got[pending].State)
	assert.Equal(t, pendingSched.ID, got[pending].ID)

	require.Contains(t, got, staleEntry)
	assert.Equal(t, domain.ScheduleStateStale, got[staleEntry].State)
	assert.Equal(t, staleSched.ID, got[staleEntry].ID, "the batched read must pick the same row the single-row method does")

	assert.NotContains(t, got, plain)

	empty, err := repo.ListActionableEntrySchedules(ctx, "t1", nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "an empty page of entries is the ordinary shape, same as GetEntriesByIDs — not a query to send")
}
