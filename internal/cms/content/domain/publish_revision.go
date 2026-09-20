package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EntryPublishRevision is one RELEASE: the payload that went live, plus the
// four facts about the release itself (ADR-018).
//
// NOT TO BE CONFUSED WITH EntryRevision, which sits in the same package and
// means the opposite half of the same subject. That type is the WORKING COPY at
// some version — every draft save produces one, a publish produces none. This
// type is the LIVE SNAPSHOT at some release — every publish produces one, a
// draft save produces none. An editor asking "what did I type last Tuesday"
// wants the other type; an editor asking "put back what the site was showing in
// February" wants this one, and only this one can answer because only this one
// knows which of the many drafts was the one on the shelf.
//
// STORED AND RESTORABLE, which is where ADR-018 departs from ADR-014 §5. §5
// stored revisions and refused to restore them, on the ground that a restore
// replays a whole old payload through UpdateEntry, which REFUSES unwritable keys
// rather than dropping them, so a restricted role could never restore an entry
// holding a restricted field it may not write. ADR-018 §4 resolves that by
// guarding only the keys whose value would actually CHANGE: replaying a
// restricted field's identical value is not a write to it, so the common case —
// the restricted field was not what went wrong — stops being refused, while an
// actual attempt to move a restricted value still hits errFieldWriteForbidden.
// The bypass ADR-009 refused is still refused.
type EntryPublishRevision struct {
	ID       uuid.UUID
	TenantID string
	EntryID  uuid.UUID

	// RevisionNo is the per-entry ordinal, from 1, allocated at publish time.
	//
	// It is what the API addresses a revision by, and it is deliberately NOT
	// Version: `entries.version` moves on every draft save, so two consecutive
	// releases are v4 and v19 and an editor asked to "restore version 19" has
	// to be told which of the nineteen numbers are real. Retention makes the
	// surviving range start above 1 for a much-published entry; it never leaves
	// a hole in the middle.
	RevisionNo int

	// Payload is the live snapshot verbatim, the value `published_payload` took
	// at this release. It holds every restricted field's value in full, exactly
	// as EntryRevision's comment warns, so §6 masking applies before it reaches
	// any response — projectPublishRevision is the only place allowed to put it
	// on the wire.
	Payload json.RawMessage

	// Version is the entry version this release put on the shelf: the
	// `published_version` the publish landed. It is the join back to
	// EntryRevision's history and to a schedule's pinned version, and it is what
	// a console shows when an editor is working out WHICH draft this was.
	Version int

	// PublishedAt is the instant of THIS release, and is not
	// entries.published_at, which means "first release since the last unpublish"
	// and deliberately survives a re-publish unchanged. Copied from the
	// updated_at the entry row took in the same statement, so the release and
	// the row it produced share one clock reading.
	PublishedAt time.Time

	// PublishedBy is who answers for the release — for a scheduled one, the
	// person who filed the schedule, never the worker. Nil-able on 000031's
	// principle: the source column is nullable, and copying a specific false
	// answer into the column whose whole job is "who released this" is the thing
	// 000031 refused. A reader renders nil as unknown.
	PublishedBy *uuid.UUID

	// Via / ViaScheduleID are the mechanism, the same closed vocabulary as
	// Activity.Via and biconditional in the same way. They are stored here as
	// well as on the activity line because the two answer different questions: a
	// filtered activity stream can be pruned, and this row must still be able to
	// say on its own whether a person pressed the button.
	Via           string
	ViaScheduleID *uuid.UUID

	CreatedAt time.Time
}

// MaxPublishRevisionsPerEntry is the retention cap: an entry keeps its newest
// this-many releases and the publish that exceeds it deletes the oldest, in the
// same transaction (ADR-018 §1).
//
// A PURGE, not a read limit, and that is the deliberate difference from
// EntryRevision's entryRevisionListLimit — which caps what a query returns and
// leaves every row on disk, because 000034 landed with its storage question
// still open. Here the question is answered: what is worth keeping is the recent
// releases, the payloads are full copies of entries that are already quota-bound
// individually, and unbounded growth of full copies is a real cost rather than a
// hypothetical one. Twenty is a judgement, not a measurement — long enough that
// "roll back to before this week's mess" is always available, short enough that
// the storage stays a small multiple of the entry.
//
// The number is deliberately not configurable per tenant. A knob here would mean
// two tenants disagreeing about what "revision 3" refers to over time and a
// support answer that starts with "it depends"; ADR-018 lists per-tenant
// retention as a trigger condition instead.
const MaxPublishRevisionsPerEntry = 20

// RestoreDetails is the Activity.Details payload of an entry.restore line: which
// revision was replayed, when that revision had gone live, and which of its keys
// could not be carried over.
//
// DroppedKeys is the honest half. A revision can be older than the type: fields
// get removed from a content type, and their values in an old snapshot have
// nowhere to land. Those keys are dropped rather than refused — refusing would
// make a whole class of old revisions permanently unrestorable over one field
// nobody uses any more — and dropping silently would be worse than either, so
// the names travel on the activity line and in the response. An empty slice
// marshals as [] rather than being omitted: "nothing was dropped" is a fact
// worth stating positively, since its absence would otherwise be indistinguishable
// from an older writer that did not record it.
func RestoreDetails(revisionNo int, revisionPublishedAt time.Time, droppedKeys []string) json.RawMessage {
	if droppedKeys == nil {
		droppedKeys = []string{}
	}
	b, err := json.Marshal(map[string]any{
		"revision_no":  revisionNo,
		"published_at": revisionPublishedAt,
		"dropped_keys": droppedKeys,
	})
	if err != nil {
		// Unreachable: an int, a time and a []string. Returning nil rather than
		// panicking keeps a marshalling bug from taking down a restore, on
		// ScheduleStaleDetails' reasoning.
		return nil
	}
	return b
}

// PublishOrigin says why a publish happened, so the revision row can record it.
//
// It exists as a type rather than two more parameters on SetEntryPublishState
// because that method is called from the service, the scheduler worker and about
// twenty integration tests; a variadic option of this type lets the two callers
// that know the mechanism say so while the tests that do not care keep compiling
// unchanged. The zero value is a direct, hand-pressed publish, which is both the
// common case and the right default for a caller that has not thought about it.
type PublishOrigin struct {
	Via        string
	ScheduleID *uuid.UUID
}

// Valid reports whether the pair is one the revision table's CHECK will accept —
// a legal mechanism, and a schedule id present exactly when the mechanism is a
// schedule.
func (o PublishOrigin) Valid() bool {
	return ValidActivityVia(o.Via) && (o.Via == ActivityViaSchedule) == (o.ScheduleID != nil)
}
