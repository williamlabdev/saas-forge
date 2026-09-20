package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Publish revisions and restore (ADR-018).
//
// THE ONE THING TO UNDERSTAND ABOUT THIS FILE: restoring is EDITING, not
// releasing. Every endpoint here reads or writes the WORKING COPY, and the
// published snapshot is never touched by any of them. That is what lets restore
// take content:update rather than content:publish without putting a hole in
// ADR-014 §1's human gate: an agent that restores an old draft has changed what
// the editor sees and changed nothing the public sees, which is precisely the
// unattended-write shape §1 already permits. Going live still needs a person
// with content:publish, through SetEntryStatus or a schedule.
//
// It is also why the restore does NOT go through a special repository path. It
// ends in the same UpdateEntry the console's own save ends in, so every
// constraint that governs an ordinary edit — required fields, formats, the
// unique ledger, relation existence, quota, the optimistic lock — governs a
// restore identically, and none of them had to be taught about revisions. A
// revision that cannot be made valid under today's schema is simply a save that
// fails, with the error code the editor already knows.

// EntryRevisionDTO is one release as the list returns it: WITHOUT its payload.
//
// The absence is the design (see ListEntryPublishRevisions in the repository): a
// list of twenty full snapshots is twenty entries on the wire to render a table
// of dates, and each of those payloads would need field-level masking to render
// none of them.
type EntryRevisionDTO struct {
	// RevisionNo is what every other endpoint here addresses a revision by. It
	// is NOT the entry version — see domain.EntryPublishRevision — and the two
	// are both present so a console never has to guess which number an editor
	// means.
	RevisionNo int `json:"revision_no"`
	Version    int `json:"version"`

	// PublishedAt is when THIS release went live, which is not the entry's
	// published_at (that one means "first release since the last unpublish" and
	// does not move on a re-publish).
	PublishedAt time.Time `json:"published_at"`
	// PublishedBy is who answers for the release; null is unrecorded, which a
	// reader renders as unknown rather than inventing an actor — the same
	// three-state rule the entry provenance fields follow. No `omitempty`, for
	// the reason ActivityDTO's header gives: a key that vanishes at its zero
	// value is a key an API-shape test can silently stop covering.
	PublishedBy *uuid.UUID `json:"published_by"`

	// Via and ViaScheduleID say HOW the release happened: "" for a person
	// pressing publish, "schedule" for a worker acting on an intent filed
	// earlier. Rendering a 03:00 scheduled release as a person publishing at
	// 03:00 is not a missing detail but a false statement about who was at the
	// keyboard, which is why neither is omitempty.
	Via           string     `json:"via"`
	ViaScheduleID *uuid.UUID `json:"via_schedule_id"`
}

// EntryRevisionDetailDTO is one release WITH the snapshot it carried.
//
// A separate type rather than an `omitempty` payload on the list DTO. A field
// that is absent on every row of the list and present on every single read is
// two shapes wearing one name, and the shape a client actually has to handle is
// decided by which URL it called — which the type system may as well say.
type EntryRevisionDetailDTO struct {
	EntryRevisionDTO
	// Data is the snapshot, MASKED for this reader: keys the caller may not read
	// are absent, exactly as they are absent from an entry's own `data`. What
	// the console renders is therefore the part of the old release this person
	// was entitled to see, never the whole row as stored.
	Data json.RawMessage `json:"data"`
}

// EntryRevisionDiffDTO compares a stored release against the CURRENT WORKING
// COPY — not against the live snapshot, and the choice is the whole point of the
// endpoint. "What is live versus what I am editing" is already answered by the
// entry's own data/published_data pair (ADR-014 §6). The question this answers
// is the one that comes just before pressing restore: if I put February back,
// what changes about the draft I am looking at now.
//
// IT REUSES §6'S DIFF SHAPE rather than inventing one: two payloads narrowed
// IDENTICALLY, plus the flag that says whether something the reader cannot see
// also differs. The identical narrowing is not a nicety — mask one side only and
// every restricted key reads as "removed", which is a diff that invents changes.
type EntryRevisionDiffDTO struct {
	RevisionNo  int       `json:"revision_no"`
	Version     int       `json:"version"`
	PublishedAt time.Time `json:"published_at"`

	// RevisionData is the old release; Data is the working copy as it stands
	// now. Named so the pair reads in the direction the restore would move: from
	// revision_data to data today, and back the other way if the button is
	// pressed.
	RevisionData json.RawMessage `json:"revision_data"`
	Data         json.RawMessage `json:"data"`

	// ChangedKeys names the top-level keys that differ, computed BEFORE masking
	// and then filtered to the ones this reader may see. Computing it after
	// masking would silently agree with has_hidden_changes and make that flag
	// unable to disagree with anything.
	ChangedKeys []string `json:"changed_keys"`
	// HasHiddenChanges says that a key the reader cannot see also differs — §6's
	// flag, and the reason a restricted field cannot be moved without the
	// console being able to say something changed.
	HasHiddenChanges bool `json:"has_hidden_changes"`
	// DroppedKeys names keys this revision carries that the type no longer
	// defines. They are shown in the diff as well as in the restore result so the
	// loss is visible BEFORE the button is pressed rather than reported after it.
	DroppedKeys []string `json:"dropped_keys"`
}

