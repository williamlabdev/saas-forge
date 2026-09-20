package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// --- scheduled publish / unpublish (ADR-017) ---------------------------------

// ErrSchedulePending is returned by CreateEntrySchedule when the entry already
// has a pending schedule.
//
// It exists as a repository sentinel rather than only as a service check
// because the check and the insert cannot be made atomic in the service: two
// concurrent requests both read "no pending schedule" and both insert. The
// partial unique index (migration 000041) is what actually decides, and this is
// how that decision reaches the caller as the same 409 the service's own
// friendly check produces.
var ErrSchedulePending = apperrors.New(
	"CONTENT_SCHEDULE_EXISTS",
	"this entry already has a pending schedule",
	http.StatusConflict,
)

// scheduleColumns is shared by every read here, for the reason
// contentTypeColumns gives: a column written but not read back comes back as
// its zero value, and for `state` that would mean every row reading as
// ScheduleStatePending — the worker would re-execute finished schedules.
const scheduleColumns = `id, tenant_id, entry_id, content_type_id,
	action, run_at, pinned_version, state, requested_by,
	created_at, executed_at, error`

func scanSchedule(row pgx.Row) (*domain.EntrySchedule, error) {
	var s domain.EntrySchedule
	if err := row.Scan(
		&s.ID, &s.TenantID, &s.EntryID, &s.ContentTypeID,
		&s.Action, &s.RunAt, &s.PinnedVersion, &s.State, &s.RequestedBy,
		&s.CreatedAt, &s.ExecutedAt, &s.Error,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateEntrySchedule files one intent.
func (r *PostgresContentRepository) CreateEntrySchedule(ctx context.Context, s *domain.EntrySchedule) error {
	return r.withTenant(ctx, s.TenantID, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO content_entry_schedules
				(id, tenant_id, entry_id, content_type_id, action, run_at,
				 pinned_version, state, requested_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			s.ID, s.TenantID, s.EntryID, s.ContentTypeID, s.Action, s.RunAt,
			s.PinnedVersion, s.State, s.RequestedBy, s.CreatedAt,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return ErrSchedulePending
			}
			return fmt.Errorf("insert entry schedule: %w", err)
		}
		return nil
	})
}

// GetPendingEntrySchedule returns the entry's live intent, or (nil, nil) when
// it has none.
//
// nil-nil rather than ErrNotFound because "no schedule" is the ordinary state
// of almost every entry: the read path that decorates an EntryDTO with it asks
// this question about entries it expects to answer no for, and an error-typed
// no would put a branch on every one of them.
func (r *PostgresContentRepository) GetPendingEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	var out *domain.EntrySchedule
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		s, err := scanSchedule(q.QueryRow(ctx, `
			SELECT `+scheduleColumns+`
			FROM content_entry_schedules
			WHERE tenant_id = $1 AND entry_id = $2 AND state = $3`,
			tenantID, entryID, domain.ScheduleStatePending))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = s
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get pending entry schedule: %w", err)
	}
	return out, nil
}

// GetActionableEntrySchedule returns the newest schedule the reader can still
// DO something about — pending or stale — or (nil, nil) when there is none.
//
// STALE BELONGS IN THIS SET, and that is the whole reason the method exists
// beside GetPendingEntrySchedule. A stale schedule is not finished business: it
// is a release that will not happen unless someone re-files it, and the person
// who needs to know that is the editor looking at the entry. Showing only
// pending rows means the marker on the entry simply VANISHES at the moment it
// starts mattering — the failure mode ADR-017 §2 accepted and this closes.
//
// done / cancelled / superseded / failed are excluded because nothing is owed:
// the first three happened as intended, and a failed one is reported through
// GET /schedule, which reads any state and carries the error text.
func (r *PostgresContentRepository) GetActionableEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	var out *domain.EntrySchedule
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		s, err := scanSchedule(q.QueryRow(ctx, `
			SELECT `+scheduleColumns+`
			FROM content_entry_schedules
			WHERE tenant_id = $1 AND entry_id = $2 AND state = ANY($3)
			ORDER BY created_at DESC, id DESC
			LIMIT 1`,
			tenantID, entryID,
			[]string{domain.ScheduleStatePending, domain.ScheduleStateStale}))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = s
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get actionable entry schedule: %w", err)
	}
	return out, nil
}

