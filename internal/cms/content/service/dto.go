package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// FieldDTO mirrors EntityField in admin-app.schema.json so a GET /types/{name}
// response lines up field-for-field with the frontend contract.
type FieldDTO struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required"`
	// Multiple has no omitempty, matching Required: on a bool it would make
	// `false` indistinguishable from "this server does not know about the flag",
	// and a client that has to guess will guess wrong.
	Multiple   bool     `json:"multiple"`
	EnumValues []string `json:"enum_values,omitempty"`
	// ReadRoles / WriteRoles are the tenant roles allowed to see and to send this
	// field's value; absent means unrestricted.
	//
	// The DECLARATION is public to anyone who may read the schema, and only the
	// VALUES are restricted. That is deliberate: an editor whose form is missing
	// a field needs to be able to find out that the field exists and who owns it,
	// or the answer to "where did the salary box go" is a support ticket. Nothing
	// leaks — a field key was already visible to every audience that can call
	// GET /types.
	ReadRoles  []string `json:"read_roles,omitempty"`
	WriteRoles []string `json:"write_roles,omitempty"`
	// Description is the author's own sentence about the field. Documentation
	// rather than a rule, and the admin form renders it as the ONE helper line
	// under the control — the same line the value rules below are composed
	// into, which is why it stopped being dropped at import (migration 000046).
	Description string `json:"description,omitempty"`
	// The constraint attributes. `unique` has no omitempty, for Multiple's
	// reason; the other four are absent when unset because absent IS unset —
	// there is no third state a client could confuse it with.
	Unique  bool     `json:"unique"`
	Format  string   `json:"format,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	// Readable / Writable are what the DECLARATION means FOR THE CALLER who
	// asked — not part of the schema, and deliberately not in the artifact or in
	// the generated EntityField contract. read_roles is a fact about the field;
	// these two are facts about this request.
	//
	// They exist because a client cannot work them out and must not try. The
	// admin app does not even know its own tenant role (it sends X-User-Id,
	// X-Tenant-Id and X-User-Roles — never X-Tenant-Role), and a client that
	// re-derived the rule would be a second copy of an authorization decision
	// that has already changed once today (reading became a precondition for
	// writing). One decision point, published as an answer.
	//
	// A client needs them for a reason that is NOT security — the server refuses
	// regardless. It needs them so it does not SEND a field it may not write:
	// a form built from the schema round-trips every key, which turns "this
	// field is not yours" into a failed save of the whole record, or worse (see
	// canWriteField). No omitempty: `false` is the interesting value, and a
	// client that cannot tell false from absent will guess wrong.
	//
	// SCOPE: these answer the FIELD question only. Writable is true for a viewer,
	// who holds no content:update at all — the verb was already decided by
	// authorize() before this projection ran, and folding it in here would put
	// two different refusals behind one boolean. The imprecision is benign in the
	// direction that matters: a viewer is refused the whole write with a reason
	// they can understand ("you may not edit"), which is exactly the confusion
	// these flags exist to prevent at the FIELD level. Hiding edit affordances
	// from a viewer is the client's own job and a separate gap — it does not
	// currently know its tenant role at all.
	Readable bool `json:"readable"`
	Writable bool `json:"writable"`
	// Supported lists the filter operators this field accepts (ADR-013 §6).
	//
	// It was reachable before only by GETTING IT WRONG: the same list is the
	// `supported` detail on CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD, so a caller
	// had to send a refused filter to learn which filters exist. That is a poor
	// API for a person and an unusable one for an agent, which has to plan its
	// query before it sends one.
	//
	// Derived, not declared — so it is deliberately absent from the schema
	// artifact and from ArtifactField. The artifact is a state document that
	// round-trips through git; a value nobody can edit and everybody must
	// recompute would be the first thing in it able to go stale.
	//
	// It answers the FIELD's grammar only, exactly as read_roles is a fact about
	// the field. Whether THIS caller may filter on it is the separate question
	// Readable already answers — an unreadable field cannot be filtered at all —
	// and folding that in would put two different refusals behind one list, the
	// mistake the Writable note above declines to make. No omitempty: rich text
	// supports NO operator, and `[]` is the answer that says so.
	Supported []repository.Op `json:"supported"`
	// RelationEntity stays last so the JSON shape keeps the field order the
	// contract lists.
	RelationEntity string `json:"relation_entity,omitempty"`
	// Component is the NAME of the component a `component` field takes its
	// shape from (ADR-020). The sub-fields are not inlined here: a type's
	// schema says which component, GET /components/{name} says what it is.
	Component string `json:"component,omitempty"`
	// Components is the allowed-list of a `dynamiczone` field: which components
	// its items may name under "__component" (ADR-020 Amendment 1). Same
	// bargain as Component — names only, and GET /components/{name} says what
	// each one is — which is what keeps a page-builder type's schema readable
	// when it accepts a dozen blocks.
	Components []string `json:"components,omitempty"`
}

type ContentTypeDTO struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Label string    `json:"label,omitempty"`
	// ReadRoles / WriteRoles / OwnOnlyRoles are the DATA-level declaration
	// (migration 000027): which roles may read and write this collection's
	// entries, and which of them see only their own. Absent means unrestricted.
	//
	// Public to anyone who may read the schema, exactly as the field lists are,
	// and for the same reason: an editor whose collection came back empty needs
	// to be able to find out that it is a permission and whose it is, or the
	// answer to "where did my articles go" is a support ticket.
	ReadRoles    []string   `json:"read_roles,omitempty"`
	WriteRoles   []string   `json:"write_roles,omitempty"`
	OwnOnlyRoles []string   `json:"own_only_roles,omitempty"`
	Fields       []FieldDTO `json:"fields"`
	// Readable / Writable / OwnOnly are what the declaration means FOR THIS
	// CALLER — facts about the request, not about the schema, so they are absent
	// from the artifact and from the generated contract for the same reason the
	// field-level pair is.
	//
	// Writable ALSO FOLDS IN THE VERB, and that is the difference between this
	// pair and FieldDTO's. A field cannot answer the verb question without
	// putting two unrelated refusals behind one boolean; a type can, because "may
	// you write entries of this collection" is exactly the question the verb
	// answers too. It closes the gap FieldDTO.Writable documents: a viewer holds
	// no content:update, so every field of every type came back writable and the
	// admin app rendered an editable form for a caller who could not save it. A
	// client hides the edit affordance on THIS flag; the per-field one then says
	// which boxes within an editable form are theirs.
	//
	// No omitempty on any of the three: false is the interesting value, and a
	// client that cannot tell false from absent will guess wrong.
	Readable bool `json:"readable"`
	Writable bool `json:"writable"`
	// OwnOnly tells the caller their view of this collection is confined to
	// entries they authored. It is not a refusal and never causes one — it exists
	// so a short list reads as "yours" rather than as an outage.
	OwnOnly   bool      `json:"own_only"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type EntryDTO struct {
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`
	// Data is the working copy for an admin audience and the published snapshot
	// for a delivery audience — see EntryAudience.
	Data json.RawMessage `json:"data"`
	// Version is the optimistic-lock counter of the copy in Data: the working
	// copy's for an admin (usable as If-Match), the snapshot's PublishedVersion
	// for delivery. It must track Data, because a counter that moves while Data
	// does not is a side channel announcing unpublished edits (OD2-023 F2).
	Version int `json:"version"`
	// Status is the editorial state; PublishedAt is present only when published.
	Status string `json:"status"`
	// HasUnpublishedChanges tells an editor that saved edits are not live yet.
	// Admin audience only: it is editorial state, and a public reader has no
	// business learning that unreleased edits exist.
	HasUnpublishedChanges bool       `json:"has_unpublished_changes,omitempty"`
	Locale                string     `json:"locale"`
	TranslationGroupID    uuid.UUID  `json:"translation_group_id"`
	PublishedAt           *time.Time `json:"published_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	// UpdatedAt is when Data last changed — admin audience only, and absent from
	// a delivery response entirely.
	//
	// It once carried the working copy's timestamp for every audience, which
	// leaked that an edit had been saved and when (OD2-023 F2). The first fix
	// swapped in published_at for delivery, but published_at is "first release
	// since the last retract" and a re-publish does NOT move it — so the public
	// got a field named updated_at that never updates even as the content
	// does. A frozen timestamp under that name misinforms more than no field at
	// all, so delivery now gets none; Version (the snapshot's) is what a caller
	// watches for change. A real snapshot timestamp needs its own column —
	// ADR-006 records the trigger.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// CreatedBy / UpdatedBy / PublishedBy — admin audience only, and absent from
	// a delivery response entirely. UpdatedBy is precisely the class of field
	// OD2-025 removed updated_at for: it moves on every save, so a public poller
	// could infer that unreleased edits exist — and now also who is making them.
	// Absent (not null) when unrecorded; nil means no person to name, which a
	// reader must render as unknown rather than inventing an actor.
	CreatedBy   *uuid.UUID `json:"created_by,omitempty"`
	UpdatedBy   *uuid.UUID `json:"updated_by,omitempty"`
	PublishedBy *uuid.UUID `json:"published_by,omitempty"`
	// The WHAT beside those WHOs (ADR-013 §2, ADR-014 §4) — admin audience only,
	// on the same grounds: they describe internal writers, and the agent id is a
	// tenant's private automation topology.
	//
	// Two pairs, not one, because they answer different questions and a console
	// column that shows only the first is the specific incompleteness ADR-014 §4
	// opens with: created_by_kind says an entry was authored by hand in March,
	// while updated_by_kind is what says a bot edited it at 10:03 today.
	//
	// Absent means unrecorded and MUST render as unknown. For the created pair
	// that is rows predating migration 000030; for the updated pair, rows whose
	// last write predates 000031 — a population that only shrinks, since every
	// write through this service records it. Falling back to the matching WHO's
	// name is the one rendering the ADR forbids outright: it turns "we don't
	// know" into a named person's byline.
	CreatedByKind  string  `json:"created_by_kind,omitempty"`
	CreatedByAgent *string `json:"created_by_agent,omitempty"`
	UpdatedByKind  *string `json:"updated_by_kind,omitempty"`
	UpdatedByAgent *string `json:"updated_by_agent,omitempty"`

	// PublishedData is the live snapshot standing beside Data's working copy, so
	// a console can show WHAT differs rather than only that something does
	// (ADR-014 §6). Admin audience only: delivery is already being served the
	// snapshot as Data, and a preview link goes to people outside the platform.
	//
	// Absent means there is no snapshot — a draft never published, which is not
	// the same as "published and identical" and must not be rendered as an empty
	// diff. Whether the two copies differ at all is still HasUnpublishedChanges,
	// which the database answers with jsonb equality; this field is the material
	// for the field-by-field view, not a second source of truth about it.
	//
	// It goes through the SAME masking as Data — see MarshalJSON. A diff is the
	// one place where showing two versions of a document makes it tempting to
	// read the raw columns, and the raw column is precisely what field-level read
	// permission does not cover (it lives in this projector, never in SQL).
	PublishedData json.RawMessage `json:"published_data,omitempty"`
	// HasHiddenChanges says at least one field this reader may not see differs
	// between the two copies. It exists because masking both sides is not enough
	// on its own: with the keys simply removed, a change confined to them renders
	// as "no changes", which is a stronger claim than "nothing you may see
	// changed" and a false one. A reader about to press publish would be
	// endorsing edits the interface told them did not exist.
	//
	// omitempty, like HasUnpublishedChanges: false is the ordinary case, and the
	// delivery allowlist test depends on cleared bools leaving no key behind.
	HasHiddenChanges bool `json:"has_hidden_changes,omitempty"`

	// Related holds the entries this one's relation fields point at, when the
	// caller asked for them with ?populate= (ADR-006 Amendment 6, extended to
	// the admin audience by Amendment 7; preview is still refused). Keyed by
	// the RELATION FIELD's key, and standing BESIDE Data rather than replacing
	// anything in it: the ids in `data` are untouched, so a field's JSON type
	// does not change with the query.
	//
	// The entries in here are in the SAME view as the parent, because they come
	// out of the same projector under the same subject: published snapshots for
	// delivery, working copies for admin. "Live" therefore means different
	// things per audience — a never-published target is nothing to delivery and
	// an ordinary draft to an editor — which is the whole substance of
	// Amendment 7.
	//
	// omitempty, and that is the contract, not a tidy-up: no key at all means
	// "nobody asked". A key that IS present but null (single) or empty (multi)
	// means "asked, and nothing at the other end is there" — a different
	// answer, and the one a consumer must not retry.
	//
	// One level only. The entries in here are projected the same way their
	// parent is but never populated themselves, so this key never nests.
	Related map[string]RelatedEntries `json:"related,omitempty"`

	// Schedule is the entry's OUTSTANDING publish/unpublish intent (ADR-017) —
	// pending, or stale — absent when it has none. Admin audience only, on the
	// plainest of the grounds this struct already uses: it announces an
	// unreleased editorial decision, and a delivery reader learning that a page
	// comes down on Friday is the same class of leak as their learning that
	// unreleased edits exist (has_unpublished_changes, OD2-023 F2).
	//
	// Stale is here for the same reason pending is: it is something that still
	// needs a decision — somebody has to look at the edit that invalidated it
	// and re-file — and a field that vanished the moment the release broke would
	// make this the one screen that goes quiet exactly when it matters.
	//
	// It carries no terminal schedules. "This went live last Tuesday as planned"
	// is history and belongs in the activity stream; what an editor needs beside
	// the entry is the thing that is still going to happen to it, or the thing
	// that was supposed to.
	//
	// Set by withSchedule and by nothing else — never in the shared literal in
	// ProjectEntry, per the rule at the bottom of that function.
	Schedule *EntryScheduleSummary `json:"schedule,omitempty"`

	// ReviewDecision is the entry's LATEST reviewer verdict, shown only while
	// it still describes the entry a reader would see (ADR-014 Amendment:
	// review decisions) — Version above equal to the decision's own
	// EntryVersion. The moment the author saves again, Version moves on and
	// this field goes back to absent on its own: nothing clears it, nothing
	// re-queries to notice the save, the comparison in withReviewDecision
	// simply stops matching. Admin audience only, on withSchedule's exact
	// reasoning: it announces an editorial verdict a delivery or preview reader
	// has no standing to see.
	//
	// Set by withReviewDecision and by nothing else — never in the shared
	// literal in ProjectEntry, same rule as Schedule.
	ReviewDecision *ReviewDecisionDTO `json:"review_decision,omitempty"`

	// aud is set by ProjectEntry and by nothing else. Unexported so it cannot be
	// spelled from outside, and unset on any DTO built as a literal — which
	// MarshalJSON treats as an error rather than a default.
	aud EntryAudience
	// hidden is the field keys this reader may not see, computed by ProjectEntry
	// and applied by MarshalJSON. nil is the common case and means nothing is
	// hidden — which is safe ONLY because a DTO that never went through
	// ProjectEntry has no audience either, and MarshalJSON refuses it outright.
	// That refusal is what keeps a nil here from being a fail-open default.
	hidden []string
	// project is the payload keys the CALLER asked to keep (ADR-013 §7), or nil
	// for the whole payload. Set by narrowedTo and applied by MarshalJSON.
	//
	// Unlike hidden, forgetting it is not a leak: it can only return more of
	// what this reader was already entitled to. That is why it may be set after
	// ProjectEntry rather than inside it — the fail-closed machinery around
	// `aud` and `hidden` is answering a different question, and borrowing it
	// here would suggest this one is also load-bearing for confidentiality.
	project []string
}

