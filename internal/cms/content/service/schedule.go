package service

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Scheduled publish / unpublish (ADR-017).
//
// THE ONE THING TO UNDERSTAND ABOUT THIS FILE: it does not perform anything. It
// records a decision — this entry, this operation, at this time, on this
// version — and every authorization question is settled HERE, at the moment a
// person makes that decision. The worker in internal/cms/content/scheduler is
// deliberately unauthorized: it holds no credential, has no subject, and asks
// nobody's permission. That is not a shortcut. Re-authorizing at execution time
// would mean deciding what to do when the requester's role has since changed,
// and every answer to that is wrong in a different way (silently drop a release
// the editor is still expecting; publish under a permission they no longer
// hold). ADR-017 §4 makes the ruling: authorization is a fact about the moment
// of the decision, and the mechanism that preserves it is pinned_version.

// ScheduleEntryInput is one filed intent, as it arrives from the wire.
//
// RunAt is a STRING, not a time.Time, and that is deliberate. The three ways a
// time can be wrong — unparseable, already past, absurdly far ahead — are one
// error code to the caller (CONTENT_SCHEDULE_TIME_INVALID), and splitting the
// parse into the handler would put one of the three there and two here. A
// caller that sends "next tuesday" and a caller that sends last year have made
// the same mistake and should read the same message.
type ScheduleEntryInput struct {
	Action string `json:"action"`
	// RunAt is the RAW string off the wire, deliberately not a time.Time.
	//
	// Decoding it in the handler would split "this is not a usable moment" into
	// two answers: a 400 INVALID_JSON for a timestamp encoding/json cannot read,
	// and a 422 CONTENT_SCHEDULE_TIME_INVALID for one it can read but that has
	// already passed. Those are the same mistake from the caller's side, and
	// they get one code because a client should not have to branch on which
	// layer noticed.
	RunAt string `json:"run_at"`
}

