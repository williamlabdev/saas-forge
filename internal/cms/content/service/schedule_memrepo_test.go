package service

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// memRepo's half of content_entry_schedules (000041).
//
// The rules it mirrors are the ones the TABLE enforces, not the ones the
// service does — the partial unique index on pending rows, and the two
// transitions that other writes make. Anything the service checks (a run_at in
// the past, an action nobody defined) is deliberately NOT re-checked here: a
// fake that refuses what the database would happily store cannot be used to
// prove the service refuses it first.

func (m *memRepo) CreateEntrySchedule(_ context.Context, s *domain.EntrySchedule) error {
	// uq_content_entry_schedules_pending: one pending row per entry. Returning
	// the repository's own sentinel rather than a generic error is what lets a
	// service test drive the 409 path through the same branch production takes.
	for _, it := range m.schedules {
		if it.TenantID == s.TenantID && it.EntryID == s.EntryID && it.State == domain.ScheduleStatePending {
			return repository.ErrSchedulePending
		}
	}
	cp := *s
	if cp.State == "" {
		cp.State = domain.ScheduleStatePending
	}
	m.schedules = append(m.schedules, &cp)
	*s = cp
	return nil
}

func (m *memRepo) GetPendingEntrySchedule(_ context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	for _, it := range m.schedules {
		if it.TenantID == tenantID && it.EntryID == entryID && it.State == domain.ScheduleStatePending {
			cp := *it
			return &cp, nil
		}
	}
	// (nil, nil), not ErrNotFound: "nothing is scheduled" is an ordinary answer
	// on every read path that decorates an entry, and making it an error would
	// put an error branch on the common case.
	return nil, nil
}

func (m *memRepo) GetLatestEntrySchedule(_ context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	var latest *domain.EntrySchedule
	for _, it := range m.schedules {
		if it.TenantID != tenantID || it.EntryID != entryID {
			continue
		}
		if latest == nil || it.CreatedAt.After(latest.CreatedAt) {
			latest = it
		}
	}
	if latest == nil {
		return nil, apperrors.ErrNotFound
	}
	cp := *latest
	return &cp, nil
}

func (m *memRepo) CancelEntrySchedule(_ context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	for _, it := range m.schedules {
		if it.TenantID == tenantID && it.EntryID == entryID && it.State == domain.ScheduleStatePending {
			it.State = domain.ScheduleStateCancelled
			cp := *it
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) ListDueEntrySchedules(_ context.Context, now time.Time, limit int) ([]*domain.EntrySchedule, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []*domain.EntrySchedule
	for _, it := range m.schedules {
		if it.State == domain.ScheduleStatePending && !it.RunAt.After(now) {
			cp := *it
			out = append(out, &cp)
		}
	}
	// Oldest due first, mirroring ORDER BY run_at: a backlog must drain in the
	// order it was promised, or the batch cap turns into a queue that starves
	// its own head.
	sort.SliceStable(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memRepo) ClaimDueEntrySchedule(_ context.Context, tenantID string, id uuid.UUID, now time.Time) (*domain.EntrySchedule, error) {
	for _, it := range m.schedules {
		if it.TenantID != tenantID || it.ID != id {
			continue
		}
		// The WHERE clause of the real claim, minus the locking. (nil, nil) here
		// stands for "another replica has it, or it stopped being pending" —
		// the fake cannot produce the first, but the service and worker must
		// treat both the same way, and this keeps that branch reachable.
		if it.State != domain.ScheduleStatePending || it.RunAt.After(now) {
			return nil, nil
		}
		cp := *it
		return &cp, nil
	}
	return nil, nil
}

func (m *memRepo) FinishEntrySchedule(_ context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error {
	for _, it := range m.schedules {
		if it.TenantID == tenantID && it.ID == id && it.State == domain.ScheduleStatePending {
			it.State = state
			it.ExecutedAt = executedAt
			it.Error = errMsg
			return nil
		}
	}
	return apperrors.ErrNotFound
}

// markSchedulesStale mirrors the statement UpdateEntry runs in its own
// transaction — INCLUDING the activity line it writes there, because a fake
// that invalidates silently cannot be used to prove the invalidation is
// audible.
//
// `pinned_version < newVersion` and not `!=` for the same reason the SQL says
// so: a version can only go up, and the inequality documents that this can
// never demote a schedule pinned to a version that has not happened. The
// `action == publish` filter is the ADR-017 §2 ruling: an unpublish retracts the
// live snapshot, which an edit to the working copy does not touch.
func (m *memRepo) markSchedulesStale(e *domain.Entry) {
	for _, it := range m.schedules {
		if it.TenantID != e.TenantID || it.EntryID != e.ID ||
			it.State != domain.ScheduleStatePending ||
			it.Action != domain.ScheduleActionPublish || it.PinnedVersion >= e.Version {
			continue
		}
		it.State = domain.ScheduleStateStale
		entryID := e.ID
		kind := domain.ActorKindHuman
		if e.UpdatedByKind != nil {
			kind = *e.UpdatedByKind
		}
		m.activity = append(m.activity, &domain.Activity{
			ID:            uuid.New(),
			TenantID:      e.TenantID,
			OccurredAt:    e.UpdatedAt,
			ActorKind:     kind,
			ActorUserID:   clonePtr(e.UpdatedBy),
			ActorAgentID:  e.UpdatedByAgent,
			Action:        domain.ActivityEntryScheduleStale,
			TargetEntryID: &entryID,
			Outcome:       domain.ActivityOutcomeSuccess,
			Details:       domain.ScheduleStaleDetails(it.ID, it.PinnedVersion, e.Version),
		})
	}
}

// GetActionableEntrySchedule mirrors the pending-OR-stale read the admin DTO
// decoration uses. Newest first, so a re-filed schedule shadows the one it
// replaced.
func (m *memRepo) GetActionableEntrySchedule(_ context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error) {
	var latest *domain.EntrySchedule
	for _, it := range m.schedules {
		if it.TenantID != tenantID || it.EntryID != entryID {
			continue
		}
		if it.State != domain.ScheduleStatePending && it.State != domain.ScheduleStateStale {
			continue
		}
		if latest == nil || it.CreatedAt.After(latest.CreatedAt) {
			latest = it
		}
	}
	if latest == nil {
		return nil, nil
	}
	cp := *latest
	return &cp, nil
}

// ListActionableEntrySchedules is GetActionableEntrySchedule for a slice of
// ids, mirroring the DISTINCT ON query's per-entry "newest pending-or-stale
// row" answer — just built by calling the single-row rule once per id rather
// than a second, divergent selection algorithm the fake would have to keep in
// step with the real one.
func (m *memRepo) ListActionableEntrySchedules(ctx context.Context, tenantID string, entryIDs []uuid.UUID) (map[uuid.UUID]*domain.EntrySchedule, error) {
	if len(entryIDs) == 0 {
		return nil, nil
	}
	out := make(map[uuid.UUID]*domain.EntrySchedule, len(entryIDs))
	for _, id := range entryIDs {
		s, err := m.GetActionableEntrySchedule(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out[id] = s
		}
	}
	return out, nil
}

func (m *memRepo) markSchedulesSuperseded(tenantID string, entryID uuid.UUID) {
	for _, it := range m.schedules {
		if it.TenantID == tenantID && it.EntryID == entryID && it.State == domain.ScheduleStatePending {
			it.State = domain.ScheduleStateSuperseded
		}
	}
}