// narrowedTo restricts the payload to the caller's ?fields= selection. nil is
// the no-op, so a read path that does not offer projection needs no branch.
//
// A method on the DTO rather than a parameter on ProjectEntry: the projector's
// signature is exported and has nine call sites, and none of the other eight
// has a projection to pass. Adding a parameter there would put a nil at every
// one of them and make the interesting case invisible.
func (d EntryDTO) narrowedTo(keys []string) EntryDTO {
	d.project = keys
	return d
}

// withRelated attaches this entry's expanded relations (ADR-006 Amendment 6,
// Amendment 7 for the admin audience).
// nil is the no-op, so a read path that was not asked to populate needs no
// branch — the same shape as narrowedTo, and for the same reason.
//
// Like `project` and unlike `hidden`, forgetting it cannot leak: it only ever
// adds entries this credential was separately authorised to read, through the
// same projector, under the same subject.
func (d EntryDTO) withRelated(rel map[string]RelatedEntries) EntryDTO {
	if len(rel) == 0 {
		return d
	}
	d.Related = rel
	return d
}

// withSchedule attaches the entry's live-or-invalidated intent — the caller
// supplies a pending or stale row (ADR-017 §2), never a finished one.
//
// A method on the DTO rather than a parameter on ProjectEntry, for narrowedTo's
// reason: the projector's signature is exported with nine call sites, and only
// one of them has a schedule to pass. The other eight would carry a nil that
// says nothing.
//
// UNLIKE narrowedTo, forgetting this IS a difference a reader can see — but
// getting it wrong in the other direction is the one that leaks, so the
// audience is checked here as well as in MarshalJSON. Two checks because they
// fail differently: this one keeps the field out of the struct at all, so
// nothing internal can read it back off a delivery DTO either.
func (d EntryDTO) withSchedule(s *domain.EntrySchedule) EntryDTO {
	if s == nil || d.aud != audienceAdmin {
		return d
	}
	d.Schedule = &EntryScheduleSummary{
		Action:        s.Action,
		RunAt:         s.RunAt,
		PinnedVersion: s.PinnedVersion,
		State:         s.State,
	}
	return d
}