// EntryScheduleDTO is a schedule as its own endpoints return it.
//
// It is the FULL row, including the terminal states, because the question this
// endpoint exists to answer is "why did my post not go live" — and every useful
// answer to that (stale, cancelled, superseded, failed and its message) lives
// in a row that is no longer pending.
type EntryScheduleDTO struct {
	ID     uuid.UUID `json:"id"`
	Action string    `json:"action"`
	RunAt  time.Time `json:"run_at"`
	// PinnedVersion is the entry version the requester approved. It is exposed
	// rather than kept internal because it is the field that explains a `stale`
	// state: an editor comparing it against the entry's current version can see
	// at a glance that the content moved on.
	PinnedVersion int    `json:"pinned_version"`
	State         string `json:"state"`
	// RequestedBy is who answers for the release. nil is unrecorded, which a
	// reader renders as unknown rather than inventing an actor — the same
	// three-state rule the entry provenance fields follow.
	RequestedBy *uuid.UUID `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	// ExecutedAt is present only when the operation actually ran, and Error only
	// when it failed. Both are `omitempty` because their absence is meaningful:
	// a cancelled schedule has no execution instant, and inventing one would
	// claim a run that never happened.
	ExecutedAt *time.Time `json:"executed_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// EntryScheduleSummary is the four fields that ride along on an admin EntryDTO
// — what is going to happen to this entry, and when.
//
// A narrower type rather than the DTO above, and not to save bytes: the entry
// read carries only PENDING schedules, so requested_by/executed_at/error would
// be a constant nil, a constant nil and a constant empty string on every entry
// in the tenant. Fields that are always absent teach a client to stop looking
// at them, which is how a field that later becomes meaningful gets ignored.
type EntryScheduleSummary struct {
	Action        string    `json:"action"`
	RunAt         time.Time `json:"run_at"`
	PinnedVersion int       `json:"pinned_version"`
	State         string    `json:"state"`
}

func projectSchedule(s *domain.EntrySchedule) EntryScheduleDTO {
	dto := EntryScheduleDTO{
		ID:            s.ID,
		Action:        s.Action,
		RunAt:         s.RunAt,
		PinnedVersion: s.PinnedVersion,
		State:         s.State,
		CreatedAt:     s.CreatedAt,
		ExecutedAt:    s.ExecutedAt,
		Error:         s.Error,
	}
	if s.RequestedBy != uuid.Nil {
		id := s.RequestedBy
		dto.RequestedBy = &id
	}
	return dto
}

// The refusal vocabulary. Five codes, and each names a DIFFERENT mistake the
// caller can fix — which is the test a new error code has to pass here. A
// single CONTENT_SCHEDULE_INVALID covering all five would tell an editor who
// typed the wrong year exactly what it tells one whose colleague already
// scheduled the post.

func errScheduleActionInvalid(action string) error {
	return apperrors.New("CONTENT_SCHEDULE_ACTION_INVALID", "schedule action must be publish or unpublish", 422).
		WithDetails(map[string]any{"action": action, "allowed": domain.AllScheduleActions()})
}

// errScheduleTimeInvalid carries `reason` so the three ways a time is wrong stay
// distinguishable to a human without becoming three codes a client must switch
// on. The code is what a program branches on; the detail is what a person reads.
func errScheduleTimeInvalid(runAt, reason string) error {
	return apperrors.New("CONTENT_SCHEDULE_TIME_INVALID", "run_at must be an RFC3339 timestamp within the next year, in the future", 422).
		WithDetails(map[string]any{"run_at": runAt, "reason": reason})
}

// errScheduleExists carries the EXISTING run_at, not just the fact of a clash.
// "Already scheduled" leaves the editor to go and look; "already scheduled for
// Monday 09:00" is usually the whole answer.
func errScheduleExists(existing *domain.EntrySchedule) error {
	return repository.ErrSchedulePending.WithDetails(map[string]any{
		"action": existing.Action,
		"run_at": existing.RunAt.UTC().Format(time.RFC3339),
	})
}

var errScheduleNotFound = apperrors.New(
	"CONTENT_SCHEDULE_NOT_FOUND",
	"this entry has no schedule",
	http.StatusNotFound,
)

// errScheduleNoop refuses a schedule that would do nothing when it came due.
//
// It is a refusal rather than a silent accept for the reason SetEntryStatus
// cancels its activity record on the same condition: an editor who schedules a
// publish for an already-published, unchanged entry has misunderstood the state
// of their content, and the useful moment to tell them is now — not on Monday,
// with a `done` row that released nothing.
//
// It is checked against the state AT SCHEDULING TIME, which cannot be a
// promise about Monday: the entry may be unpublished in between, and then the
// schedule that was a no-op becomes meaningful. That direction is fine. The
// reverse — a schedule that becomes a no-op — is handled by SetEntryStatus's own
// short-circuit when the worker gets there.
func errScheduleNoop(reason string) error {
	return apperrors.New("CONTENT_SCHEDULE_NOOP", "this schedule would change nothing", 422).
		WithDetails(map[string]any{"reason": reason})
}

// ScheduleEntry files an intent against the entry's current version.
//
// THE AUTHORIZATION IS THE SAME VERB THE IMMEDIATE OPERATION TAKES: publish
// needs content:publish, unpublish keeps content:update, exactly as
// SetEntryStatus splits them (ADR-014 §1, step 6). Any other arrangement makes
// this endpoint a way around the human gate — an agent holding content:update
// could schedule a release for five seconds' time and have published without
// ever holding content:publish. The gate has to be the same gate.
func (s *contentService) ScheduleEntry(ctx context.Context, typeName string, id uuid.UUID, in ScheduleEntryInput) (_ EntryScheduleDTO, err error) {
	// Validated before the recorder opens, for SetEntryStatus's reason: until the
	// action is known there is no authorization verb to ask for, and a nonsense
	// value must not reach the point where it picks one.
	if !domain.ValidScheduleAction(in.Action) {
		return EntryScheduleDTO{}, errScheduleActionInvalid(in.Action)
	}
	act := s.activityWrite(ctx, domain.ActivityEntrySchedule, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()

	action := ActionContentUpdate
	if in.Action == domain.ScheduleActionPublish {
		action = ActionContentPublish
	}
	sub, err := s.authorize(ctx, action, id.String(), typeName)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	runAt, err := s.parseRunAt(in.RunAt)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	if err := guardTypeWrite(ct, sub); err != nil {
		return EntryScheduleDTO{}, err
	}
	existing, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	// Scheduling a write is a write, so confinement applies — an editor confined
	// to their own rows must not be able to schedule a release of someone
	// else's.
	if err := guardOwned(ct, sub, existing); err != nil {
		return EntryScheduleDTO{}, err
	}
	act.describe(ct.Fields, existing.Payload)
	if err := guardScheduleUseful(in.Action, existing); err != nil {
		return EntryScheduleDTO{}, err
	}
	// The friendly half of the one-pending rule. The database decides (partial
	// unique index, 000041) and CreateEntrySchedule maps its violation onto the
	// same code; this read is what makes the common case carry the existing
	// run_at instead of a constraint name.
	pending, err := s.repo.GetPendingEntrySchedule(ctx, sub.TenantID, id)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	if pending != nil {
		return EntryScheduleDTO{}, errScheduleExists(pending)
	}
	row := &domain.EntrySchedule{
		ID:            uuid.New(),
		TenantID:      sub.TenantID,
		EntryID:       existing.ID,
		ContentTypeID: ct.ID,
		Action:        in.Action,
		RunAt:         runAt,
		// WHICH VERSION THIS IS ABOUT, and the two actions mean different things
		// by it (domain.EntrySchedule.PinnedVersion, ADR-017 §2).
		//
		// publish pins the working copy the requester was looking at — taken from
		// the row we just read under the same guards, never from the caller: a
		// client that could name the version could pin an approval to content it
		// had not seen, which is the entire property the column exists to
		// provide.
		//
		// unpublish notes the LIVE snapshot's version instead, and nothing
		// enforces it. A retraction takes down what is published; editing the
		// working copy does not change what is published, so a save must not
		// invalidate it.
		PinnedVersion: pinnedVersionFor(in.Action, existing),
		State:         domain.ScheduleStatePending,
		RequestedBy:   sub.ResponsibleUserID(),
		CreatedAt:     s.clock(),
	}
	if err := s.repo.CreateEntrySchedule(ctx, row); err != nil {
		return EntryScheduleDTO{}, err
	}
	return projectSchedule(row), nil
}

// pinnedVersionFor picks the version the schedule records.
//
// The unpublish half reads PublishedVersion, which guardScheduleUseful has
// already guaranteed is a real snapshot: it refuses to file an unpublish for an
// entry that is not published. So this cannot record 0 and pretend it means
// something.
func pinnedVersionFor(action string, e *domain.Entry) int {
	if action == domain.ScheduleActionPublish {
		return e.Version
	}
	return e.PublishedVersion
}

// parseRunAt turns the wire's string into an instant, or explains which of the
// three ways it is wrong.
func (s *contentService) parseRunAt(raw string) (time.Time, error) {
	runAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errScheduleTimeInvalid(raw, "not an RFC3339 timestamp")
	}
	now := s.clock()
	// Strictly in the future. A run_at of "now" is not a schedule, it is a
	// publish written the long way round — and it would land in the past by the
	// time the next tick ran, where it is indistinguishable from a schedule the
	// worker was late for.
	if !runAt.After(now) {
		return time.Time{}, errScheduleTimeInvalid(raw, "must be in the future")
	}
	if runAt.After(now.Add(domain.ScheduleMaxHorizon)) {
		return time.Time{}, errScheduleTimeInvalid(raw, "must be within a year")
	}
	return runAt.UTC(), nil
}

// guardScheduleUseful is the CONTENT_SCHEDULE_NOOP check.
//
// The publish half deliberately mirrors SetEntryStatus's short-circuit
// condition rather than a simpler `status == published`: under ADR-006 an entry
// can be published AND have unreleased edits, and scheduling a release of those
// edits is the ordinary use of this feature. Testing status alone would refuse
// the main case.
func guardScheduleUseful(action string, e *domain.Entry) error {
	if action == domain.ScheduleActionPublish {
		if e.Status == domain.StatusPublished && !e.HasUnpublishedChanges {
			return errScheduleNoop("entry is already published with no unreleased changes")
		}
		return nil
	}
	if e.Status != domain.StatusPublished {
		return errScheduleNoop("entry is not published")
	}
	return nil
}

// CancelEntrySchedule withdraws the pending intent.
//
// THE VERB FOLLOWS THE SCHEDULE, not the endpoint: cancelling a scheduled
// publish takes content:publish, cancelling a scheduled unpublish takes
// content:update. Two authorize calls, and the second is the one that matters —
// without it, an agent holding content:update could cancel a human's approved
// release, which is interference with the ADR-014 §1 gate arriving through the
// only door that was left open. "Not publishing" is a decision about a publish.
//
// The first call is not ceremony either: it is what establishes the subject,
// applies ADR-013 §4's untyped-request rule, and confines an agent to its own
// content type — all before this method reads anything at all.
func (s *contentService) CancelEntrySchedule(ctx context.Context, typeName string, id uuid.UUID) (err error) {
	act := s.activityWrite(ctx, domain.ActivityEntryScheduleCancel, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentUpdate, id.String(), typeName)
	if err != nil {
		return err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return err
	}
	if err := guardTypeWrite(ct, sub); err != nil {
		return err
	}
	existing, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return err
	}
	if err := guardOwned(ct, sub, existing); err != nil {
		return err
	}
	act.describe(ct.Fields, existing.Payload)
	pending, err := s.repo.GetPendingEntrySchedule(ctx, sub.TenantID, id)
	if err != nil {
		return err
	}
	if pending == nil {
		return errScheduleNotFound
	}
	if pending.Action == domain.ScheduleActionPublish {
		if _, err := s.authorize(ctx, ActionContentPublish, id.String(), typeName); err != nil {
			return err
		}
	}
	if _, err := s.repo.CancelEntrySchedule(ctx, sub.TenantID, id); err != nil {
		// The row stopped being pending between the read above and here — a
		// concurrent cancel, or the worker executing it. Either way there is
		// nothing pending to withdraw, which is what this code says.
		if errors.Is(err, apperrors.ErrNotFound) {
			return errScheduleNotFound
		}
		return err
	}
	return nil
}

