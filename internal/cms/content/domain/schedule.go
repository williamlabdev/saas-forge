package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EntrySchedule is one recorded intent: "at RunAt, put this entry into the
// state Action names, on RequestedBy's authority, against the version they were
// looking at" (ADR-017).
//
// IT IS NOT A JOB. The distinction matters at every field below. A job is work
// that must eventually happen and is retried until it does; this is a decision
// a person made in advance, which the system may quite properly DECLINE to
// carry out — because the content moved on (PinnedVersion), because someone
// published by hand first (ScheduleStateSuperseded), or because the person
// changed their mind (ScheduleStateCancelled). Every one of those is a correct
// outcome, not a failure to process.
//
// The consequence is that a schedule never re-enters ScheduleStatePending. All
// five exits are terminal, and a stale one is NOT rescheduled automatically:
// ADR-014 §1 makes publish a human gate, and quietly re-aiming an approval at a
// version nobody approved is precisely the laundering that gate exists to stop.
type EntrySchedule struct {
	ID       uuid.UUID
	TenantID string

	// EntryID and ContentTypeID together are how every read path in this
	// codebase keys an entry. The type id is denormalised onto the row so the
	// worker — which starts from a schedule and nothing else — can reach the
	// entry without a lookup that exists for no other caller. An entry cannot
	// change type, so the copy cannot go stale (migration 000041).
	EntryID       uuid.UUID
	ContentTypeID uuid.UUID

	// Action is ScheduleActionPublish or ScheduleActionUnpublish — the same two
	// operations the immediate API offers, deliberately. A schedule that could
	// express a state change nobody may perform directly would be a way around
	// the authz split between content:publish and content:update.
	Action string

	RunAt time.Time

	// PinnedVersion records WHICH VERSION THIS SCHEDULE IS ABOUT, and what it
	// means — and whether anything enforces it — depends on Action.
	//
	// FOR publish IT IS A PIN. It is Entry.Version as it stood when the schedule
	// was filed: the version the requester approved. It is checked twice — once
	// when the working copy is updated (the pending schedule goes stale in the
	// same transaction as the write) and again by the worker before it acts. Two
	// checks rather than one because neither alone is enough: the first gives
	// the editor immediate feedback, the second closes the window where a write
	// lands between the poll and the publish.
	//
	// FOR unpublish IT IS A NOTE, and nothing reads it as a condition. It holds
	// Entry.PublishedVersion — the version of the SNAPSHOT the retraction will
	// take down. An unpublish retracts what is live; editing the working copy
	// does not change what is live, so a save must not invalidate it. Pinning it
	// would mean a typo fix on Tuesday silently cancels Friday's takedown, and a
	// takedown is a stop-the-bleeding action (ADR-014 §1) — the one kind of
	// schedule that must not fail closed. See ADR-017 §2.
	PinnedVersion int

	State string

	// RequestedBy is the person who answers for the release. The worker stamps
	// it into Entry.PublishedBy, so provenance names the approver and never the
	// process. There is no agent or service spelling here: only a human reaches
	// this type (ADR-014 §1, ADR-017 §6).
	RequestedBy uuid.UUID

	CreatedAt time.Time

	// ExecutedAt is set exactly when the operation actually ran — states
	// ScheduleStateDone and ScheduleStateFailed, and nothing else. A cancelled
	// or superseded row has no execution instant because nothing executed, and
	// saying so with NULL beats recording the moment a clerk changed a state and
	// calling it a publish time.
	ExecutedAt *time.Time

	// Error carries the failure's cause, empty unless State is
	// ScheduleStateFailed (biconditional CHECK, migration 000041).
	Error string
}

// Schedule actions. Kept in lockstep with content_entry_schedules_action_check.
const (
	ScheduleActionPublish   = "publish"
	ScheduleActionUnpublish = "unpublish"
)

// AllScheduleActions is the legal action set, in lifecycle order.
func AllScheduleActions() []string {
	return []string{ScheduleActionPublish, ScheduleActionUnpublish}
}