// ListActionableEntrySchedules is GetActionableEntrySchedule for a page of
// entries in one round trip, for the reason GetEntriesByIDs takes a slice
// instead of being called once per row: the list endpoint already fetched a
// whole page, and decorating it one entry at a time would turn a single query
// into N — the exact per-row schedule lookup GetActionableEntrySchedule's own
// doc comment warns against embedding in a list projector.
//
// DISTINCT ON (entry_id) ... ORDER BY entry_id, created_at DESC, id DESC picks
// the same row GetActionableEntrySchedule's ORDER BY / LIMIT 1 would pick for
// that entry, just for every id in the slice at once. state = ANY(...) keeps
// the pending/stale filter identical to the single-row method, so a page and a
// direct GET can never disagree about which entries are "actionable".
//
// No ids is not a degenerate query — like GetEntriesByIDs, it is the ordinary
// shape of an empty page.
func (r *PostgresContentRepository) ListActionableEntrySchedules(ctx context.Context, tenantID string, entryIDs []uuid.UUID) (map[uuid.UUID]*domain.EntrySchedule, error) {
	if len(entryIDs) == 0 {
		return nil, nil
	}
	out := make(map[uuid.UUID]*domain.EntrySchedule, len(entryIDs))
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT DISTINCT ON (entry_id) `+scheduleColumns+`
			FROM content_entry_schedules
			WHERE tenant_id = $1 AND entry_id = ANY($2) AND state = ANY($3)
			ORDER BY entry_id, created_at DESC, id DESC`,
			tenantID, entryIDs,
			[]string{domain.ScheduleStatePending, domain.ScheduleStateStale})
		if err != nil {
			return fmt.Errorf("list actionable entry schedules: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanSchedule(rows)
			if err != nil {
				return fmt.Errorf("list actionable entry schedules scan: %w", err)
			}
			out[s.EntryID] = s
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetLatestEntrySchedule returns the newest schedule for the entry in ANY
// state, or apperrors.ErrNotFound when the entry has never been scheduled.
//
// "Any state" is what makes GET /entries/{id}/schedule worth calling: the
// question an editor asks after a post did not appear is answered by the
// terminal row (stale, cancelled, failed and why), and a read that returned
// only pending rows would answer "nothing scheduled" — indistinguishable from
// never having scheduled it at all.
func (r *PostgresContentRepository) GetLatestEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	var out *domain.EntrySchedule
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		s, err := scanSchedule(q.QueryRow(ctx, `
			SELECT `+scheduleColumns+`
			FROM content_entry_schedules
			WHERE tenant_id = $1 AND entry_id = $2
			ORDER BY created_at DESC, id DESC
			LIMIT 1`, tenantID, entryID))
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("get latest entry schedule: %w", err)
	}
	return out, nil
}

// CancelEntrySchedule withdraws the entry's pending intent and returns the row
// as it now stands. apperrors.ErrNotFound when there was nothing pending.
//
// The state moves; the row stays. A DELETE would erase the evidence that a
// release was planned and called off, which is exactly the question a "why is
// this not live" investigation ends on — and migration 000041 gives the table
// no DELETE policy at all, so this is the only spelling available.
func (r *PostgresContentRepository) CancelEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	var out *domain.EntrySchedule
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		s, err := scanSchedule(q.QueryRow(ctx, `
			UPDATE content_entry_schedules
			SET state = $4
			WHERE tenant_id = $1 AND entry_id = $2 AND state = $3
			RETURNING `+scheduleColumns,
			tenantID, entryID, domain.ScheduleStatePending, domain.ScheduleStateCancelled))
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("cancel entry schedule: %w", err)
	}
	return out, nil
}

// ListDueEntrySchedules answers the worker's only question: which schedules,
// ACROSS EVERY TENANT, are due.
//
// THIS IS THE ONE CROSS-TENANT READ IN THE CONTENT PLANE, and it does not go
// through withTenant — it cannot. app_current_tenant() fails closed when
// `app.tenant_id` is unset, so a cross-tenant SELECT under a production
// non-superuser role returns nothing. It would return rows in dev and CI, which
// connect as the postgres superuser and bypass RLS entirely, so the bug would
// be invisible until production and would present as "scheduled posts silently
// never went live". Hence the explicit opt-in GUC and the single extra policy
// that reads it (migration 000041 carries the full argument and the boundary:
// this table only, SELECT only, no content in the columns).
//
// The rows it returns are CANDIDATES, not claims. Nothing is locked here and
// two replicas will happily return the same row; the claim happens per row in
// ClaimDueEntrySchedule, under ordinary tenant RLS, where SKIP LOCKED settles
// the race.
func (r *PostgresContentRepository) ListDueEntrySchedules(ctx context.Context, now time.Time, limit int) ([]*domain.EntrySchedule, error) {
	if limit <= 0 {
		limit = scheduleBatchDefault
	}
	if limit > scheduleBatchMax {
		limit = scheduleBatchMax
	}
	// A caller-owned transaction is refused rather than joined. Joining would
	// mean running this under whatever tenant the caller set, which is the one
	// scope in which it returns the wrong answer — and setting the scan GUC
	// inside someone else's unit of work would widen reads they never asked to
	// widen. The worker owns its own transaction; nothing else calls this.
	if r.tx != nil {
		return nil, fmt.Errorf("content: ListDueEntrySchedules is a cross-tenant scan and must not join a tenant-bound transaction")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Transaction-local, like app.tenant_id: cleared at rollback, so the pooled
	// connection carries no scan privilege into the next request. This
	// transaction is read-only by construction — it runs one SELECT and then
	// rolls back.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.scheduler_scan', 'on', true)`); err != nil {
		return nil, fmt.Errorf("set scheduler scan context: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT `+scheduleColumns+`
		FROM content_entry_schedules
		WHERE state = $1 AND run_at <= $2
		ORDER BY run_at
		LIMIT $3`, domain.ScheduleStatePending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due entry schedules: %w", err)
	}
	defer rows.Close()
	var out []*domain.EntrySchedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due entry schedule: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due entry schedules: %w", err)
	}
	return out, nil
}

// scheduleBatchDefault / scheduleBatchMax bound one tick's work. A tick is not
// obliged to drain the queue: whatever it leaves is still due on the next one,
// and a bounded batch keeps a backlog from turning into a single transaction
// that holds locks for minutes.
const (
	scheduleBatchDefault = 50
	scheduleBatchMax     = 500
)

// ClaimDueEntrySchedule takes the row lock that decides which replica executes
// this schedule, and returns (nil, nil) when another one got there first.
//
// IT MUST BE CALLED INSIDE WithTx, and the caller must perform the whole
// operation — the publish, the state transition and the activity record — in
// that same transaction. The lock IS the claim: there is no `running` state and
// no claimed_at, so a worker that dies mid-flight simply rolls back to pending
// and the next tick picks the row up. That is the property that lets this
// feature ship without the reclaim job an outbox needs.
//
// SKIP LOCKED rather than a plain FOR UPDATE: a second worker that waited on
// the lock would acquire it the moment the first committed, re-read a row that
// is no longer pending, and find nothing to do — the same outcome, reached
// after a wait whose length is the first worker's publish. Skipping says so
// immediately.
//
// The state and run_at predicates are re-evaluated UNDER the lock. They were
// already true when ListDueEntrySchedules saw the row, and the window between
// is exactly long enough for an editor to cancel.
func (r *PostgresContentRepository) ClaimDueEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, now time.Time) (*domain.EntrySchedule, error) {
	if r.tx == nil {
		return nil, fmt.Errorf("content: ClaimDueEntrySchedule must run inside WithTx — the row lock it takes is the claim, and a transaction that ends here releases it")
	}
	var out *domain.EntrySchedule
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		s, err := scanSchedule(q.QueryRow(ctx, `
			SELECT `+scheduleColumns+`
			FROM content_entry_schedules
			WHERE tenant_id = $1 AND id = $2 AND state = $3 AND run_at <= $4
			FOR UPDATE SKIP LOCKED`,
			tenantID, id, domain.ScheduleStatePending, now))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = s
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim due entry schedule: %w", err)
	}
	return out, nil
}

// FinishEntrySchedule moves a claimed schedule to its terminal state.
//
// executedAt and errMsg are not free parameters: migration 000041 enforces
// both biconditionals (done/failed ⇔ an execution instant; failed ⇔ a message),
// so a caller that gets them wrong gets a constraint violation rather than a
// row that misreports what happened. That is deliberate — the states differ in
// what they claim about the world, and the database is the last reader that can
// still tell.
func (r *PostgresContentRepository) FinishEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error {
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE content_entry_schedules
			SET state = $3, executed_at = $4, error = $5
			WHERE tenant_id = $1 AND id = $2 AND state = $6`,
			tenantID, id, state, executedAt, errMsg, domain.ScheduleStatePending)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Not an error the caller can act on, but not silence either: it
			// means the row stopped being pending between the claim and here,
			// which under the locking above should be unreachable.
			return apperrors.ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, apperrors.ErrNotFound) {
			return err
		}
		return fmt.Errorf("finish entry schedule: %w", err)
	}
	return nil
}