// RestoreEntryInput is the restore request body.
type RestoreEntryInput struct {
	RevisionNo int `json:"revision_no"`
}

// RestoreEntryResultDTO is the restored entry plus what the restore had to leave
// behind.
//
// A WRAPPER RATHER THAN A BARE EntryDTO, which is a deliberate departure from
// the approved shape and the reason is dropped_keys. Those names are the one
// thing a caller cannot recompute from the response — the keys are gone from
// both the entry and the current schema — and an EntryDTO has nowhere to put
// them. The alternatives were worse in the ways this codebase already refuses:
// an `omitempty` field bolted onto EntryDTO would leak revision vocabulary into
// every delivery response's type, and reporting the loss only on the activity
// line would tell the editor's colleagues what it did not tell the editor.
type RestoreEntryResultDTO struct {
	Entry        EntryDTO        `json:"entry"`
	RestoredFrom RestoredFromDTO `json:"restored_from"`
}

// RestoredFromDTO is the provenance of a restore: which release was replayed,
// when it had been live, and what could not be carried over.
type RestoredFromDTO struct {
	RevisionNo  int       `json:"revision_no"`
	PublishedAt time.Time `json:"published_at"`
	// DroppedKeys is `[]` rather than null when nothing was dropped, so a client
	// can iterate it without a nil check — and so that "nothing was lost" is
	// stated positively rather than inferred from an absent key.
	DroppedKeys []string `json:"dropped_keys"`
}

// errRevisionNotFound covers three situations on purpose: no such ordinal, an
// ordinal retention has discarded, and an entry with no releases at all. They
// are one code because the answer a console has to give is the same for all
// three — this is not something you can restore — and distinguishing "expired"
// from "never existed" would need a tombstone per purged row.
var errRevisionNotFound = apperrors.New(
	"CONTENT_REVISION_NOT_FOUND",
	"this entry has no such published revision",
	http.StatusNotFound,
)

// ErrRevisionNoInvalid answers a revision number that is not a number, or is not
// a positive one. It is 400 rather than 404 because the request is malformed
// rather than pointing at something absent: `/revisions/latest` is a client bug,
// and answering 404 would send its author looking for a missing row.
//
// EXPORTED because the path segment is parsed in the handler — that is where the
// raw string exists — and the body's revision_no is checked here. One spelling
// for one mistake, or a caller who mistypes the URL and a caller who mistypes
// the body read two different codes for the same error.
func ErrRevisionNoInvalid(raw string) error {
	return apperrors.New("CONTENT_REVISION_NO_INVALID", "revision_no must be a positive integer", 400).
		WithDetails(map[string]any{"revision_no": raw})
}

// guardRevisionAudience refuses the two credentials that must never learn that
// revisions exist, before any row is touched.
//
// DELIVERY AND PREVIEW: a revision holds content that was live at some point and
// content that a restore would make live again, and the public surface is
// defined by the CURRENT snapshot alone (ADR-004/006). A preview credential is
// the sharper case — it leaves the platform's edge, and it addresses one entry,
// so handing it that entry's release history would hand an outsider every
// version of the page they were shown one draft of.
//
// AN AGENT: refused too, and this one is a ruling rather than a consequence.
// ADR-013 §5's tool list does not include restore, but "the tool list is UX, not
// authorization" — an agent credential holds content:read and content:update, so
// without this line it could drive these endpoints at the HTTP layer directly.
// Restore is refused because it is the one write whose blast radius is not
// bounded by what the agent wrote: it replaces the ENTIRE working copy with a
// document the agent did not author and a person approved for a different day.
// The reads are refused with it, because an agent that could read the history
// could reconstruct the payload restore would have written.
//
// 403 and not 404, on guardPreviewCollection's reasoning: the refusal is uniform
// — it depends only on the credential, never on the entry or the ordinal — so it
// discriminates nothing and enumerates nothing.
func guardRevisionAudience(sub authn.Subject) error {
	if sub.PublicDelivery {
		return apperrors.ErrForbidden
	}
	if sub.IsAgent() {
		return apperrors.New(
			"CONTENT_REVISION_AGENT_FORBIDDEN",
			"an agent credential may not read or restore published revisions",
			403,
		)
	}
	return nil
}

