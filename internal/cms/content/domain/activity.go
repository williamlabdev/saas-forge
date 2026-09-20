package domain

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Activity is one append-only line of "who did what to which thing, and did it
// work" (ADR-014 §3). It is the console's answer to the question the CMS could
// not answer at all before it existed: an agent wrote something at 10:03 and
// the only trace was the row it left behind.
//
// IT DOES NOT STORE PAYLOADS, and that is the division of labour with §5's
// revisions: this table answers "who did what", revisions answer "what the
// content became". ChangedKeys carries key names and never values — the moment
// values live here too, this has quietly become a second version history with
// none of the retention rules of the first, and a copy of every restricted
// field's value in a table with no field-level masking. Title is the one
// denormalised scrap of content, and it is fenced accordingly: see TitleFor.
type Activity struct {
	ID         uuid.UUID
	TenantID   string
	OccurredAt time.Time

	// ActorKind / ActorUserID / ActorAgentID are the trio §4's three-state
	// rendering needs. They travel together for the reason provenanceOf states
	// about the entry columns: a kind without its agent id, or an agent line
	// with nobody answerable for it, is a half-written fact the reader cannot
	// render honestly.
	//
	// ActorUserID is the party who ANSWERS for the action, not necessarily who
	// typed: for an agent credential it is the principal who minted it. nil is
	// "no person to name" — a delivery or preview credential — and a reader must
	// render it as the service it is, never as a person.
	ActorKind    string
	ActorUserID  *uuid.UUID
	ActorAgentID *string

	// Action is one of the ActivityAction* constants below.
	Action string

	// TargetType is the content type name the action concerned, empty when it
	// concerned none. TargetEntryID is set for the actions that name one entry.
	//
	// TargetTitle is a human-readable label for that entry, denormalised at
	// write time on purpose: the entry may be deleted, and an activity stream of
	// bare uuids is one nobody reads. It is EMPTY whenever no unrestricted text
	// field could supply one — see TitleFor.
	TargetType    string
	TargetEntryID *uuid.UUID
	TargetTitle   string

	// Outcome is ActivityOutcomeSuccess or ActivityOutcomeDenied; ErrorCode
	// carries the refusal's stable code and is empty on success.
	Outcome   string
	ErrorCode string

	// ChangedKeys names the payload keys this action altered. KEYS ONLY — see
	// the type comment. Empty for actions that change no payload (reads,
	// refusals, publish/unpublish, deletes).
	ChangedKeys []string

	// Via names the MECHANISM that carried the action out, empty for the
	// ordinary case of a request the actor made themselves. The only other
	// value today is ActivityViaSchedule (ADR-017): the actor approved the
	// release earlier and a worker performed it when the moment came.
	//
	// This is not the payload/details column the type comment refuses. It is a
	// closed vocabulary — the CHECK in migration 000041 admits exactly '' and
	// 'schedule' — precisely so it cannot drift into one: a free-text column
	// here would be filled with content by the third caller who needed to
	// "just note one thing", and then this table would hold values with no
	// field-level masking over them.
	//
	// It exists because the alternative readings are both wrong. Omitting the
	// scheduled publishes leaves an activity stream that covers half the
	// releases; including them unmarked tells an operator that a person
	// published at 03:00 when that person was asleep and had approved it on
	// Friday.
	Via string

	// ViaScheduleID is the content_entry_schedules row that caused this, set
	// exactly when Via is ActivityViaSchedule (biconditional CHECK, 000041).
	//
	// It is a breadcrumb, not a foreign key: activity outlives the schedule,
	// which cascades away with its entry. A reader that cannot resolve the id
	// has learned the true thing — the schedule is gone.
	ViaScheduleID *uuid.UUID

	// Details is structured JSON describing the ACTION — never content values,
	// for the reason the type comment gives about ChangedKeys. nil when the
	// action has nothing structured to say, which is every action but
	// entry.schedule.stale today.
	Details json.RawMessage
}

// Activity mechanisms — the values Activity.Via may take. Kept in lockstep with
// the content_activity_via_check constraint (000041).
const (
	// ActivityViaDirect is the zero value: the actor made the request that
	// produced this line. It is spelled as a constant so a reader does not have
	// to guess what an empty Via means.
	ActivityViaDirect = ""
	// ActivityViaSchedule means a scheduler worker performed the action on the
	// authority of a schedule the actor filed earlier (ADR-017 §4).
	ActivityViaSchedule = "schedule"
)

