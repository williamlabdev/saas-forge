// Package domain holds the runtime-dynamic content model: a content type is a
// row-level schema (a set of fields) and an entry is a JSONB document validated
// against that schema at write time. Adding a field is a data change, not a code
// change — one set of tables and one set of endpoints serve any content type.
package domain

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Field types — the canonical set mirrors EntityField.type in the vendored
// admin-app.schema.json (internal/cms/content/contract). A parity test asserts
// AllowedFieldTypes() stays in lockstep with that schema; do not change one
// without the other.
const (
	FieldTypeString   = "string"
	FieldTypeText     = "text"
	FieldTypeRichText = "richtext"
	FieldTypeNumber   = "number"
	FieldTypeBoolean  = "boolean"
	FieldTypeEnum     = "enum"
	FieldTypeDate     = "date"
	FieldTypeDateTime = "datetime"
	FieldTypeFile     = "file"
	FieldTypeRelation = "relation"
)

// AllowedFieldTypes is the legal field_type set, in declaration order.
func AllowedFieldTypes() []string {
	return []string{
		FieldTypeString,
		FieldTypeText,
		FieldTypeRichText,
		FieldTypeNumber,
		FieldTypeBoolean,
		FieldTypeEnum,
		FieldTypeDate,
		FieldTypeDateTime,
		FieldTypeFile,
		FieldTypeRelation,
	}
}

// ValidFieldType reports whether t is a legal field_type.
func ValidFieldType(t string) bool {
	for _, x := range AllowedFieldTypes() {
		if x == t {
			return true
		}
	}
	return false
}

// MaxMultipleElements caps how many values one multi-valued field may hold.
//
// A code constant, not a Quota dimension — same reasoning as MaxUploadBytes:
// this is a platform abuse backstop, not something anyone would price. Making
// it per-plan costs a plans migration plus two composition roots for no benefit
// today.
//
// MaxEntryBytes already bounds the payload, but it bounds BYTES: 256 KiB of
// short tags is roughly forty thousand elements. Harmless for string/enum/number,
// and a self-inflicted denial of service for relation, where every element is an
// existence check.
const MaxMultipleElements = 100

// AllowedMultipleTypes is the set a field may declare `multiple` on.
//
// The excluded types are excluded for different reasons, and the difference
// matters if someone reopens this:
//
//   - boolean — permanently. A multiset of booleans is a count.
//   - date, datetime — for want of a demand and a UI story, the same weak
//     refusal as text. This is deliberately NOT the reason an earlier version of
//     this list gave, which was that the comparison operators emit
//     `(payload ->> key)::numeric` and its text analogues and raise SQLSTATE
//     22P02 against an array. The cast does raise 22P02 — that is executed, not
//     assumed, in repository/multivalue_number_integration_test.go — but a
//     multi-valued field never reaches it: OpsForCardinality leaves such a field
//     only has/nhas, and parseSort refuses to order by it at all. The argument
//     was load-bearing for nothing, and keeping it would make these two look
//     like they were waiting on work the operator layer had already done.
//   - text — shares the string validation arm, so it costs one line to allow.
//     Refused only for want of a demand and a UI story; the weakest refusal here.
//   - file — a gallery is an ORDERED sequence with an aggregate byte budget, and
//     containment queries discard order. It needs its own cardinality decision
//     plus a per-entry asset budget, not this flag. See ADR-005.
//   - richtext — the value is ALREADY a sequence (of blocks). "Multiple rich
//     texts" is a document boundary the grammar cannot see and no renderer or
//     query can honour; a caller who wants two documents wants two fields, or
//     two entries. See ADR-010.
//
// number was moved OUT of this list once the cast argument above was executed
// instead of asserted, and nothing but the flag itself had to change: the write
// path already validates array elements one at a time through validateScalar,
// and containment already types its operand — containmentDoc runs it through
// typedValue and builds {"scores":[7]}, not {"scores":["7"]}. jsonb compares
// numbers as numeric, so 4 and 4.0 are ONE element at query time, which is the
// same answer validatePayload's duplicate check gives at write time. The two
// agreeing is what makes the type safe to hold a set at all.
//
// What a multi-valued number cannot do is what no multi-valued field can:
// range-compare or sort. "How many scored above 5" is an aggregate over
// elements, and it needs its own syntax rather than a widened operator set.
func AllowedMultipleTypes() []string {
	return []string{FieldTypeString, FieldTypeEnum, FieldTypeRelation, FieldTypeNumber}
}