// ValidScheduleAction reports whether a is a legal action.
func ValidScheduleAction(a string) bool {
	for _, x := range AllScheduleActions() {
		if x == a {
			return true
		}
	}
	return false
}

// Schedule states. Kept in lockstep with content_entry_schedules_state_check.
//
// The five terminal states are NOT interchangeable and must not be collapsed
// into a single "not pending". Each answers a different question the editor
// asks when a post did not appear:
//
//	done       — it ran, the content is live.
//	stale      — the approved version was superseded by an edit; nobody's
//	             approval was on the content by the time the moment came, so
//	             nothing was published. Reschedule to release the new version.
//	cancelled  — a person withdrew it.
//	superseded — a person did the operation by hand before the moment came, so
//	             the schedule had nothing left to do.
//	failed     — it ran and the operation itself errored. Error says why.
const (
	ScheduleStatePending    = "pending"
	ScheduleStateDone       = "done"
	ScheduleStateStale      = "stale"
	ScheduleStateCancelled  = "cancelled"
	ScheduleStateSuperseded = "superseded"
	ScheduleStateFailed     = "failed"
)

// AllScheduleStates is the legal state set, pending first.
func AllScheduleStates() []string {
	return []string{
		ScheduleStatePending,
		ScheduleStateDone,
		ScheduleStateStale,
		ScheduleStateCancelled,
		ScheduleStateSuperseded,
		ScheduleStateFailed,
	}
}

// ValidScheduleState reports whether s is a legal state.
func ValidScheduleState(s string) bool {
	for _, x := range AllScheduleStates() {
		if x == s {
			return true
		}
	}
	return false
}

// ScheduleTargetStatus maps an action onto the editorial state it produces.
//
// It exists so the mapping is written once. The worker, the service's
// no-op check and the activity verb all need it, and three hand-written
// `if action == "publish"` branches are three places for the fourth caller to
// get it backwards.
func ScheduleTargetStatus(action string) string {
	if action == ScheduleActionPublish {
		return StatusPublished
	}
	return StatusDraft
}

// ScheduleMaxHorizon bounds how far ahead a schedule may be filed.
//
// A year is not a technical limit — the row would sit there quite happily for
// ten. It is a limit on how long a version stays pinned: PinnedVersion promises
// that what goes live is what a person approved, and an approval nobody has
// looked at since 2027 is not meaningfully an approval. Anything beyond this is
// almost always a typo in the year, which is the case the bound actually
// catches.
const ScheduleMaxHorizon = 365 * 24 * time.Hour

// PinsVersion reports whether Action's schedule is invalidated by an edit to
// the working copy.
//
// It is a function and not an `action == publish` at each site for
// ScheduleTargetStatus's reason: the asymmetry is a ruling (ADR-017 §2), it is
// read in three places — the SQL invalidation, the worker's re-check, and the
// tests that pin the ruling — and the fourth caller to spell it by hand is the
// one that gets it backwards.
func (s *EntrySchedule) PinsVersion() bool { return s.Action == ScheduleActionPublish }

// ScheduleStaleDetails is the Activity.Details payload of an
// entry.schedule.stale line: which schedule died, the version it approved, and
// the version that overtook it.
//
// THE KEYS ARE DUPLICATED IN SQL — markSchedulesStale builds the same object in
// the transaction that does the invalidating, because the row must not exist
// unless the write it describes committed. This is the Go spelling, used by the
// worker's own stale path, and the two are kept in lockstep by
// TestSchedule_StaleActivityKeysMatchTheGoSpelling.
func ScheduleStaleDetails(scheduleID uuid.UUID, pinned, current int) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"schedule_id":    scheduleID,
		"pinned_version": pinned,
		"entry_version":  current,
	})
	if err != nil {
		// Unreachable: the map holds a uuid and two ints. Returning nil rather
		// than panicking keeps a marshalling bug from taking down a publish.
		return nil
	}
	return b
}
