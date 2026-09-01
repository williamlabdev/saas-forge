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
