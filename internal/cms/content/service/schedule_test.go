package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Scheduled publish/unpublish, service side (ADR-017).
//
// Every test here nails the clock down. "In the future" and "more than a year
// out" are the two boundaries this feature is made of, and a test that computes
// them from time.Now() is a test whose failure mode is a flake at midnight
// rather than a red build on the change that broke it.

var schedNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return schedNow } }

// newSchedSvc is newSvc with a frozen clock and, optionally, an authorizer that
// answers something other than yes.
func newSchedSvc(az authz.Authorizer) (ContentService, *memRepo) {
	repo := &memRepo{}
	if az == nil {
		az = authz.NewAllowAllAuthorizer()
	}
	svc := NewContentService(repo, az, staticPlan(Quota{}))
	svc.(*contentService).WithClock(fixedClock())
	return svc, repo
}

func rfc(d time.Duration) string { return schedNow.Add(d).Format(time.RFC3339) }

// requireCodeStatus is requireCode plus the HTTP status. The status is part of
// each of these contracts — 422 vs 409 vs 404 is what a client branches on —
// and the existing helper only checks the code.
func requireCodeStatus(t *testing.T, err error, code string, status int) {
	t.Helper()
	requireCode(t, err, code)
	ae, _ := apperrors.As(err)
	assert.Equal(t, status, ae.HTTPStatus)
}

func TestScheduleEntry_PinsTheVersionItApproved(t *testing.T) {
	svc, repo := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID

	dto, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(2 * time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, domain.ScheduleActionPublish, dto.Action)
	assert.Equal(t, domain.ScheduleStatePending, dto.State)
	assert.Equal(t, schedNow.Add(2*time.Hour), dto.RunAt.UTC())
	// THE POINT OF THE WHOLE FEATURE, in one assertion: what was approved is a
	// specific version, not "whatever this entry says on Friday". Version 1 is
	// the draft that was in front of the person when they pressed schedule.
	assert.Equal(t, 1, dto.PinnedVersion)
	require.Len(t, repo.schedules, 1)
	assert.Equal(t, "t1", repo.schedules[0].TenantID)
	assert.NotEqual(t, uuid.Nil, repo.schedules[0].RequestedBy,
		"the requester is the provenance the worker publishes under; an empty one would make published_by unanswerable")
}

func TestScheduleEntry_RejectsUnknownAction(t *testing.T) {
	svc, _ := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID

	_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{Action: "archive", RunAt: rfc(time.Hour)})
	requireCodeStatus(t, err, "CONTENT_SCHEDULE_ACTION_INVALID", 422)
}

// The three ways a time is wrong all answer with ONE code. They are one
// question — "is this a moment this system will act on?" — and splitting them
// would make a client branch on which flavour of no it received.
func TestScheduleEntry_RejectsUnusableTimes(t *testing.T) {
	cases := map[string]string{
		"not a timestamp":   "next friday",
		"in the past":       rfc(-time.Minute),
		"now, not future":   schedNow.Format(time.RFC3339),
		"beyond a year":     rfc(366 * 24 * time.Hour),
		"date without zone": "2026-12-01T09:00:00",
	}
	for name, runAt := range cases {
		t.Run(name, func(t *testing.T) {
			svc, repo := newSchedSvc(nil)
			ctx := ctxTenant("t1")
			id := seedEntry(t, svc, ctx).ID

			_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
				Action: domain.ScheduleActionPublish, RunAt: runAt,
			})
			requireCodeStatus(t, err, "CONTENT_SCHEDULE_TIME_INVALID", 422)
			assert.Empty(t, repo.schedules, "a refused schedule must not leave a row behind")
		})
	}
}

func TestScheduleEntry_OnePendingPerEntry(t *testing.T) {
	svc, _ := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID

	_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(2 * time.Hour),
	})
	require.NoError(t, err)

	_, err = svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(3 * time.Hour),
	})
	requireCodeStatus(t, err, "CONTENT_SCHEDULE_EXISTS", 409)
	// The existing time rides in the details, because the client's next move is
	// to show the editor what is already booked — and a 409 that does not say
	// what it collided with forces a second round trip to find out.
	ae, _ := apperrors.As(err)
	require.NotNil(t, ae.Details)
	assert.Contains(t, string(mustJSON(t, ae.Details)), schedNow.Add(2*time.Hour).Format(time.RFC3339))
}