// GetEntrySchedule returns the entry's newest schedule in any state.
func (s *contentService) GetEntrySchedule(ctx context.Context, typeName string, id uuid.UUID) (_ EntryScheduleDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryRead, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	// SECOND LAYER, and the same one ListActivity spells out. This verb IS
	// content:read, so isReadAction lets a delivery or preview credential
	// through the chokepoint; nothing but this line stands between a public
	// token and the knowledge that a page comes down on Friday. The route is
	// only mounted on the admin router today — which is exactly the kind of fact
	// that stops being true without anyone revisiting this file.
	if sub.PublicDelivery {
		return EntryScheduleDTO{}, apperrors.ErrForbidden
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return EntryScheduleDTO{}, err
	}
	existing, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return EntryScheduleDTO{}, err
	}
	// Confinement: a row this caller does not own answers 404 here for the same
	// reason GetEntry does — the alternative distinguishes "someone else's
	// entry" from "no such id".
	if err := guardOwned(ct, sub, existing); err != nil {
		return EntryScheduleDTO{}, err
	}
	act.describe(ct.Fields, existing.Payload)
	row, err := s.repo.GetLatestEntrySchedule(ctx, sub.TenantID, id)
	if err != nil {
		if errors.Is(err, apperrors.ErrNotFound) {
			return EntryScheduleDTO{}, errScheduleNotFound
		}
		return EntryScheduleDTO{}, err
	}
	return projectSchedule(row), nil
}