// withReviewDecision attaches the entry's latest review decision, but ONLY
// while it is still current — dec.EntryVersion equal to d.Version, the working
// copy version ProjectEntry set this DTO's Version to for the admin audience
// this field is restricted to. A decision whose EntryVersion has fallen behind
// is exactly the "sent back and then re-edited" case ADR-014 Amendment: review
// decisions treats as resolved; showing it here would tell an editor a stale
// verdict still applies to what they are looking at.
//
// Same two-check shape withSchedule uses and the same reason: audience is
// checked here as well as in MarshalJSON, so nothing internal can read this
// field back off a delivery or preview DTO either.
func (d EntryDTO) withReviewDecision(dec *domain.EntryReviewDecision) EntryDTO {
	if dec == nil || d.aud != audienceAdmin || dec.EntryVersion != d.Version {
		return d
	}
	d.ReviewDecision = &ReviewDecisionDTO{
		ID:           dec.ID,
		Decision:     dec.Decision,
		Reason:       dec.Reason,
		EntryVersion: dec.EntryVersion,
		DecidedBy:    dec.DecidedBy,
		DecidedAt:    dec.DecidedAt,
	}
	return d
}

// entryWire is EntryDTO stripped of its methods, so MarshalJSON can hand it to
// the encoder without recursing into itself.
type entryWire EntryDTO