// A schedule that would change nothing is refused rather than accepted and
// quietly dropped later: the editor is told now, while they are still looking
// at the screen, instead of on Monday when nothing happened.
func TestScheduleEntry_RefusesNoops(t *testing.T) {
	t.Run("publish an already-live entry with no edits", func(t *testing.T) {
		svc, _ := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID
		_, err := svc.SetEntryStatus(ctx, "order", id, domain.StatusPublished, 0)
		require.NoError(t, err)

		_, err = svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
		})
		requireCodeStatus(t, err, "CONTENT_SCHEDULE_NOOP", 422)
	})

	t.Run("unpublish a draft", func(t *testing.T) {
		svc, _ := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID

		_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionUnpublish, RunAt: rfc(time.Hour),
		})
		requireCodeStatus(t, err, "CONTENT_SCHEDULE_NOOP", 422)
	})

	t.Run("publish a live entry that HAS edits is not a noop", func(t *testing.T) {
		svc, _ := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID
		_, err := svc.SetEntryStatus(ctx, "order", id, domain.StatusPublished, 0)
		require.NoError(t, err)
		_, err = svc.UpdateEntry(ctx, "order", id, mustJSON(t, map[string]any{"title": "T2", "state": "new"}), 0)
		require.NoError(t, err)

		_, err = svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
		})
		require.NoError(t, err, "releasing pending edits is the ordinary case; refusing it would gut the feature")
	})
}

// WHICH VERB SCHEDULING ASKS FOR, pinned white-box — the same shape as the
// schema-proposal tests, and for the same reason: every role that can publish
// can also update, so a role-level test cannot tell the two apart. The property
// under test is ADR-014 §1's human gate, which exists precisely for the caller
// that holds one verb and not the other.
func TestScheduleEntry_AsksForTheVerbItIsDeferring(t *testing.T) {
	t.Run("scheduling a publish needs content:publish", func(t *testing.T) {
		svc, repo := newSchedSvc(denyVerb{action: authz.ActionContentPublish})
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID

		_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
		})
		require.ErrorIs(t, err, apperrors.ErrForbidden,
			"otherwise 'publish this on Friday' is a way to publish without the verb")
		assert.Empty(t, repo.schedules)
	})

	t.Run("scheduling an unpublish does not", func(t *testing.T) {
		// Published by a caller who HAS the verb — the entry has to be live for
		// the unpublish to be anything but a noop, and getting it there is setup,
		// not the property under test.
		svc, repo := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID
		_, err := svc.SetEntryStatus(ctx, "order", id, domain.StatusPublished, 0)
		require.NoError(t, err)

		limited := NewContentService(repo, denyVerb{action: authz.ActionContentPublish}, staticPlan(Quota{}))
		limited.(*contentService).WithClock(fixedClock())
		_, err = limited.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionUnpublish, RunAt: rfc(time.Hour),
		})
		require.NoError(t, err, "taking something down is gated on content:update, as unpublish itself is")
	})
}

// Editing the working copy invalidates the pending schedule, IN THE SAME
// TRANSACTION as the edit — and does not block it. The editor keeps working;
// what they lose is the release they no longer approved.
func TestUpdateEntry_MakesAPendingScheduleStale(t *testing.T) {
	svc, repo := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID
	_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)

	_, err = svc.UpdateEntry(ctx, "order", id, mustJSON(t, map[string]any{"title": "edited", "state": "new"}), 0)
	require.NoError(t, err, "an edit must never be refused because something is scheduled")

	require.Len(t, repo.schedules, 1)
	assert.Equal(t, domain.ScheduleStateStale, repo.schedules[0].State)

	// And it is written down. A release that quietly stops being a release is
	// the failure this line exists to prevent: the activity feed is where an
	// editor finds out that their Friday release is now their problem again.
	var stale *domain.Activity
	for _, a := range repo.activity {
		if a.Action == domain.ActivityEntryScheduleStale {
			stale = a
		}
	}
	require.NotNil(t, stale, "the invalidation is an editorial event, not an implementation detail")
	require.NotNil(t, stale.TargetEntryID)
	assert.Equal(t, id, *stale.TargetEntryID)
	var d map[string]any
	require.NoError(t, json.Unmarshal(stale.Details, &d))
	assert.Equal(t, repo.schedules[0].ID.String(), d["schedule_id"])
	assert.EqualValues(t, 1, d["pinned_version"], "what the requester approved")
	assert.EqualValues(t, 2, d["entry_version"], "what overtook it")
}