// resolveRevisionEntry is the guard sequence every endpoint in this file shares:
// authorize, refuse the audiences that must not be here, find the type, find the
// entry, and refuse a row this caller does not own.
//
// Hoisted because the ORDER is load-bearing and four copies of it would be four
// chances to reorder one. The audience refusal comes before the type lookup so
// it cannot be timed against which types exist; confinement comes last so that a
// row a confined caller may not touch answers 404 rather than confirming its
// existence, exactly as GetEntry does.
func (s *contentService) resolveRevisionEntry(ctx context.Context, action, typeName string, id uuid.UUID) (authn.Subject, *domain.ContentType, *domain.Entry, error) {
	sub, err := s.authorize(ctx, action, id.String(), typeName)
	if err != nil {
		return sub, nil, nil, err
	}
	if err := guardRevisionAudience(sub); err != nil {
		return sub, nil, nil, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return sub, nil, nil, err
	}
	// Read or write gate depending on the verb: a restore is a write to the
	// type, and a type this caller may not write entries of is not one they may
	// replay an old payload into.
	if action == ActionContentUpdate {
		if err := guardTypeWrite(ct, sub); err != nil {
			return sub, nil, nil, err
		}
	} else if err := guardTypeRead(ct, sub); err != nil {
		return sub, nil, nil, err
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return sub, nil, nil, err
	}
	if err := guardOwned(ct, sub, e); err != nil {
		return sub, nil, nil, err
	}
	return sub, ct, e, nil
}

// ListEntryRevisions returns the entry's releases, newest first.
func (s *contentService) ListEntryRevisions(ctx context.Context, typeName string, id uuid.UUID) (_ []EntryRevisionDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryRead, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, ct, e, err := s.resolveRevisionEntry(ctx, ActionContentRead, typeName, id)
	if err != nil {
		return nil, err
	}
	act.describe(ct.Fields, e.Payload)
	rows, err := s.repo.ListEntryPublishRevisions(ctx, sub.TenantID, id)
	if err != nil {
		return nil, err
	}
	// An entry with no releases answers 200 with `[]`, not 404. "This has never
	// been published" is a true and useful answer about an entry that exists;
	// 404 would say the entry does not, which the caller has just been told
	// otherwise by every other endpoint.
	out := make([]EntryRevisionDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, projectPublishRevision(r))
	}
	return out, nil
}

// GetEntryRevision returns one release with its snapshot, masked.
func (s *contentService) GetEntryRevision(ctx context.Context, typeName string, id uuid.UUID, revisionNo int) (_ EntryRevisionDetailDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryRead, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, ct, e, err := s.resolveRevisionEntry(ctx, ActionContentRead, typeName, id)
	if err != nil {
		return EntryRevisionDetailDTO{}, err
	}
	act.describe(ct.Fields, e.Payload)
	rev, err := s.loadRevision(ctx, sub.TenantID, id, revisionNo)
	if err != nil {
		return EntryRevisionDetailDTO{}, err
	}
	// THE MASK IS THE WHOLE REASON ADR-014 §5 could not expose these rows. The
	// stored payload holds every restricted field's value in full; what leaves
	// here is that document minus the keys this reader may not see, the same
	// operation MarshalJSON applies to an entry's own data. It is applied at the
	// projector rather than at the wire because this DTO has no MarshalJSON of
	// its own — which is a difference worth noticing, not hiding: an
	// EntryRevisionDetailDTO built as a literal by some later code path would
	// carry whatever payload was put in it.
	return EntryRevisionDetailDTO{
		EntryRevisionDTO: projectPublishRevision(*rev),
		Data:             stripKeys(rev.Payload, unreadableKeys(ct.Fields, sub)),
	}, nil
}