// MarshalJSON renders the DTO for its audience, and is the last gate before an
// entry reaches a wire.
//
// It re-applies the delivery projection at SERIALISATION time instead of
// trusting whatever the constructor assigned. That is deliberate duplication:
// OD2-023 F2 shipped because an admin-only field was set in a shared literal
// and the delivery branch was trusted to unset it. Here, a delivery DTO cannot
// carry admin fields no matter what any code between projection and response
// did to it — including code written later by someone who never read
// ProjectEntry.
//
// An unset audience is refused outright. The caller sees a 500 (see
// response.write, which marshals before committing a status), which is the
// correct outcome: a DTO nobody assigned an audience to is a read path whose
// author never answered the question, and answering it wrong leaks drafts.
func (d EntryDTO) MarshalJSON() ([]byte, error) {
	w := entryWire(d)
	switch d.aud {
	case audienceAdmin:
		// Everything stays: the editor is entitled to the whole record.
		//
		// `related` included, since ADR-006 Amendment 7. It used to be cleared
		// here because populate was refused for this audience; now it is the
		// admin view of the targets — working copies, never-published rows
		// included — built by the same projector under the same subject, so
		// there is nothing here that a direct read of the target collection
		// would not also give this credential.
	case audienceDelivery:
		// Admin-only fields, cleared unconditionally. Keep this list in step
		// with the struct — TestDelivery_EntryDTOCarriesNoAuthorship fails,
		// naming the key, when a new field is added and not classified here.
		// It can only do that for a field its fixture actually populates,
		// because these are all `omitempty`: adding a field here means adding a
		// value for it there too, or the guard silently stops covering it.
		w.HasUnpublishedChanges = false
		w.UpdatedAt = nil
		w.CreatedBy, w.UpdatedBy, w.PublishedBy = nil, nil, nil
		w.CreatedByKind, w.CreatedByAgent = "", nil
		w.UpdatedByKind, w.UpdatedByAgent = nil, nil
		w.PublishedData, w.HasHiddenChanges = nil, false
		w.Schedule = nil
		w.ReviewDecision = nil
	case audiencePreview:
		// Same admin-only list as delivery MINUS UpdatedAt, which describes the
		// working copy this audience is being shown — see ProjectEntry. Spelled
		// out rather than sharing delivery's case: the two audiences agree today
		// on three of four fields, and a fallthrough would make the next field
		// added silently inherit an answer nobody chose for preview.
		w.HasUnpublishedChanges = false
		w.CreatedBy, w.UpdatedBy, w.PublishedBy = nil, nil, nil
		w.CreatedByKind, w.CreatedByAgent = "", nil
		w.UpdatedByKind, w.UpdatedByAgent = nil, nil
		w.PublishedData, w.HasHiddenChanges = nil, false
		// Populate is refused for a preview credential
		// (CONTENT_POPULATE_PREVIEW_UNSUPPORTED): it names one entry, and
		// expanding relations is a way to read others. Cleared here too, so the
		// scope rule survives any path that set it anyway.
		w.Related = nil
		w.Schedule = nil
		w.ReviewDecision = nil
	case audienceUnset:
		// The zero value, and the only one a caller reaches by accident: an
		// EntryDTO built as a struct literal never ran through ProjectEntry, so
		// nobody answered the audience question for it. Named explicitly rather
		// than left to the default below so that `audienceUnset` is referenced
		// by the code that enforces it — a const only mentioned in comments is
		// one `unused` cleanup away from deletion, and deleting it would slide
		// audienceAdmin into the iota-zero slot and make the ACCIDENT case the
		// full-record projection.
		return nil, fmt.Errorf("content: entry %s has no audience — build DTOs with ProjectEntry, never as a literal", d.ID)
	default:
		return nil, fmt.Errorf("content: entry %s has an unrecognised audience %d — build DTOs with ProjectEntry, never as a literal", d.ID, d.aud)
	}
	// Field-level read permission, applied at the SAME gate and for the same
	// reason as the admin-only fields above: whatever happened to this DTO
	// between projection and response, the keys its reader may not see are not in
	// the bytes. Doing it here rather than in ProjectEntry also keeps it out of
	// the path of anything that reads dto.Data internally.
	//
	// Untouched when nothing is hidden — which is every type that declares no
	// permission — so the stored JSONB text goes out verbatim and a payload is
	// never re-encoded for a reader who was refused nothing.
	//
	// The caller's own ?fields= narrowing composes here, and the two cannot
	// fight: both only REMOVE keys, so the result is the same set whichever runs
	// first — a projection can never put back what permission took out. That is
	// a property of the operations rather than of this line's order, which is
	// why the order is not load-bearing and permission is written last only
	// because it is the one that must be true unconditionally.
	w.Data = stripKeys(keepKeys(w.Data, d.project), d.hidden)
	// The snapshot gets the SAME two operations, not a copy of the intent. Both
	// sides of a diff have to be narrowed identically or the diff itself invents
	// changes: mask only the working copy and every restricted key reads as
	// "removed"; project only the working copy and every key outside ?fields=
	// reads as "added". Sharing the line is what makes those failure modes
	// unreachable rather than merely unlikely.
	//
	// Nothing guards this with `if aud == audienceAdmin` because both the
	// delivery and preview cases have already nil'd it above, and stripKeys on
	// empty bytes is the identity.
	w.PublishedData = stripKeys(keepKeys(w.PublishedData, d.project), d.hidden)
	return json.Marshal(w)
}