// markSchedulesStale is called from UpdateEntry's applied path, inside its
// transaction, with the entry as it stands AFTER the write.
//
// Same transaction and not a follow-up call: the point of the pinned version is
// that an approved version cannot be overtaken silently, and a save that
// committed while the invalidation was still in flight would leave a window in
// which the worker publishes content nobody approved. The window is small; the
// content that goes through it is the content someone was in the middle of
// changing.
//
// It does NOT block the update, and that ordering is the ruling: an editor's
// save always wins, and the schedule is what yields.
//
// ONLY publish SCHEDULES ARE INVALIDATED (ADR-017 §2, and this is the ruling
// that the first cut got wrong). An unpublish retracts the LIVE SNAPSHOT, which
// a working-copy edit does not touch — so a typo fix on Tuesday must not
// silently cancel Friday's takedown. A takedown is a stop-the-bleeding action
// and is the one schedule that must not fail closed.
//
// The `pinned_version < $3` predicate makes this a no-op for a schedule filed
// against the new version, so it is safe on any write path that bumps version.
//
// IT ALSO WRITES THE ACTIVITY LINE, in SQL, in this transaction. Activity is
// normally recorded by the service after the operation returns (see
// RecordActivity), and this is the ApplySchema exception for the same reason:
// nobody performed this action, so there is no service call to hang it off, and
// a row claiming an invalidation that rolled back would be worse than no row.
// The three facts it carries are the ones that make it actionable — which
// schedule, the version it approved, the version that overtook it — and the
// keys are kept in lockstep with domain.ScheduleStaleDetails.
//
// target_title is left empty on purpose: domain.TitleFor's read-restriction
// fence lives in Go and must not be reimplemented in SQL, and this line is
// about a schedule rather than about content.
func markSchedulesStale(ctx context.Context, q querier, e *domain.Entry) error {
	if _, err := q.Exec(ctx, `
		WITH staled AS (
			UPDATE content_entry_schedules
			SET state = $4
			WHERE tenant_id = $1 AND entry_id = $2 AND state = $3
			  AND action = $5 AND pinned_version < $6
			RETURNING id, pinned_version
		)
		INSERT INTO content_activity
			(id, tenant_id, occurred_at, actor_kind, actor_user_id, actor_agent_id,
			 action, target_type, target_entry_id, target_title,
			 outcome, error_code, changed_keys, via, via_schedule_id, details)
		SELECT gen_random_uuid(), $1, $7, COALESCE($8::text, $9), $10::uuid, $11::text,
		       $12, COALESCE(ct.name, ''), $2, '',
		       $13, '', '{}'::text[], '', NULL,
		       jsonb_build_object(
		           'schedule_id', staled.id,
		           'pinned_version', staled.pinned_version,
		           'entry_version', $6::int)
		FROM staled LEFT JOIN content_types ct ON ct.id = $14`,
		e.TenantID, e.ID, domain.ScheduleStatePending, domain.ScheduleStateStale,
		domain.ScheduleActionPublish, e.Version,
		e.UpdatedAt, e.UpdatedByKind, domain.ActorKindHuman, e.UpdatedBy, e.UpdatedByAgent,
		domain.ActivityEntryScheduleStale, domain.ActivityOutcomeSuccess, e.ContentTypeID,
	); err != nil {
		return fmt.Errorf("mark entry schedules stale: %w", err)
	}
	return nil
}

// markSchedulesSuperseded is called from SetEntryPublishState, inside its
// transaction: someone performed by hand the operation a schedule was waiting
// to perform, so the schedule has nothing left to do.
//
// 'superseded' rather than 'done', and the difference is not cosmetic. 'done'
// would claim the schedule fired, so the activity stream and the schedule row
// would disagree about what put the entry live — and an editor debugging a
// release would be reading a record that says a worker acted at a time no
// worker ran.
//
// THE WORKER'S OWN EXECUTION DOES NOT LAND HERE, because it moves its schedule
// out of 'pending' BEFORE calling SetEntryPublishState. That ordering is load
// bearing: reversed, the worker's publish would supersede the very schedule it
// is executing, and the terminal state would depend on which UPDATE ran last.
func markSchedulesSuperseded(ctx context.Context, q querier, tenantID string, entryID uuid.UUID) error {
	if _, err := q.Exec(ctx, `
		UPDATE content_entry_schedules
		SET state = $4
		WHERE tenant_id = $1 AND entry_id = $2 AND state = $3`,
		tenantID, entryID, domain.ScheduleStatePending, domain.ScheduleStateSuperseded,
	); err != nil {
		return fmt.Errorf("mark entry schedules superseded: %w", err)
	}
	return nil
}