// DiffEntryRevision compares a release against the current working copy.
func (s *contentService) DiffEntryRevision(ctx context.Context, typeName string, id uuid.UUID, revisionNo int) (_ EntryRevisionDiffDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryRead, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, ct, e, err := s.resolveRevisionEntry(ctx, ActionContentRead, typeName, id)
	if err != nil {
		return EntryRevisionDiffDTO{}, err
	}
	act.describe(ct.Fields, e.Payload)
	rev, err := s.loadRevision(ctx, sub.TenantID, id, revisionNo)
	if err != nil {
		return EntryRevisionDiffDTO{}, err
	}
	// Both sides pruned to the CURRENT schema before anything is compared, so
	// the diff describes what a restore would actually do. Comparing the raw
	// revision would report a deleted field as a difference the editor could act
	// on, when nothing they press can put it back; it is reported as a dropped
	// key instead, which is what it is.
	old := pruneUndefined(rev.Payload, ct.Fields)
	current := pruneUndefined(e.Payload, ct.Fields)
	hidden := unreadableKeys(ct.Fields, sub)
	changed := visibleKeys(domain.ChangedKeysBetween(old, current), hidden)
	return EntryRevisionDiffDTO{
		RevisionNo:   rev.RevisionNo,
		Version:      rev.Version,
		PublishedAt:  rev.PublishedAt,
		RevisionData: stripKeys(old, hidden),
		Data:         stripKeys(current, hidden),
		ChangedKeys:  changed,
		// hiddenKeysDiffer takes the UNMASKED pair on purpose: the flag exists to
		// report on exactly the keys the masked documents no longer contain.
		HasHiddenChanges: hiddenKeysDiffer(current, old, hidden),
		DroppedKeys:      droppedKeys(rev.Payload, ct),
	}, nil
}