// MultipleAllowedFor reports whether t may carry multiple values.
func MultipleAllowedFor(t string) bool {
	for _, x := range AllowedMultipleTypes() {
		if x == t {
			return true
		}
	}
	return false
}

// Editorial states for an entry. status is orthogonal to Entry.Version:
// version is an optimistic lock (concurrency), status is editorial lifecycle.
// Keep them apart — a version history, if it ever lands, is a third concept.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// AllowedStatuses is the legal status set, in lifecycle order. Kept in lockstep
// with the entries_status_check constraint (migration 000016); a parity test
// asserts the two do not drift.
func AllowedStatuses() []string {
	return []string{StatusDraft, StatusPublished}
}

// ValidStatus reports whether s is a legal editorial state.
func ValidStatus(s string) bool {
	for _, x := range AllowedStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// DefaultLocale is the locale an entry lands in when the tenant never opted
// into localisation. It is a real value rather than an empty string so the
// unique (tenant, translation_group_id, locale) index applies uniformly.
const DefaultLocale = "default"

// localePattern accepts BCP-47-ish tags ("en", "zh-TW") plus DefaultLocale.
// Kept in lockstep with the entries_locale_check constraint (migration 000018).
var localePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,34}$`)

// ValidLocale reports whether s is a usable locale tag.
func ValidLocale(s string) bool { return localePattern.MatchString(s) }

// identPattern constrains content-type names and field keys to safe
// SQL/JSON identifiers. Keys are validated here at definition time so the
// repository can use them as JSONB object keys safely (they are still bound as
// query parameters, never string-concatenated — see the filter builder).
var identPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// ValidName reports whether s is a safe content-type name.
func ValidName(s string) bool { return identPattern.MatchString(s) }

// ValidFieldKey reports whether s is a safe field key.
func ValidFieldKey(s string) bool { return identPattern.MatchString(s) }

// ContentType is a runtime-defined schema: a named, tenant-scoped set of fields.
type ContentType struct {
	ID       uuid.UUID
	TenantID string
	Name     string
	Label    string
	Fields   []Field
	// ReadRoles / WriteRoles / OwnOnlyRoles are DATA-level permission: which
	// tenant roles may read and write the ENTRIES of this type, and which of them
	// see only the entries they created. EMPTY means unrestricted, exactly as on
	// a Field — see type_permission.go for the convention and migration 000027
	// for why the declaration lives on the schema rather than on each entry.
	//
	// MUTABLE, like the field lists and for the same reason: a permission change
	// invalidates no stored value. OwnOnlyRoles is the one that can be refused
	// against stored data, and only when it TIGHTENS — see CountEntriesWithoutAuthor.
	ReadRoles    []string
	WriteRoles   []string
	OwnOnlyRoles []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// FieldByKey returns the field with the given key, if defined.
func (ct *ContentType) FieldByKey(key string) (Field, bool) {
	for _, f := range ct.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Field is one column in a content type's row-level schema.
type Field struct {
	ID            uuid.UUID
	ContentTypeID uuid.UUID
	Key           string
	Type          string
	Label         string
	Required      bool
	// Multiple makes the field hold a JSON array of Type rather than one value —
	// a sibling flag on the existing types, not a type of its own. One flag buys
	// free-form tags (string), a controlled vocabulary (enum) and tags-as-entities
	// (relation); three new field types would buy the same thing while touching
	// the vendored contract's type enum, which four hand-synced copies and a
	// parity test all depend on.
	//
	// IMMUTABLE after creation, enforced in the service. Flipping it invalidates
	// every stored value in one direction — a scalar "ai" is not a legal value for
	// a multi field — and because validatePayload checks the WHOLE document, that
	// does not merely block writes to this field: it makes every entry of the type
	// un-PATCHable, with an error naming a field the caller never touched.
	Multiple   bool
	EnumValues []string
	// ReadRoles / WriteRoles are the tenant roles allowed to see and to send
	// this field's value. EMPTY means unrestricted, which is what every field
	// predating migration 000026 carries — see field_permission.go for the
	// convention, the absence of an owner bypass, and why a public delivery
	// credential matches no non-empty list.
	//
	// MUTABLE, unlike Type and Multiple above. Those are one-way doors because
	// flipping them invalidates stored VALUES; changing who may read a value
	// invalidates nothing, and granting and revoking is the entire feature.
	ReadRoles      []string
	WriteRoles     []string
	RelationEntity string
	// Ordinal is the field's position in its type's definition order, 1-based.
	// It exists because that order is load-bearing — the admin form renders it,
	// and NewArtifact deliberately preserves it rather than sorting — and until
	// migration 000025 it was an accident of the query plan (every field of a
	// type shares one created_at, so ORDER BY created_at was a total tie).
	//
	// Assigned by the repository on insert, never by a caller: it is a position
	// in a sequence the repository owns, and a caller-supplied one would have to
	// answer what happens when two fields claim the same slot.
	Ordinal   int
	CreatedAt time.Time
}

// InEnum reports whether v is one of an enum field's allowed values.
func (f Field) InEnum(v string) bool {
	for _, e := range f.EnumValues {
		if e == v {
			return true
		}
	}
	return false
}

// Entry is a single content document. Payload is stored verbatim as JSONB; the
// service validates it against the content type's fields before it lands here.
type Entry struct {
	ID            uuid.UUID
	TenantID      string
	ContentTypeID uuid.UUID
	// Payload is the WORKING copy — what an editor sees and writes. It is never
	// served publicly; delivery reads PublishedPayload (migration 000020).
	Payload json.RawMessage
	// Version is an optimistic lock — it exists so a concurrent writer can't be
	// silently clobbered. It says nothing about editorial state; see Status.
	//
	// It also KEYS EntryRevision, which is not a second job bolted on: ADR-014
	// §5 chose this counter over a new one precisely because every applied
	// write already bumps it, so (entry_id, version) is unique for free. What
	// it still is not is a count of stored revisions — publish and unpublish
	// advance it without producing one.
	Version int
	// Status is the editorial state (draft | published). New entries start as
	// draft — publishing is always a deliberate act, never a side effect of a
	// write. This is what a public delivery path filters on (ADR-004).
	Status string
	// PublishedPayload is the snapshot delivery serves. Only publishing writes
	// it, which is what makes publish deliberate for EDITS and not just for an
	// entry's first release — before 000020 both states read Payload, so every
	// update to a published entry went live on write (ADR-006).
	//
	// Non-nil exactly when Status == StatusPublished; the DB CHECK enforces it.
	PublishedPayload json.RawMessage
	// PublishedVersion is the Version that was snapshotted — a record of WHICH
	// version is live. It is deliberately not what HasUnpublishedChanges is
	// computed from: version bumps on every write, including one that stores
	// the content the row already had (ADR-006).
	PublishedVersion int
	// HasUnpublishedChanges reports whether the working copy has moved on from
	// the published snapshot — i.e. an editor has saved edits the public cannot
	// see yet. A draft has nothing published, so it is never "unpublished
	// changes"; it is simply unpublished, which IsPublished already tells you.
	//
	// The repository computes this in SQL and every path that returns an
	// existing entry carries it, so it is never stale. It is NOT a method here
	// because the test is `payload IS DISTINCT FROM published_payload`, and the
	// database is the one that can answer it without shipping both payloads to
	// Go: jsonb compares semantically, so content written two ways is not an
	// edit — notably numeric form, where '{"n":1.0}' and '{"n":1}' are equal
	// but their text is not.
	HasUnpublishedChanges bool
	// PublishedAt is set exactly when Status == StatusPublished, and cleared on
	// unpublish. A re-publish deliberately does not move it — but an unpublish
	// clears it, so it means "first released since the last retract", not "first
	// released ever". The DB enforces the same invariant (migration 000016).
	PublishedAt *time.Time
	// Locale is this row's language. One row per locale — a translation
	// publishes independently of its siblings, which is the whole reason the
	// model is not a locale-keyed payload (migration 000018).
	Locale string
	// TranslationGroupID ties an entry to its translations. A standalone entry
	// is simply a group of one. Unique together with Locale per tenant.
	TranslationGroupID uuid.UUID
	// CreatedBy / UpdatedBy / PublishedBy record WHO, and are nil when there is
	// no person to name — rows predating migration 000021, and any writer that is
	// not a human. nil means "not recorded"; a reader must render it as unknown
	// rather than inventing an actor.
	//
	// UpdatedBy tracks the WORKING copy's last write, so it moves with UpdatedAt.
	// PublishedBy describes the SNAPSHOT, exactly as PublishedVersion does: it is
	// written when the snapshot is taken and cleared on retract, because a
	// retracted entry has nothing released and therefore no releaser. The DB
	// enforces the retract half (entries_published_by_snapshot_check).
	CreatedBy   *uuid.UUID
	UpdatedBy   *uuid.UUID
	PublishedBy *uuid.UUID
	// CreatedByKind and CreatedByAgent record WHAT wrote the row, next to
	// CreatedBy's WHO ANSWERS FOR it (ADR-013 §2, migration 000030).
	//
	// The split exists because an agent credential has both, and they are not
	// the same party: CreatedBy holds the principal who minted the credential —
	// so the row has an owner, own_only keeps working, and nothing accumulates
	// as unownable — while these two say the keystrokes were software's and name
	// which. A UI that renders CreatedBy alone is not wrong, only incomplete: it
	// says Alice, when the honest rendering is "Alice's agent content-bot".
	//
	// CreatedByKind is never empty for a row that came from the database
	// (NOT NULL, defaulted to human); CreatedByAgent is non-nil exactly when the
	// kind is agent, which the schema enforces both ways.
	CreatedByKind  string
	CreatedByAgent *string
	// UpdatedByKind and UpdatedByAgent are the same fact about the LAST write
	// that CreatedByKind/CreatedByAgent are about the first (ADR-014 §4,
	// migration 000031). They move with UpdatedBy, which is why the three are
	// always assigned together — a stale kind beside a fresh UpdatedBy would
	// attribute a bot's edit to the person who made the one before it.
	//
	// UpdatedByKind is a POINTER where CreatedByKind is a plain string, and the
	// asymmetry is the point: 000030 could backfill created_by_kind because no
	// agent could have written those rows, while rows predating 000031 may well
	// have been last touched by one. nil is "not recorded" — the third state
	// ADR-014 §4 requires a reader to render as unknown rather than falling back
	// to naming UpdatedBy's human.
	UpdatedByKind  *string
	UpdatedByAgent *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ActorKind values as stored in entries.created_by_kind (migration 000030).
//
// A third spelling of the same vocabulary — the wire has jwt.Kind*, identity
// has authn.ActorKind*, storage has these. They are separate because the layers
// are: domain must not import an auth package to name a column value. The three
// are pinned equal by tests at the layers that can see more than one
// (actor_kind_pairing_test.go); the schema's CHECK constraint is the fourth
// copy and the one that has the last word.
const (
	ActorKindHuman   = "human"
	ActorKindAgent   = "agent"
	ActorKindService = "service"
)

// WriteActor is the last-write trio (ADR-014 §4) travelling as ONE value.
//
// It exists because of the bulk schema mutations. Entry writes stamp the trio
// through recordUpdateProvenance, which assigns all three onto the entry for a
// stated reason: the schema refuses every half-written combination, so a call
// site able to set one without the others will eventually do it. A repository
// method taking three separate parameters is exactly such a call site, and
// DeleteField/RenameField needed the trio threaded in (william 0806) — so the
// trio became a type rather than three arguments in a row.
//
// A zero value is not "no actor". Kind is checked against the same CHECK
// constraint the columns carry, so a caller that forgets to fill this is
// refused by the database rather than quietly recorded as an empty string.
type WriteActor struct {
	// Kind is one of the ActorKind constants above.
	Kind string
	// UserID is who ANSWERS for the write — for an agent credential, the
	// minting principal, never nil when Kind is agent (000031's CHECK).
	UserID *uuid.UUID
	// AgentID names the bot, and is non-nil exactly when Kind is agent.
	AgentID *string
}

// IsPublished reports whether the entry is publicly readable.
func (e *Entry) IsPublished() bool { return e.Status == StatusPublished }