// ValidActivityVia reports whether v is a legal mechanism.
func ValidActivityVia(v string) bool {
	return v == ActivityViaDirect || v == ActivityViaSchedule
}

// Activity actions. The vocabulary is fixed here rather than derived from the
// call sites, so that adding a call site cannot quietly invent a verb nobody
// has to render, and so the structural test in ADR-014 §驗證計畫第 6 條 has
// something to check the ADR-013 §5 tool list AGAINST.
//
// Naming is by PLATFORM action, not by tool: a human pressing publish and an
// agent calling cms_update_entry land in the same stream and must be comparable
// there. The mapping from ADR-013 §5's nine tools onto these lives in the test,
// as a hand-written literal taken from that ADR — deriving it from this list
// would be the log certifying its own completeness, which §驗證計畫第 6 條
// forbids in as many words.
//
// DELIBERATELY ABSENT: media, webhooks, usage, and tenant administration. Those
// are not omissions to be filled in later by whoever notices — the dominating
// rule binds "what an agent can do", and an agent cannot reach any of them by
// construction (a request naming no content type is refused at authorize(),
// ADR-013 §4). Adding them is additive and needs its own reason; not having
// them does not weaken the rule.
const (
	// Entry actions. Every one of these except the reads is emitted on success
	// as well as on refusal — see the recorder in the service package for why
	// the reads are refusal-only.
	ActivityEntryCreate       = "entry.create"
	ActivityEntryRead         = "entry.read"
	ActivityEntryList         = "entry.list"
	ActivityEntryUpdate       = "entry.update"
	ActivityEntryDelete       = "entry.delete"
	ActivityEntryPublish      = "entry.publish"
	ActivityEntryUnpublish    = "entry.unpublish"
	ActivityEntryTranslations = "entry.translations"
	// ActivityEntryAttribution is reading who last changed each field of one
	// entry (§6, step 4) — a read of the RECORD, not of the content, which is
	// why it is its own verb rather than another entry.read. Filing it under
	// entry.read would tell an operator that an agent was refused the entry when
	// what it was refused was the answer to "who did this".
	ActivityEntryAttribution = "entry.attribution"
	// ActivityEntrySchedule / ActivityEntryScheduleCancel are FILING and
	// WITHDRAWING an intent (ADR-017), not performing one. They are separate
	// verbs from entry.publish for the reason ActivityEntryAttribution is
	// separate from entry.read: an operator reading "published at 14:00" when
	// what happened at 14:00 was "asked for this to publish on Monday" has been
	// told something false about the state of the site.
	//
	// The execution, when Monday comes, is an ordinary entry.publish /
	// entry.unpublish line carrying Via = ActivityViaSchedule. So the stream
	// holds two rows for one release — the approval and the release — which is
	// the shape the question "when was this approved, and by whom" actually
	// needs.
	ActivityEntrySchedule       = "entry.schedule"
	ActivityEntryScheduleCancel = "entry.schedule.cancel"
	// ActivityEntryScheduleStale is the intent being INVALIDATED — the working
	// copy moved past the version the schedule pinned, so it will not run
	// (ADR-017 §2).
	//
	// It is a line rather than a silent state change because invalidation is the
	// one thing in this feature that happens TO an editor rather than because of
	// them: the person whose save killed the schedule is usually not the person
	// who filed it, and neither of them is told. Without this row the only
	// evidence is a state column nobody was watching, and the question "why
	// didn't it go live on Monday" has no answer at all.
	//
	// Details carries the three facts that make the row actionable: which
	// schedule, the version it approved, and the version that overtook it.
	ActivityEntryScheduleStale = "entry.schedule.stale"

	// ActivityEntryRestore is an old release being replayed into the WORKING
	// COPY (ADR-018 §3). It is deliberately not entry.update, and deliberately
	// not entry.publish.
	//
	// Not entry.update, because an operator reading "updated at 14:00" has been
	// told an editor typed something; what happened is that someone reinstated a
	// payload from months ago, and the interesting question afterwards — which
	// revision, and was anything lost on the way — has nowhere to live under a
	// verb whose Details mean nothing. Not entry.publish, because a restore does
	// NOT go live: it writes the draft and leaves the shelf alone, so filing it
	// as a publish would tell an operator the site changed when it did not.
	//
	// Details carries the three facts that make the row actionable:
	// RestoreDetails' revision_no, the published_at that revision went live at,
	// and any keys the current type no longer defines and so could not be
	// carried over.
	ActivityEntryRestore = "entry.restore"

	// ActivityEntryReviewChangesRequested is a reviewer sending an entry back
	// (ADR-014 Amendment: review decisions). It is deliberately not
	// entry.publish's refusal path and not entry.update: nothing about the
	// entry changed and nothing was released, so neither verb's Details shape
	// fits. It is the one activity verb this feature adds — the queue's
	// "sent back" state is derived (Entry.Version == the decision's
	// EntryVersion), not itself an activity line.
	//
	// Details carries ReviewDecisionDetails' one fact: the reason, so the
	// stream shows WHY without the title (a payload value) carrying it.
	ActivityEntryReviewChangesRequested = "entry.review.changes_requested"

	// Schema actions. Reading a type's declaration is what cms_describe does and
	// is therefore in the vocabulary; the write verbs are here because a schema
	// change is the one thing that can make every entry of a type invalid at
	// once, which is worth a line whoever caused it.
	ActivityTypeRead      = "type.read"
	ActivityTypeList      = "type.list"
	ActivitySchemaPlan    = "schema.plan"
	ActivitySchemaApply   = "schema.apply"
	ActivitySchemaWrite   = "schema.write"
	ActivitySchemaPropose = "schema.propose"
	// ActivitySchemaProposalRead is a proposer looking up the proposal it filed
	// (000038). It is its own verb for the ActivityEntryAttribution reason: a
	// read filed under schema.propose would tell an operator that a proposal was
	// FILED — and a refused one would say an agent tried to change the schema,
	// when what it was refused was the answer to "was mine approved yet".
	ActivitySchemaProposalRead = "schema.proposal.read"
)