// RestoreEntryRevision writes an old release back into the WORKING COPY.
//
// IT DOES NOT PUBLISH, and that is the sentence the whole design hangs on. The
// entry's status, its live snapshot and its published_at are untouched: what the
// public sees after a restore is exactly what it saw before. Releasing the
// restored draft is a second, separate act by someone holding content:publish —
// which is what keeps this endpoint out of ADR-014 §1's human gate rather than
// through it.
//
// IT IS A FULL REPLACE, unlike UpdateEntry's PATCH merge, and the difference is
// the point. Merging would mean a key added after the revision survives the
// "restore", so the result is neither the old document nor the new one but a
// third thing that never existed and that nobody reviewed. The one honest
// meaning of "put February back" is the February document.
//
// AUTHORIZATION IS content:update, the ordinary editing verb. The version
// advances, so a pending schedule pinned to the pre-restore version goes stale by
// ADR-017's existing rule — no new code, and the right outcome: an approval given
// for one document must not carry over to a different one.
func (s *contentService) RestoreEntryRevision(ctx context.Context, typeName string, id uuid.UUID, in RestoreEntryInput, expectedVersion int) (_ RestoreEntryResultDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivityEntryRestore, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	if in.RevisionNo <= 0 {
		return RestoreEntryResultDTO{}, ErrRevisionNoInvalid("")
	}
	sub, ct, existing, err := s.resolveRevisionEntry(ctx, ActionContentUpdate, typeName, id)
	if err != nil {
		return RestoreEntryResultDTO{}, err
	}
	// The optimistic lock, checked here as well as in the repository and in the
	// same position UpdateEntry checks it: AFTER confinement, so a 409 never
	// confirms the existence and version of a row this caller may not touch.
	if expectedVersion > 0 && existing.Version != expectedVersion {
		return RestoreEntryResultDTO{}, repository.ErrVersionConflict
	}
	rev, err := s.loadRevision(ctx, sub.TenantID, id, in.RevisionNo)
	if err != nil {
		return RestoreEntryResultDTO{}, err
	}

	// SCHEMA DRIFT, resolved by dropping rather than refusing. A revision can be
	// older than the type: fields get removed, and their values in an old
	// snapshot have nowhere to land. Refusing would make an entire era of an
	// entry's history permanently unrestorable over one field nobody uses any
	// more; dropping silently would be worse than either, so the names travel on
	// the response AND on the activity line.
	//
	// The other direction — a field added since — needs nothing: the key is
	// simply absent, and validation answers for it exactly as it would for a
	// hand-typed document missing a required field. That is the whole of "falls
	// through to the existing validation": a required field added after this
	// release earns the ordinary 422, a value that no longer fits its format
	// earns the ordinary 422, and a unique value another entry has since taken
	// earns the ordinary 409.
	dropped := droppedKeys(rev.Payload, ct)
	restored := pruneUndefined(rev.Payload, ct.Fields)

	// FIELD PERMISSION, and this is where ADR-018 answers the question ADR-014
	// §5 left open.
	//
	// §5 observed that a restore replays a WHOLE payload, and that
	// guardWritableKeys refuses any unwritable key it is handed — so a
	// restricted role could never restore an entry that merely CONTAINS a
	// restricted field, even one whose value they are not moving. That made
	// restore useless for exactly the types that most need it, and the tempting
	// fix was a bypass, which ADR-009 refuses outright.
	//
	// The resolution is to guard the keys whose value would actually CHANGE.
	// Replaying a restricted field's identical value is not a write to it: the
	// stored document already holds that value, nobody's permissions decided it,
	// and after the save the field reads exactly as it did before. An attempt to
	// MOVE a restricted value still lands in guardWritableKeys and still earns
	// errFieldWriteForbidden, so the property §5 was protecting is intact and
	// only the false positives are gone.
	//
	// Both directions count, because ChangedKeysBetween reports both: a
	// restricted key the revision would REMOVE from the document is a change to
	// that key and is refused just as loudly as one it would overwrite.
	changed := domain.ChangedKeysBetween(pruneUndefined(existing.Payload, ct.Fields), restored)
	if err := guardWritableKeys(ct, sub, keepKeys(restored, changed)); err != nil {
		return RestoreEntryResultDTO{}, err
	}
	// keepKeys cannot express "this key is being deleted", so removals are
	// guarded separately against the same rule. Without this, dropping a
	// restricted field by restoring a revision that predates it would be the
	// bypass the paragraph above says does not exist.
	if err := guardRemovedKeys(ct, sub, changed, restored); err != nil {
		return RestoreEntryResultDTO{}, err
	}

	// From here it is an ordinary save, deliberately: the same normalisation,
	// the same quota guard, the same repository call, the same media relink. A
	// restore that produced a document an ordinary save would have refused is a
	// restore that has escaped the rules the console enforces.
	clean, mediaRefs, relationRefs, err := s.validateAndNormalize(ctx, sub.TenantID, sub, ct, restored)
	if err != nil {
		return RestoreEntryResultDTO{}, err
	}
	quota, err := s.resolveQuota(ctx, sub.TenantID)
	if err != nil {
		return RestoreEntryResultDTO{}, err
	}
	if err := guardEntryBytes(quota, clean); err != nil {
		return RestoreEntryResultDTO{}, err
	}
	// Recorded against the same base the guard used, and BEFORE the assignment
	// below overwrites it. The title comes from the NEW payload for UpdateEntry's
	// reason: the stream is read later, and a label taken from the pre-restore
	// document would describe a version that no longer exists anywhere.
	act.changedKeys(domain.ChangedKeysBetween(pruneUndefined(existing.Payload, ct.Fields), clean)).
		describe(ct.Fields, clean).
		withDetails(domain.RestoreDetails(rev.RevisionNo, rev.PublishedAt, dropped))

	existing.Payload = clean
	// Same reason UpdateEntry recomputes it: a restore replaces the working
	// payload just as a save does, and the entries.search_text column would
	// otherwise keep indexing text the draft no longer holds.
	existing.SearchText = domain.ExtractSearchText(ct.Fields, clean)
	existing.UpdatedAt = time.Now().UTC()
	recordUpdateProvenance(existing, sub)
	// Status, published_payload, published_at and published_by are all
	// deliberately absent from this block. See the method comment: a restore
	// changes the draft and nothing else.
	if err := s.repo.UpdateEntry(ctx, existing); err != nil {
		return RestoreEntryResultDTO{}, err
	}
	// Media links follow the WORKING payload on every write, restore included —
	// otherwise an entry restored to a revision that used a different image would
	// hold a reference set describing a document it no longer contains.
	if err := s.repo.ReplaceEntryMedia(ctx, sub.TenantID, existing.ID, mediaRefs); err != nil {
		return RestoreEntryResultDTO{}, err
	}
	if err := s.repo.ReplaceEntryRelations(ctx, sub.TenantID, existing.ID, relationRefs); err != nil {
		return RestoreEntryResultDTO{}, err
	}
	if dropped == nil {
		dropped = []string{}
	}
	return RestoreEntryResultDTO{
		Entry: ProjectEntry(ct, existing, sub),
		RestoredFrom: RestoredFromDTO{
			RevisionNo:  rev.RevisionNo,
			PublishedAt: rev.PublishedAt,
			DroppedKeys: dropped,
		},
	}, nil
}