// THE ASYMMETRY (ADR-017 §2). An unpublish retracts the LIVE SNAPSHOT; editing
// the working copy changes nothing about what is live, so the edit has no
// bearing on the takedown. Staling it here would mean a page scheduled to come
// down at midnight silently stays up because somebody fixed a typo at 23:50 —
// the one failure mode a retraction must not have.
func TestUpdateEntry_LeavesAScheduledUnpublishAlone(t *testing.T) {
	svc, repo := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID
	_, err := svc.SetEntryStatus(ctx, "order", id, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, err = svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionUnpublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)

	_, err = svc.UpdateEntry(ctx, "order", id, mustJSON(t, map[string]any{"title": "edited", "state": "new"}), 0)
	require.NoError(t, err)

	require.Len(t, repo.schedules, 1)
	assert.Equal(t, domain.ScheduleStatePending, repo.schedules[0].State,
		"the takedown is about the snapshot; the working copy is not the snapshot")
	for _, a := range repo.activity {
		assert.NotEqual(t, domain.ActivityEntryScheduleStale, a.Action,
			"nothing was invalidated, so nothing may claim it was")
	}
}

func TestSetEntryStatus_SupersedesAPendingSchedule(t *testing.T) {
	svc, repo := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID
	_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)

	_, err = svc.SetEntryStatus(ctx, "order", id, domain.StatusPublished, 0)
	require.NoError(t, err)

	require.Len(t, repo.schedules, 1)
	assert.Equal(t, domain.ScheduleStateSuperseded, repo.schedules[0].State,
		"somebody did it by hand; firing again on Friday would republish whatever the entry looks like then")
}

func TestCancelEntrySchedule(t *testing.T) {
	t.Run("withdraws the pending row", func(t *testing.T) {
		svc, repo := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID
		_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
		})
		require.NoError(t, err)

		require.NoError(t, svc.CancelEntrySchedule(ctx, "order", id))
		require.Len(t, repo.schedules, 1)
		assert.Equal(t, domain.ScheduleStateCancelled, repo.schedules[0].State)
	})

	t.Run("404 when nothing is pending", func(t *testing.T) {
		svc, _ := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID

		err := svc.CancelEntrySchedule(ctx, "order", id)
		requireCodeStatus(t, err, "CONTENT_SCHEDULE_NOT_FOUND", 404)
	})

	// Cancelling a publish is a decision about a release, so it asks for the
	// release verb. Without this, a credential scoped to content:update — the
	// shape every agent credential has — could delete a human's approved
	// release the hour before it went out, which is the ADR-014 §1 gate lost by
	// a different door.
	t.Run("cancelling a scheduled publish needs content:publish", func(t *testing.T) {
		svc, repo := newSchedSvc(nil)
		ctx := ctxTenant("t1")
		id := seedEntry(t, svc, ctx).ID
		_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
			Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
		})
		require.NoError(t, err)

		// Same repository, an authorizer that has since lost the publish verb.
		limited := NewContentService(repo, denyVerb{action: authz.ActionContentPublish}, staticPlan(Quota{}))
		limited.(*contentService).WithClock(fixedClock())
		err = limited.CancelEntrySchedule(ctx, "order", id)
		require.ErrorIs(t, err, apperrors.ErrForbidden)
		assert.Equal(t, domain.ScheduleStatePending, repo.schedules[0].State)
	})
}