// keepKeys is stripKeys' opposite: it drops everything EXCEPT the named keys.
// nil keys means no projection at all, which is why the caller can pass the
// parse result straight through.
//
// A key the payload does not carry simply stays absent — an optional field with
// no value is not an error, and answering `{"body": null}` would invent a value
// the entry does not have. Failure to parse yields `{}` rather than the original
// bytes, matching stripKeys: if the document cannot be read, nothing about it
// can be published.
func keepKeys(raw json.RawMessage, keys []string) json.RawMessage {
	if len(keys) == 0 || len(raw) == 0 {
		return raw
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return json.RawMessage(`{}`)
	}
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		if v, ok := doc[k]; ok {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// EntryAudience selects WHICH copy of an entry a DTO exposes, and which fields
// come with it. Getting it wrong is a content leak: the working copy holds
// unreleased edits (ADR-006).
//
// The zero value is audienceUnset and is NEVER valid. That is the whole point:
// a DTO built by any route other than the projectors below carries no audience,
// and MarshalJSON refuses to render it rather than guessing. Previously the zero
// value WAS the admin audience, so a hand-built DTO — or a new read path that
// forgot to ask — defaulted to the copy with the unreleased edits in it. The
// default now fails closed.
//
// Nothing outside this package can name an audience: it is derived from the
// Subject by audienceFor and nowhere else, so "delivery credential, admin
// projection" is not an expressible state.
type EntryAudience int

const (
	// audienceUnset is the zero value and always a bug — see the type doc.
	audienceUnset EntryAudience = iota
	// audienceAdmin exposes the working copy — what the editor is editing.
	audienceAdmin
	// audienceDelivery exposes the published snapshot and nothing else.
	audienceDelivery
	// audiencePreview exposes the WORKING COPY in the delivery shape: what the
	// public page will look like once this draft ships. It is the delivery
	// audience with one substitution, not a third set of rules — see
	// authn.Subject.PreviewEntryID for why the credential narrows rather than
	// branches, and ProjectEntry for which fields follow the copy.
	audiencePreview
)

// audienceFor is the ONLY place a credential becomes an audience.
//
// It used to be an `aud := audienceAdmin; if sub.PublicDelivery { ... }` written
// out at each read path. Four copies of a two-line rule whose fall-through is
// the unsafe answer: a fifth read path that forgot the `if` would compile,
// pass review as "it looks like the others", and serve drafts.
// The preview check is NESTED inside PublicDelivery rather than placed beside
// it. A preview credential is a delivery credential that swaps one copy, so a
// PreviewEntryID arriving on anything that is not a delivery credential is not a
// preview — it is a malformed subject, and reading it as one would hand the
// working copy to whoever set the field. Nested, that case falls through to
// audienceAdmin, where the caller's own permissions decide as they always did.
func audienceFor(sub authn.Subject) EntryAudience {
	if sub.PublicDelivery {
		if sub.PreviewEntryID != nil {
			return audiencePreview
		}
		return audienceDelivery
	}
	return audienceAdmin
}

// EntryListResult is one page of entries. It carries EITHER offset fields
// (admin) or cursor fields (delivery), never both — the pointers are what makes
// the unused half vanish from the JSON instead of shipping a misleading zero.
// A `total` of 0 and a `has_more` of false are both meaningful values, so
// omitempty on plain ints/bools would be a bug, not a tidy-up.
type EntryListResult struct {
	Items []EntryDTO `json:"items"`
	Limit int        `json:"limit"`
	// Total and Offset: offset pagination (admin audience) only.
	Total  *int `json:"total,omitempty"`
	Offset *int `json:"offset,omitempty"`
	// NextCursor and HasMore: keyset pagination (delivery audience) only.
	// NextCursor is an OPAQUE token — it encodes the sort key, and callers that
	// parse it will break when the key changes. Absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    *bool  `json:"has_more,omitempty"`
}

// contentTypeDTO renders a content type for the caller who asked for it,
// including whether that caller may WRITE its entries at all.
//
// It is a method, and takes a context, for one reason: the write answer includes
// the verb, and the verb belongs to the authorizer. Re-deriving it from the
// tenant role here would be a second copy of the RBAC table that disagrees with
// the real decision under AUTHZ_MODE=allow and under OPA — a projection claiming
// a caller may not write while the server happily accepts their write, or the
// reverse. Asking s.authz is the only spelling that is right in all three modes.
//
// The write question is asked ONCE per type rather than once per request,
// because the resource is part of the input and a policy is entitled to use it.
// That is a rego evaluation per type on GET /types; the alternative is an
// assumption about the policy encoded in the caller.
func (s *contentService) contentTypeDTO(ctx context.Context, ct *domain.ContentType, sub authn.Subject) ContentTypeDTO {
	verbWritable := s.authz.Allow(ctx, authz.Input{
		Action:   ActionContentUpdate,
		Resource: authz.Resource{Type: resourceType, ID: ct.Name},
	}) == nil
	dto := toContentTypeDTO(ct, sub)
	dto.Writable = verbWritable && canWriteType(ct, sub)
	return dto
}

// toContentTypeDTO renders everything about a content type that does not need
// the authorizer.
//
// It takes the Subject for the same reason ProjectEntry does: readable/writable
// are per-request answers, and a projector that could not see the caller would
// have to be handed them by every call site.
//
// Writable is left FALSE here and set by contentTypeDTO. Defaulting it to the
// type-level answer alone would be the fail-OPEN direction — a viewer would come
// back writable — and the whole point of this pair is that a client can trust
// it. Nothing outside this package can build a ContentTypeDTO that skips the
// verb, because nothing outside this package calls either function.
func toContentTypeDTO(ct *domain.ContentType, sub authn.Subject) ContentTypeDTO {
	fields := make([]FieldDTO, len(ct.Fields))
	for i, f := range ct.Fields {
		fields[i] = FieldDTO{
			Readable:       canReadField(f, sub),
			Writable:       canWriteField(f, sub),
			Key:            f.Key,
			Type:           f.Type,
			Label:          f.Label,
			Required:       f.Required,
			Multiple:       f.Multiple,
			EnumValues:     f.EnumValues,
			ReadRoles:      f.ReadRoles,
			WriteRoles:     f.WriteRoles,
			Supported:      supportedOps(f),
			RelationEntity: f.RelationEntity,
			Component:      f.ComponentName,
			Components:     f.ZoneComponents,
			Description:    f.Description,
			Unique:         f.Unique,
			Format:         f.Format,
			Pattern:        f.Pattern,
			Min:            f.Min,
			Max:            f.Max,
		}
	}
	return ContentTypeDTO{
		ID:           ct.ID,
		Name:         ct.Name,
		Label:        ct.Label,
		ReadRoles:    ct.ReadRoles,
		WriteRoles:   ct.WriteRoles,
		OwnOnlyRoles: ct.OwnOnlyRoles,
		Fields:       fields,
		Readable:     canReadType(ct, sub),
		OwnOnly:      confinedAuthor(ct, sub) != nil,
		CreatedAt:    ct.CreatedAt,
		UpdatedAt:    ct.UpdatedAt,
	}
}

// ProjectEntry renders an entry for the credential that asked for it. It is the
// only constructor of a renderable EntryDTO — see EntryAudience.
//
// It takes the Subject rather than an audience so that the audience is not a
// caller decision at all. A read path cannot pick the wrong one, because it
// cannot pick.
//
// Exported so tests outside this package can build a DTO the same way
// production does. That is not a hole: the danger was never "someone
// deliberately renders an admin view", it was a delivery path quietly
// inheriting one.
//
// It takes the CONTENT TYPE rather than a bare type name because field-level
// read permission is part of the schema, and a projector that could not see the
// schema would have to be handed a precomputed key list by every caller — nine
// call sites, each free to forget. The name it used to take is ct.Name.
func ProjectEntry(ct *domain.ContentType, e *domain.Entry, sub authn.Subject) EntryDTO {
	aud := audienceFor(sub)
	dto := EntryDTO{
		ID:                 e.ID,
		Type:               ct.Name,
		hidden:             unreadableKeys(ct.Fields, sub),
		Data:               e.Payload,
		Version:            e.Version,
		Status:             e.Status,
		Locale:             e.Locale,
		TranslationGroupID: e.TranslationGroupID,
		PublishedAt:        e.PublishedAt,
		CreatedAt:          e.CreatedAt,
		UpdatedAt:          &e.UpdatedAt,
		aud:                aud,
	}
	if aud == audienceDelivery {
		// The snapshot, never the working copy. A published entry always has one
		// (DB CHECK, migration 000020); if it somehow did not, emitting an empty
		// object is the fail-closed answer — the leak would be serving Payload.
		dto.Data = e.PublishedPayload
		if len(dto.Data) == 0 {
			dto.Data = json.RawMessage(`{}`)
		}
		// Every field describing Data has to describe the SNAPSHOT too, or it
		// reports on a copy the reader was never given. Swapping Data alone left
		// Version and UpdatedAt tracking the working copy, so polling showed them
		// move while Data stood still — the exact fact has_unpublished_changes is
		// withheld from this audience to hide (OD2-023 F2).
		dto.Version = e.PublishedVersion
		// Dropped rather than re-pointed: see the field comment. published_at is
		// still exposed on its own name, where "first release" is what it says.
		dto.UpdatedAt = nil
		return dto
	}
	if aud == audiencePreview {
		// Nothing is reassigned here, and that is the point. The shared literal
		// above already holds the working copy — Data, Version and UpdatedAt all
		// read from it — so preview needs no substitution to satisfy the rule that
		// every field describing Data describes the SAME copy. The delivery branch
		// is the one that has to swap three fields; this one has to swap none.
		//
		// UpdatedAt stays, unlike delivery. There it was dropped because it
		// described the working copy while Data was the snapshot (OD2-023 F2, and
		// published_at cannot answer "when was this cut"). Here Data IS the working
		// copy, so updated_at reports on what the reviewer is actually looking at —
		// dropping it would leave "how fresh is this preview" unanswerable.
		//
		// What does NOT continue past this line is the admin-only block below:
		// has_unpublished_changes is an editing state, and created_by/updated_by/
		// published_by are internal user UUIDs. A preview link goes to people
		// outside the platform; neither is theirs to see. MarshalJSON clears all
		// four again regardless of what happens to the DTO after this.
		return dto
	}
	// Admin-only from here down. Everything below this line is gathered in one
	// place on purpose: assigning an admin-only field up in the shared literal
	// and relying on the delivery branch to unset it is the mistake that shipped
	// OD2-023 F2. Add new admin-only fields HERE, never above — and to the list
	// MarshalJSON clears, which is the backstop for exactly this mistake.
	dto.HasUnpublishedChanges = e.HasUnpublishedChanges
	dto.CreatedBy, dto.UpdatedBy, dto.PublishedBy = e.CreatedBy, e.UpdatedBy, e.PublishedBy
	dto.CreatedByKind, dto.CreatedByAgent = e.CreatedByKind, e.CreatedByAgent
	dto.UpdatedByKind, dto.UpdatedByAgent = e.UpdatedByKind, e.UpdatedByAgent
	dto.PublishedData = e.PublishedPayload
	dto.HasHiddenChanges = hiddenKeysDiffer(e.Payload, e.PublishedPayload, dto.hidden)
	return dto
}

// hiddenKeysDiffer answers "did anything you may not see change" for the keys
// MarshalJSON is about to remove from both copies.
//
// It has to be computed HERE rather than by the client, and that is the whole
// point of the field: after masking, the only party who can still tell is the
// server. Computing it from the masked bytes would always answer false.
//
// No snapshot means no diff at all — a draft that was never published is not
// "changed", it is unreleased — so the answer is false rather than "everything
// differs from nothing". The console says which one it is from the absence of
// published_data.
//
// The comparison is on raw bytes per key, which is safe only because both
// documents come out of the same jsonb column family and Postgres renders jsonb
// canonically: equal values render as equal text, so the numeric trap noted on
// unpublishedChangesExpr (1.0 vs 1 being jsonb-equal but text-different) cannot
// reach here through the database. It could through a hand-built Entry in a
// test, and the direction of that error is over-reporting — claiming a hidden
// change that did not happen — which shows the reader a warning they cannot act
// on rather than hiding one they needed.
func hiddenKeysDiffer(working, published json.RawMessage, hidden []string) bool {
	if len(hidden) == 0 || len(published) == 0 || len(working) == 0 {
		return false
	}
	var w, p map[string]json.RawMessage
	if json.Unmarshal(working, &w) != nil || json.Unmarshal(published, &p) != nil {
		// A payload that will not parse is one stripKeys renders as `{}`, so the
		// reader is about to be shown two empty documents. Saying "nothing you
		// cannot see changed" about a document nobody could read would be a
		// guess; saying it might have is the honest half.
		return true
	}
	for _, k := range hidden {
		wv, wok := w[k]
		pv, pok := p[k]
		if wok != pok || !bytes.Equal(wv, pv) {
			return true
		}
	}
	return false
}

// --- reverse references (ADR-024, 2.8a) -------------------------------------

// ReferencedByDTO is the page GET .../referenced-by returns: everything
// currently pointing at one entry or one media asset. total counts the FULL
// match set at the database, ahead of any agent-scope narrowing — see
// ReferencedByDTO's note on EntryReferencedBy/MediaReferencedBy for why an
// agent-filtered result does not adjust it.
type ReferencedByDTO struct {
	Items []ReferencedByItemDTO `json:"items"`
	Total int                   `json:"total"`
}

// ReferencedByItemDTO is one referrer. Field is "" for a media reference
// (entry_media carries no field-level granularity — ADR-024 §Known
// limitations) and the field's key for a relation reference. Published and
// Draft are independent: a referrer can hold the link in one payload, the
// other, or both, and both bits survive the merge across entry_relations /
// entry_relations_published (or entry_media / entry_media_published) rather
// than collapsing to a single "referenced" flag.
type ReferencedByItemDTO struct {
	EntryID   uuid.UUID `json:"entry_id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Field     string    `json:"field,omitempty"`
	Published bool      `json:"published"`
	Draft     bool      `json:"draft"`
}

// readableReferencedByRows narrows rows to the content types this SPECIFIC
// caller may read — an agent credential to its whitelist (AllowsContentType,
// the same rule toComponentDTO applies to used_by), and EVERY caller, human or
// agent, to the types canReadType's role check passes.
//
// The human half exists because a referrer can be of ANY type, not just the
// one the caller asked about: an editor whose role is excluded from some
// OTHER type's ReadRoles could otherwise learn — via referenced-by alone —
// that a restricted type has an entry (id, title, draft/published state) they
// are not allowed to GetEntry directly. That is the same leak guardTypeRead
// already refuses everywhere else a type is read; referenced-by just has to
// apply it per ROW instead of once, because a single result page can mix rows
// across many types.
//
// typesByName is looked up ONCE per request (contentTypesByName) and reused
// for every row, rather than re-fetched per row — the type roster does not
// change mid-request, and every reverse-lookup call needs at most one of
// these regardless of how many rows come back.
//
// Both projectReferencedBy (the GET .../referenced-by DTO) and
// DeleteMediaAsset's 409 preview call this, so the same rule protects both: a
// restricted-role editor must not learn a hidden type holds a reference
// (or what it is) whether they hit the read endpoint or provoke the delete
// refusal on an asset they CAN otherwise see.
func readableReferencedByRows(rows []repository.ReferencedByRow, sub authn.Subject, typesByName map[string]*domain.ContentType) []repository.ReferencedByRow {
	out := make([]repository.ReferencedByRow, 0, len(rows))
	for _, row := range rows {
		if sub.IsAgent() && !sub.AllowsContentType(row.TypeName) {
			continue
		}
		ct, ok := typesByName[row.TypeName]
		if !ok || !canReadType(ct, sub) {
			// Fail closed: a type this lookup cannot account for is treated as
			// unreadable rather than shown. The roster comes from the same
			// tenant-scoped ListContentTypes every row's TypeName should
			// resolve against; a miss here is a stale race, not the normal case.
			continue
		}
		out = append(out, row)
	}
	return out
}

// projectReferencedBy renders a repository.ReferencedByResult for the caller,
// narrowing with readableReferencedByRows and titling each row with
// domain.TitleFor over ITS OWN type's fields — not the target's — since a
// referrer is never the same type as what it references in general.
//
// The narrowing happens AFTER the database's COUNT(*) OVER() already fixed
// Total, so Total can read higher than len(Items) when the page held a
// referrer outside the caller's visibility — by agent whitelist OR by role.
// That is the same shape toComponentDTO already accepts for used_by (ADR-024
// §Known limitations) — fixing it would mean re-counting behind the caller's
// own visibility, which the repository layer cannot do without knowing which
// caller is asking.
func projectReferencedBy(res repository.ReferencedByResult, sub authn.Subject, typesByName map[string]*domain.ContentType) ReferencedByDTO {
	rows := readableReferencedByRows(res.Items, sub, typesByName)
	items := make([]ReferencedByItemDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, ReferencedByItemDTO{
			EntryID:   row.EntryID,
			Type:      row.TypeName,
			Title:     row.Title,
			Field:     row.FieldPath,
			Published: row.Published,
			Draft:     row.Draft,
		})
	}
	return ReferencedByDTO{Items: items, Total: res.Total}
}

// clampReferencedByPage applies GET .../referenced-by's default/max — 50 and
// 200 (ADR-024) — the same shape ListMediaAssets clamps Limit/Offset to,
// duplicated rather than shared because the two endpoints' defaults differ (20
// there, 50 here) and a shared helper parameterized on both numbers would read
// less clearly than the two literals it replaces.
func clampReferencedByPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