// AllActivityActions is the vocabulary, in declaration order.
//
// This is the log's own enumeration. It is fine for validation and for
// rendering; it is NOT a valid yardstick for "is the vocabulary complete",
// which is why §驗證計畫第 6 條 takes that yardstick from ADR-013 §5 instead.
func AllActivityActions() []string {
	return []string{
		ActivityEntryCreate,
		ActivityEntryRead,
		ActivityEntryList,
		ActivityEntryUpdate,
		ActivityEntryDelete,
		ActivityEntryPublish,
		ActivityEntryUnpublish,
		ActivityEntryTranslations,
		ActivityEntryAttribution,
		ActivityEntrySchedule,
		ActivityEntryScheduleCancel,
		ActivityEntryScheduleStale,
		ActivityEntryRestore,
		ActivityEntryReviewChangesRequested,
		ActivityTypeRead,
		ActivityTypeList,
		ActivitySchemaPlan,
		ActivitySchemaApply,
		ActivitySchemaWrite,
		ActivitySchemaPropose,
		ActivitySchemaProposalRead,
	}
}

// ValidActivityAction reports whether a is in the vocabulary.
func ValidActivityAction(a string) bool {
	for _, x := range AllActivityActions() {
		if x == a {
			return true
		}
	}
	return false
}

// FieldAuthor is the last recorded writer of ONE payload key (ADR-014 §6, the
// per-field attribution the release screen shows beside each changed field).
//
// It is derived from the activity record and stores nothing new: ChangedKeys
// already names the keys a write altered, and the actor trio already says who
// altered them. This type is that join, one row per key.
//
// The trio travels together for the reason Activity's own comment gives: a kind
// without its agent id, or an agent line with nobody answerable for it, is a
// half-written fact a reader cannot render honestly. A key with NO author is
// not represented here at all — absence is the "unknown" the ADR requires, and
// the one rendering it forbids is falling back to the entry's updated_by.
type FieldAuthor struct {
	Key          string
	ActorKind    string
	ActorUserID  *uuid.UUID
	ActorAgentID *string
	OccurredAt   time.Time
}

// Activity outcomes. Two, because §3 asks for two: it happened, or it was
// refused. A 5xx is neither — nothing was decided and nothing was done — and
// the recorder drops those rather than filing an outage under "denied".
const (
	ActivityOutcomeSuccess = "success"
	ActivityOutcomeDenied  = "denied"
)