func TestGetEntrySchedule_ReportsTheLatestInAnyState(t *testing.T) {
	svc, repo := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID

	_, err := svc.GetEntrySchedule(ctx, "order", id)
	requireCodeStatus(t, err, "CONTENT_SCHEDULE_NOT_FOUND", 404)

	_, err = svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, svc.CancelEntrySchedule(ctx, "order", id))

	// Cancelled, not gone: "it was called off" is an answer somebody needs, and
	// a 404 here would make every terminal outcome — including `failed` —
	// invisible.
	got, err := svc.GetEntrySchedule(ctx, "order", id)
	require.NoError(t, err)
	assert.Equal(t, domain.ScheduleStateCancelled, got.State)
	assert.Equal(t, repo.schedules[0].ID, got.ID)
}

func TestGetEntry_CarriesThePendingScheduleForAdmins(t *testing.T) {
	svc, _ := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID
	_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)

	dto, err := svc.GetEntry(ctx, "order", id, GetEntryInput{})
	require.NoError(t, err)
	require.NotNil(t, dto.Schedule, "the release screen has to show what is booked")
	assert.Equal(t, domain.ScheduleActionPublish, dto.Schedule.Action)
	assert.Equal(t, 1, dto.Schedule.PinnedVersion)

	// Terminal states go away — the field means "this is outstanding", not "this
	// once happened". GetEntrySchedule is the endpoint for history.
	require.NoError(t, svc.CancelEntrySchedule(ctx, "order", id))
	dto, err = svc.GetEntry(ctx, "order", id, GetEntryInput{})
	require.NoError(t, err)
	assert.Nil(t, dto.Schedule)

	// A stale row, though, KEEPS the field. Stale is an actionable state —
	// somebody has to look at the edit and re-file the release — and a field
	// that vanished the moment the schedule broke would make the release
	// calendar lie by omission at exactly the moment it matters.
	_, err = svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(2 * time.Hour),
	})
	require.NoError(t, err)
	_, err = svc.UpdateEntry(ctx, "order", id, mustJSON(t, map[string]any{"title": "edited", "state": "new"}), 0)
	require.NoError(t, err)

	dto, err = svc.GetEntry(ctx, "order", id, GetEntryInput{})
	require.NoError(t, err)
	require.NotNil(t, dto.Schedule, "stale must not disappear; it is a to-do, not a tombstone")
	assert.Equal(t, domain.ScheduleStateStale, dto.Schedule.State)
}

// The console's Scheduled badge reads `schedule` off each row of the LIST
// response, not off a second GET per row — ListEntries built every EntryDTO
// through ProjectEntry(...).narrowedTo(...).withRelated(...) alone, with no
// withSchedule anywhere in either branch, so the field was present on GetEntry
// and silently absent here. This pins that a page mixes a scheduled and an
// unscheduled entry correctly, both ways.
func TestListEntries_CarriesTheScheduleForAdmins(t *testing.T) {
	svc, _ := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	scheduled := seedEntry(t, svc, ctx)
	plain, err := svc.CreateEntry(ctx, "order", json.RawMessage(`{"title":"b"}`))
	require.NoError(t, err)
	_, err = svc.ScheduleEntry(ctx, "order", scheduled.ID, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)

	res, err := svc.ListEntries(ctx, "order", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)

	byID := make(map[uuid.UUID]EntryDTO, len(res.Items))
	for _, it := range res.Items {
		byID[it.ID] = it
	}
	require.NotNil(t, byID[scheduled.ID].Schedule, "the scheduled row must carry it in the list, not only on a direct GET")
	assert.Equal(t, domain.ScheduleActionPublish, byID[scheduled.ID].Schedule.Action)
	assert.Nil(t, byID[plain.ID].Schedule, "an entry with no schedule must not gain one")

	// Wire check, not just the Go struct: the console's badge reads the JSON key,
	// so this pins that MarshalJSON actually emits `schedule` for the row that
	// has one and omits it for the row that does not.
	assert.Contains(t, topLevelKeys(t, byID[scheduled.ID]), "schedule")
	assert.NotContains(t, topLevelKeys(t, byID[plain.ID]), "schedule")
}