// decorateWithSchedule attaches the LIVE-OR-INVALIDATED intent to an admin
// EntryDTO — pending and stale, not pending alone.
//
// stale is here because it is the state that needs a person: the release will
// not happen, and the only party who can re-file it is looking at this entry.
// Dropping the field the moment a schedule went stale would make the marker
// disappear exactly when it started mattering, which is the failure mode
// ADR-017 §2 named and this closes.
//
// A FAILED LOOKUP DOES NOT FAIL THE READ. The schedule is a decoration on a
// read that has already succeeded, and answering 500 for the whole entry
// because a secondary query failed would take an editor's content away over a
// field they may not even be looking at. It is logged, because a decoration
// that silently stops appearing is a feature that silently stops working.
func (s *contentService) decorateWithSchedule(ctx context.Context, sub authn.Subject, dto EntryDTO, entryID uuid.UUID) EntryDTO {
	if sub.PublicDelivery {
		return dto
	}
	live, err := s.repo.GetActionableEntrySchedule(ctx, sub.TenantID, entryID)
	if err != nil {
		log.Printf("content schedule: read actionable schedule for entry %s of tenant %s: %v", entryID, sub.TenantID, err)
		return dto
	}
	return dto.withSchedule(live)
}

// decorateEntriesWithSchedule is decorateWithSchedule for a page: one batched
// lookup instead of one query per row, so a list of N entries costs the same
// single extra round trip GetEntry pays for one. items and entries are
// parallel slices — the same order and length the caller already built the
// page in — because ListActionableEntrySchedules answers by entry id and this
// is where that map gets applied back onto each row's DTO.
//
// It mirrors decorateWithSchedule's two guards. PublicDelivery skips the query
// entirely rather than fetching schedules that withSchedule would immediately
// discard on every DTO in the page (audienceDelivery and audiencePreview both
// set it) — the same admin-only rule, just checked once for the page instead
// of once per row. And a FAILED LOOKUP DOES NOT FAIL THE LIST, for the same
// reason it does not fail a single GET: a page that already read successfully
// must not turn into a 500 over a decoration, logged so the field's silent
// disappearance is not itself silent.
func (s *contentService) decorateEntriesWithSchedule(ctx context.Context, sub authn.Subject, items []EntryDTO, entries []*domain.Entry) []EntryDTO {
	if sub.PublicDelivery || len(entries) == 0 {
		return items
	}
	ids := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	live, err := s.repo.ListActionableEntrySchedules(ctx, sub.TenantID, ids)
	if err != nil {
		log.Printf("content schedule: list actionable schedules for tenant %s: %v", sub.TenantID, err)
		return items
	}
	for i, e := range entries {
		items[i] = items[i].withSchedule(live[e.ID])
	}
	return items
}