func ValidActivityOutcome(o string) bool {
	return o == ActivityOutcomeSuccess || o == ActivityOutcomeDenied
}

// ActivityTitleMaxRunes bounds the denormalised label. Runes, not bytes,
// because the first tenant to need this writes Chinese and a byte slice would
// cut a character in half.
const ActivityTitleMaxRunes = 120

// TitleFor derives the human-readable label for one entry, or "" when no field
// may supply one.
//
// TWO FENCES, and both are load-bearing:
//
//  1. READ-RESTRICTED FIELDS ARE SKIPPED. The activity stream carries no
//     field-level masking — it is read by whoever may read the stream, and the
//     row is written once, at a moment with a different caller and a different
//     role than whoever reads it later. Sourcing a title from a restricted
//     field would copy that field's value into a table where the restriction
//     does not apply, which is the same leak §6's diff had to be fenced against
//     and arrives here through a different door.
//  2. ONLY string/text FIELDS, AND NOT MULTI-VALUED ONES. A number or a date
//     makes a label nobody recognises; an array makes one nobody can read. When
//     nothing qualifies the answer is "", and the console renders the id — the
//     honest degradation, and the reason this returns a value rather than
//     refusing.
//
// Field order is the type's declared order, so the label is stable for a type
// rather than depending on JSON key iteration.
func TitleFor(fields []Field, payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return ""
	}
	for _, f := range fields {
		if f.Multiple || len(f.ReadRoles) > 0 {
			continue
		}
		if f.Type != FieldTypeString && f.Type != FieldTypeText {
			continue
		}
		raw, ok := doc[f.Key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		return truncateRunes(s, ActivityTitleMaxRunes)
	}
	return ""
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// ChangedKeysBetween names the top-level keys whose values differ between two
// entry payloads. Keys only, and both directions — a key dropped from the
// document changed just as much as one added.
//
// The comparison is SEMANTIC, through the JSON decoder, for the reason
// unpublishedChangesExpr gives for using IS DISTINCT FROM on jsonb: the same
// value re-serialised is not a change, and a byte comparison would report one
// on every write that happened to reorder keys.
func ChangedKeysBetween(before, after json.RawMessage) []string {
	b := decodeObject(before)
	a := decodeObject(after)
	seen := map[string]bool{}
	var out []string
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameJSONValue(bv, av) {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]json.RawMessage{}
	}
	return out
}

// sameJSONValue compares two encoded values by their decoded shape.
//
// Known limit, stated because it decides where this may be used: integers
// beyond float64's 53-bit mantissa collapse together here while Postgres
// numeric keeps them apart. That cannot make this disagree with the database
// about anything the service stores — validateAndNormalize puts every payload
// through the same float64 round-trip before it is written — but it is not a
// general-purpose jsonb equality.
func sameJSONValue(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return string(a) == string(b)
	}
	an, err1 := json.Marshal(av)
	bn, err2 := json.Marshal(bv)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(an) == string(bn)
}

// SchemaApplyDetails is the Activity.Details payload of a schema.apply line
// filed through a proposal approval (缺口計畫 5.1, ADR-013 §3 step 8): which
// proposal it came from, which of the stored plan's step indices actually ran,
// and whether that was fewer than the plan would have run in full.
//
// Every proposal-driven schema.apply carries this, full approval and partial
// alike — unlike RestoreDetails' dropped_keys, there is no "nothing to say"
// case here worth omitting the object for: appliedSteps is the answer to "what
// ran" whether or not it happens to equal the full plan, and a reader should
// not have to fall back to re-deriving it from the proposal row's own
// applied_steps column when this line is right there. A direct ApplySchema
// call (no proposal in the loop) never calls this, so its schema.apply lines
// keep carrying nil Details exactly as before — proposalID has no meaning
// there.
func SchemaApplyDetails(proposalID uuid.UUID, appliedSteps []int, partial bool) json.RawMessage {
	if appliedSteps == nil {
		appliedSteps = []int{}
	}
	b, err := json.Marshal(map[string]any{
		"proposal_id":   proposalID,
		"applied_steps": appliedSteps,
		"partial":       partial,
	})
	if err != nil {
		// Unreachable: a uuid, an []int and a bool. nil on the RestoreDetails
		// precedent — a marshalling bug here must not take an approval down.
		return nil
	}
	return b
}