// The list endpoint's cursor-paged (delivery) branch runs the same
// decorateEntriesWithSchedule call the admin branch does — see ListEntries —
// so this pins that the batched lookup is skipped for PublicDelivery rather
// than fetched and then merely left off the wire. A pending schedule is an
// editorial plan; "this page changes on Friday" is exactly the kind of thing a
// competitor should never read off the public collection endpoint either.
func TestListEntries_DeliveryNeverCarriesSchedule(t *testing.T) {
	svc, _ := newSchedSvc(nil)
	admin := ctxTenant("t1")
	live := seedEntry(t, svc, admin)
	_, err := svc.SetEntryStatus(admin, "order", live.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, err = svc.ScheduleEntry(admin, "order", live.ID, ScheduleEntryInput{
		Action: domain.ScheduleActionUnpublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)

	res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Nil(t, res.Items[0].Schedule)
	assert.NotContains(t, topLevelKeys(t, res.Items[0]), "schedule")
}

// The delivery edge must never learn that something is booked. A pending
// schedule is an editorial plan: "this page changes on Friday" is exactly the
// kind of thing a competitor reads off a public API.
func TestDelivery_EntryDTOCarriesNoSchedule(t *testing.T) {
	e := &domain.Entry{
		ID:               uuid.New(),
		Payload:          json.RawMessage(`{"title":"working"}`),
		PublishedPayload: json.RawMessage(`{"title":"live"}`),
		Version:          2,
		PublishedVersion: 1,
		Status:           domain.StatusPublished,
		Locale:           domain.DefaultLocale,
	}
	ct := &domain.ContentType{Name: "order", Fields: []domain.Field{{Key: "title", Type: domain.FieldTypeString}}}
	pending := &domain.EntrySchedule{
		Action: domain.ScheduleActionPublish, RunAt: schedNow.Add(time.Hour),
		PinnedVersion: 2, State: domain.ScheduleStatePending,
	}

	// Guard first, or the assertions below pass vacuously: the admin audience
	// must actually emit the key.
	assert.Contains(t, topLevelKeys(t, ProjectEntry(ct, e, adminSubject()).withSchedule(pending)), "schedule")

	// withSchedule refuses the non-admin audiences outright...
	assert.NotContains(t, topLevelKeys(t, ProjectEntry(ct, e, deliverySubject()).withSchedule(pending)), "schedule")
	assert.NotContains(t, topLevelKeys(t, ProjectEntry(ct, e, previewSubject(e.ID)).withSchedule(pending)), "schedule")

	// ...and MarshalJSON clears the field again for those audiences, so a DTO
	// that acquired one by any other route still cannot render it. Two layers on
	// purpose: this is the same belt-and-braces the authorship fields get.
	leaked := ProjectEntry(ct, e, deliverySubject())
	leaked.Schedule = &EntryScheduleSummary{Action: domain.ScheduleActionPublish, RunAt: schedNow, PinnedVersion: 2, State: domain.ScheduleStatePending}
	assert.NotContains(t, topLevelKeys(t, leaked), "schedule")
}

// Scheduling and cancelling are recorded like every other editorial decision
// (ADR-014 §3). Without this the release calendar would be the one part of the
// system where "who decided this" has no answer.
func TestSchedule_RecordsActivity(t *testing.T) {
	svc, repo := newSchedSvc(nil)
	ctx := ctxTenant("t1")
	id := seedEntry(t, svc, ctx).ID

	_, err := svc.ScheduleEntry(ctx, "order", id, ScheduleEntryInput{
		Action: domain.ScheduleActionPublish, RunAt: rfc(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, svc.CancelEntrySchedule(ctx, "order", id))

	var actions []string
	for _, a := range repo.activity {
		actions = append(actions, a.Action)
	}
	assert.Contains(t, actions, domain.ActivityEntrySchedule)
	assert.Contains(t, actions, domain.ActivityEntryScheduleCancel)
}