// loadRevision fetches one revision and translates the repository's generic
// not-found into this feature's code. Shared by the three endpoints that address
// a revision by number so the 404 has one spelling.
func (s *contentService) loadRevision(ctx context.Context, tenantID string, entryID uuid.UUID, revisionNo int) (*domain.EntryPublishRevision, error) {
	if revisionNo <= 0 {
		// Unreachable from the handler, which parses and rejects first. Kept
		// because the service is also called directly by tests and by any future
		// non-HTTP caller, and a non-positive ordinal must not turn into a
		// database round trip that answers 404 for a request that was malformed.
		return nil, ErrRevisionNoInvalid("")
	}
	rev, err := s.repo.GetEntryPublishRevision(ctx, tenantID, entryID, revisionNo)
	if err != nil {
		if errors.Is(err, apperrors.ErrNotFound) {
			return nil, errRevisionNotFound
		}
		return nil, err
	}
	return rev, nil
}

func projectPublishRevision(r domain.EntryPublishRevision) EntryRevisionDTO {
	return EntryRevisionDTO{
		RevisionNo:    r.RevisionNo,
		Version:       r.Version,
		PublishedAt:   r.PublishedAt,
		PublishedBy:   r.PublishedBy,
		Via:           r.Via,
		ViaScheduleID: r.ViaScheduleID,
	}
}

// droppedKeys names the payload keys a revision carries that the type no longer
// defines — what a restore would have to leave behind.
//
// It reads the revision's OWN keys rather than diffing against the entry,
// because the question is about the schema and not about the current document: a
// key the working copy also still carries would be dropped from both by
// pruneUndefined on the next ordinary save, and reporting it here would blame the
// restore for a schema change.
func droppedKeys(payload json.RawMessage, ct *domain.ContentType) []string {
	keys, err := payloadKeys(payload)
	if err != nil {
		// A stored snapshot that will not parse is not something a caller can
		// act on, and it cannot be restored either — validateAndNormalize will
		// refuse it a moment later with a code about the payload. Reporting no
		// dropped keys here is the honest answer to "which of its keys are
		// undefined": none are known, because none could be read.
		return nil
	}
	var out []string
	for _, k := range keys {
		if _, ok := ct.FieldByKey(k); !ok {
			out = append(out, k)
		}
	}
	return out
}

// visibleKeys filters a key list down to what this reader may see. Used on the
// diff's changed_keys so the list agrees with the two payloads beside it: naming
// a key that has been masked out of both documents would tell the reader that
// something they cannot see changed, which is has_hidden_changes' job and is
// said there once rather than twice in two shapes.
func visibleKeys(keys, hidden []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !containsKey(hidden, k) {
			out = append(out, k)
		}
	}
	return out
}

// guardRemovedKeys refuses a restore that would DELETE a field this caller may
// not write.
//
// It exists because keepKeys — which builds the document guardWritableKeys
// inspects — can only carry keys that are present. A key the revision does not
// have is a key the restore removes, and a removal is a write: the field reads
// differently afterwards, and whoever set it did so under a permission this
// caller does not hold. Without this the "restore only what changes" rule above
// would have a hole exactly the shape of "restore a revision from before the
// restricted field existed".
func guardRemovedKeys(ct *domain.ContentType, sub authn.Subject, changed []string, restored json.RawMessage) error {
	if !anyRestricted(ct.Fields) {
		return nil
	}
	present, err := payloadKeys(restored)
	if err != nil {
		return err
	}
	for _, k := range changed {
		if containsKey(present, k) {
			continue // an overwrite; guardWritableKeys already answered for it
		}
		f, ok := ct.FieldByKey(k)
		if !ok {
			continue
		}
		if !canWriteField(f, sub) {
			return errFieldWriteForbidden(k, effectiveWriteRoles(f))
		}
	}
	return nil
}

func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}
