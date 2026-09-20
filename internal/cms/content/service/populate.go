package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Relation populate for the delivery audience — ADR-006 Amendment 6, extended
// to dotted paths up to maxPopulateDepth levels by Amendment 8.
//
// The shape decision that drives everything in this file: `data` keeps the ids
// EXACTLY as stored, and the expanded entries arrive in a sibling key,
// `related`. Substituting objects for ids in place would make one field's JSON
// type depend on a query parameter — `"author": "9f2a…"` without ?populate and
// `"author": {…}` with it — which no typed client can model, and which turns
// adding a populate to one call site into a decoding error in every other
// consumer of the same content type.
//
// It also keeps the two questions separable: `data` answers "what does this
// entry say", `related` answers "and what is at the other end". A client that
// does not know about populate reads exactly what it read before.
//
// A dotted path (`?populate=author.avatar`) nests the SAME contract one level
// down: the target entry's OWN `related` key carries its own expansions, built
// by the identical projector and the identical audience rules as the parent —
// see populateLevel. Nothing about component or dynamic-zone fields changes:
// this file reads relation ids off the entry's TOP-LEVEL payload keys only
// (decodeObject/relationIDs), so a relation nested inside a component or a
// dynamic-zone block was never reachable by ?populate= and still is not — a
// dotted path only walks from one ENTRY's relation field to the next entry's
// top-level fields, never into a component's sub-fields (ADR-020 §3, see
// TestComponentQueryRefusals).

// maxPopulateDepth caps the number of dot-separated segments a single
// ?populate= value may name — `author` is depth 1, `author.avatar` depth 2,
// `author.avatar.owner` depth 3. A code constant, not a plan dimension, for
// the same reason maxPopulateIDs is one: it bounds abuse, not a feature a
// tenant would buy more of. 3 is generous for the shapes populate exists to
// serve (a post's author, that author's avatar) and still short enough that a
// caller cannot use it to walk an arbitrarily deep object graph one entry at a
// time.
const maxPopulateDepth = 3

// maxPopulateIDs caps the DEDUPLICATED ids one page may expand — TOTAL across
// every level of every dotted path, not per level. A code constant rather than
// a plan dimension, for the same reason domain.MaxMultipleElements is one: it
// is an abuse backstop, not something anyone would price. The number it bounds
// is limit × populated fields × elements per field × depth, and depth joins
// the other factors as of Amendment 8: three levels of expansion multiply the
// fan-out by up to 3x for the same reason two populated fields already did.
//
// 500 is chosen against the default page: it is well past limit=100 with
// several single-valued relations (the ordinary shape, which dedupes hard when
// a page shares authors), and it bites only on the fan-out cases where the
// honest answer is "ask for a smaller page or a shallower populate".
const maxPopulateIDs = 500

// populateNode is one resolved segment of a ?populate= path: the relation
// field on the PARENT type, the type its values point at, and — when the
// caller asked for more than one segment — the child nodes to expand INSIDE
// each fetched target.
//
// Two different dotted values that share a prefix (`author.avatar` and
// `author.bio`) resolve to the SAME node for `author` with two children,
// rather than two separate walks: populateLevel fetches `author` once per
// page either way, and merging here is what makes that true instead of merely
// intended.
//
// The target type is resolved during PARSING, not while walking the page, and
// that ordering is deliberate — it mirrors parseProjection's. A key naming an
// unreadable type costs one round trip and a 403, rather than a full page of
// work followed by a refusal.
type populateNode struct {
	field    domain.Field
	target   *domain.ContentType
	children []*populateNode
}

// RelatedEntries is one populated relation field's value on the wire.
//
// It exists to keep CARDINALITY faithful to the schema rather than to the data.
// A single-valued relation renders an object or `null`; a multi-valued one
// renders an array, empty if nothing at the other end is live. Rendering a
// one-element array for a scalar field (or unwrapping a one-element array for a
// multi field) would reintroduce exactly the type instability that keeping ids
// in `data` avoids.
//
// A single-valued field with no live target renders `null` rather than being
// omitted, and a multi-valued one renders `[]`. The key's PRESENCE is the
// signal: "you asked me to expand this and there is nothing live at the other
// end" is a different answer from "you did not ask", and a consumer that cannot
// tell them apart will retry the request that already succeeded.
type RelatedEntries struct {
	multiple bool
	items    []EntryDTO
}

// MarshalJSON renders the cardinality described on RelatedEntries. The nil
// slice under `multiple` becomes `[]` and not `null`: an empty array is what
// "no live targets" means for a list, and json.Marshal of a nil slice would
// spell it as the absence of a list instead.
func (r RelatedEntries) MarshalJSON() ([]byte, error) {
	if r.multiple {
		if len(r.items) == 0 {
			return []byte(`[]`), nil
		}
		return json.Marshal(r.items)
	}
	if len(r.items) == 0 {
		return []byte(`null`), nil
	}
	return json.Marshal(r.items[0])
}

// parsePopulate resolves every ?populate= value into a forest of populateNode,
// refusing every path it cannot honour BEFORE a row is read.
//
// The refusals are named separately rather than collapsed into one "bad
// populate key", because the causes have different fixes and a caller cannot
// guess which applies: too many segments, a typo, a field that is real but not
// a relation, and a field this credential may not read at all.
//
// The last of those reuses errFieldQueryForbidden — the SAME 403 filter, sort
// and fields answer — on the same reasoning parseProjection gives: silently
// dropping the key would tell a caller that a relation it may not see is empty.
//
// The TARGET type gets guardTypeRead too, at EVERY segment, and that check is
// the one this function exists to make impossible to forget. Field-level
// permission lives on the parent's field; the collection at the other end has
// its own read_roles, and populate is a read of that collection — nested
// segments read a FURTHER collection the same way. Without this, a public type
// with a relation into a restricted one would hand the restricted entries out
// through the parent — the parent's own permissions saying nothing about it.
// The write path already draws this line at one level (checkRelations calls
// guardTypeRead on the related type); this is the read half of the same rule,
// walked to whatever depth the caller asked for.
func (s *contentService) parsePopulate(ctx context.Context, ct *domain.ContentType, sub authn.Subject, raw []string) ([]*populateNode, error) {
	// Resolved once per TYPE NAME across the WHOLE parse, not once per segment:
	// `author` and `reviewer` both naming `person`, at any depth, is the
	// ordinary shape, and checkRelations already caches it this way on the
	// write side.
	targets := map[string]*domain.ContentType{}
	var roots []*populateNode
	for _, raw := range raw {
		key := strings.TrimSpace(raw)
		if key == "" {
			// An empty ?populate= is a URL builder spelling "no selection",
			// exactly as it is for ?fields=. Treated as absent so the parameter
			// can be built by string concatenation.
			continue
		}
		segments := strings.Split(key, ".")
		if len(segments) > maxPopulateDepth {
			// Checked before any segment is looked up, so a path that is merely
			// too long is answered as "too deep" rather than as an unknown field
			// on whichever segment the walk happened to reach first.
			return nil, apperrors.New("CONTENT_POPULATE_TOO_DEEP",
				fmt.Sprintf("populate expands at most %d levels", maxPopulateDepth), 400).
				WithDetails(map[string]any{"populate": key, "depth": len(segments), "max_depth": maxPopulateDepth})
		}
		curType := ct
		siblings := &roots
		for _, seg := range segments {
			seg = strings.TrimSpace(seg)
			node, err := s.resolvePopulateSegment(ctx, curType, sub, seg, key, targets, siblings)
			if err != nil {
				return nil, err
			}
			curType = node.target
			siblings = &node.children
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	return roots, nil
}

// resolvePopulateSegment resolves ONE dotted segment against curType, reusing
// an existing sibling node when this segment was already reached by another
// populate value (see parsePopulate's doc comment on merging).
func (s *contentService) resolvePopulateSegment(ctx context.Context, curType *domain.ContentType, sub authn.Subject, seg, fullKey string, targets map[string]*domain.ContentType, siblings *[]*populateNode) (*populateNode, error) {
	for _, n := range *siblings {
		if n.field.Key == seg {
			return n, nil
		}
	}
	field, ok := curType.FieldByKey(seg)
	if !ok {
		return nil, apperrors.New("CONTENT_POPULATE_FIELD_UNKNOWN", "populate field is not defined", 400).
			WithDetails(map[string]any{"field": seg, "populate": fullKey, "known": relationKeys(curType, sub)})
	}
	if field.Type != domain.FieldTypeRelation {
		return nil, apperrors.New("CONTENT_POPULATE_NOT_RELATION", "populate is only meaningful for a relation field", 400).
			WithDetails(map[string]any{"field": seg, "populate": fullKey, "type": field.Type, "known": relationKeys(curType, sub)})
	}
	if !canReadField(field, sub) {
		return nil, errFieldQueryForbidden(seg, "populate")
	}
	target, ok := targets[field.RelationEntity]
	if !ok {
		t, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, field.RelationEntity)
		if err != nil {
			// DeleteContentType refuses a type any relation field still names
			// (CONTENT_TYPE_REFERENCED), so this is a broken invariant rather
			// than a caller mistake. Reported as one: a 404 here would say the
			// PARENT entry does not exist, sending whoever reads the log after
			// the wrong row entirely.
			return nil, apperrors.Wrap("CONTENT_POPULATE_TARGET_MISSING", "relation target type is missing", 500, err).
				WithDetails(map[string]any{"field": seg, "relation_entity": field.RelationEntity})
		}
		target = t
		targets[field.RelationEntity] = t
	}
	if err := guardTypeRead(target, sub); err != nil {
		return nil, err
	}
	node := &populateNode{field: field, target: target}
	*siblings = append(*siblings, node)
	return node, nil
}

// relationKeys lists the keys ?populate= would accept on this type, for an
// error's details. It lists only the fields this caller may READ: naming a
// restricted field here would turn a 400 about a typo into a disclosure of the
// schema's hidden half, which is the one thing the 403 above is careful not to
// do.
//
// No cap on the list. A type's field count is already bounded by the plan's
// MaxFieldsPerType, and the relation subset of it is smaller still.
func relationKeys(ct *domain.ContentType, sub authn.Subject) []string {
	out := []string{}
	for _, f := range ct.Fields {
		if f.Type == domain.FieldTypeRelation && canReadField(f, sub) {
			out = append(out, f.Key)
		}
	}
	return out
}

// guardPopulateAudience is the ONE audience ?populate= is still refused for.
//
// Delivery and admin both expand — the same grammar, the same depth cap, the
// same 500-id budget, the same `related` sibling key. What differs is which
// COPY is read at both ends, and that is not a permission question but the same
// question ProjectEntry already answers for `data`: delivery reads the
// published snapshot, admin reads the working copy. ADR-006 Amendment 7 settled
// it; Amendment 6 had left admin on a 400 (CONTENT_POPULATE_FORBIDDEN, now
// retired) because the draft view needed a decision, never because it was
// unsafe.
//
// PREVIEW keeps its 403, and that one IS a scope rule rather than a shape rule.
// A preview credential names exactly ONE entry — guardPreviewCollection exists
// so it cannot be turned into a general read key — and populate is precisely a
// way to read OTHER entries with it. Expanding them under the delivery audience
// (published snapshots only) would be safe, but it would also make `related`
// describe a different copy from `data` on the one response where `data` is a
// working copy, and the whole EntryAudience apparatus exists to keep those two
// from drifting.
func guardPopulateAudience(aud EntryAudience, populate []string) error {
	if len(populate) == 0 {
		return nil
	}
	if aud == audiencePreview {
		return apperrors.New("CONTENT_POPULATE_PREVIEW_UNSUPPORTED", "a preview credential addresses one entry; it cannot expand relations to others", 403).
			WithDetails(map[string]any{"populate": populate})
	}
	return nil
}

// populateBudget tracks the DEDUPLICATED relation ids fetched across the whole
// populate walk — every level of every dotted path — against maxPopulateIDs.
// A pointer shared by every populateLevel call in one request, because the cap
// is a property of the REQUEST, not of any one level: a path three segments
// deep that fans out 200 ids at each level must be refused for the same reason
// 600 ids at one level already was.
type populateBudget struct {
	seen map[uuid.UUID]struct{}
}

func newPopulateBudget() *populateBudget {
	return &populateBudget{seen: make(map[uuid.UUID]struct{})}
}

// add folds ids into the running total and refuses once the DEDUPLICATED count
// exceeds maxPopulateIDs. Refused rather than truncated, exactly as the
// single-level cap always was: a silently short `related` is indistinguishable
// from "those targets are not live", so a consumer would render a page with
// entries missing and no reason to retry.
func (b *populateBudget) add(ids []uuid.UUID) error {
	for _, id := range ids {
		b.seen[id] = struct{}{}
	}
	if len(b.seen) > maxPopulateIDs {
		return apperrors.New("CONTENT_POPULATE_TOO_MANY", "this page names more relation targets than one request may expand", 400).
			WithDetails(map[string]any{
				"count": len(b.seen),
				"max":   maxPopulateIDs,
				"hint":  "ask for a smaller limit, fewer populate fields, or a shallower populate path",
			})
	}
	return nil
}

// populateFor expands the relation fields of one PAGE — every dotted path, to
// whatever depth each was parsed to — in a bounded number of extra queries
// (one per LEVEL of depth actually requested, not one per entry), and returns
// the top-level `related` map for each entry by id. Nested levels are attached
// to the returned EntryDTOs themselves (EntryDTO.Related), so a client reading
// `related.author.related.avatar` finds the second level exactly where the
// first level's shape says an expanded entry's own relations would be.
//
// See populateLevel for the walk; this wrapper only seeds it with an empty
// ancestor path and a fresh budget.
func (s *contentService) populateFor(ctx context.Context, specs []*populateNode, entries []*domain.Entry, sub authn.Subject) (map[uuid.UUID]map[string]RelatedEntries, error) {
	if len(specs) == 0 || len(entries) == 0 {
		return nil, nil
	}
	return s.populateLevel(ctx, specs, entries, sub, nil, newPopulateBudget())
}

// populateLevel expands ONE level of populateNode specs against `entries`,
// then recurses into any node that named children — one extra query per node
// per level, exactly the batching populateFor promises: collect every id the
// level names across every entry and every spec, deduplicate, fetch, and only
// then build the per-entry values from the index. The obvious one-pass
// spelling — fetch as each value is met — is the N+1 that makes populate cost
// more than the round trips it was meant to save, and it stays N+1-free at
// every depth, not just the first.
//
// The ids are read from the copy the READER is being served, never from a copy
// chosen by this function. For delivery that is the published snapshot: a
// working copy that has added a relation, or pointed one somewhere else, must
// not have that edit expanded — the id itself would be an observation of an
// unreleased change, which is the leak ADR-006 Amendment 4 closed for filters
// and Amendment 5 for sorts. For admin it is the working copy, for the mirror
// reason: an editor who repoints a relation and then asks what is at the other
// end must be shown what they are about to publish, not what is still live.
// The SAME rule applies at every level: a nested entry's own relations are
// read from ITS published snapshot for delivery, its working copy for admin —
// populateSourcePayload takes only the audience, never a level, so this falls
// out for free.
//
// The audience is derived from the SUBJECT here rather than taken as a
// parameter, and that is the same refusal ProjectEntry makes: a read path
// cannot pass the wrong audience because it cannot pass one at all. Both places
// the copy is chosen — the ids read out, and the rows read back in — hang off
// this single value, so `related` can never describe a different copy from
// `data`, at any depth.
//
// `ancestors` is the set of entry ids ALREADY being rendered above this level
// on this branch — the root page's own entries, plus every target fetched at
// a shallower level along the same path. Before recursing into a node's
// children for a specific fetched target, populateLevel checks the target
// against this set: a self-referencing relation that points back at an entry
// already on the branch is rendered (the caller asked for one more level and
// gets the object, not a hole) but NOT expanded again — expanding it would
// only reproduce a subtree this same walk already computed, and doing that on
// every occurrence is the "infinite work within a bounded depth" the maxPopulateDepth
// cap alone does not prevent. maxPopulateDepth already makes the walk
// terminate; this makes it not repeat itself while doing so.
func (s *contentService) populateLevel(ctx context.Context, specs []*populateNode, entries []*domain.Entry, sub authn.Subject, ancestors map[uuid.UUID]struct{}, budget *populateBudget) (map[uuid.UUID]map[string]RelatedEntries, error) {
	if len(specs) == 0 || len(entries) == 0 {
		return nil, nil
	}
	aud := audienceFor(sub)

	// The path this level and everything under it must not re-expand: every
	// ancestor, plus every entry BEING expanded at this level (an entry whose
	// own relation points at itself is a one-hop cycle, not a two-hop one).
	path := make(map[uuid.UUID]struct{}, len(ancestors)+len(entries))
	for id := range ancestors {
		path[id] = struct{}{}
	}
	for _, e := range entries {
		path[e.ID] = struct{}{}
	}

	wanted := make(map[uuid.UUID]map[string][]uuid.UUID, len(entries))
	var newIDs []uuid.UUID
	seenNew := map[uuid.UUID]struct{}{}
	for _, e := range entries {
		doc := decodeObject(populateSourcePayload(aud, e))
		perField := make(map[string][]uuid.UUID, len(specs))
		for _, sp := range specs {
			ids := relationIDs(doc[sp.field.Key], sp.field.Multiple)
			perField[sp.field.Key] = ids
			for _, id := range ids {
				if _, ok := seenNew[id]; !ok {
					seenNew[id] = struct{}{}
					newIDs = append(newIDs, id)
				}
			}
		}
		wanted[e.ID] = perField
	}
	if err := budget.add(newIDs); err != nil {
		return nil, err
	}
	// The one query for THIS level. matchPublished narrows to rows the public
	// may see (published status AND a snapshot); dropping it is what makes a
	// never-published target expandable for an editor, which is the whole
	// point of Amendment 7. Both spellings keep tenancy: the predicate binds
	// tenant_id and withTenant sets app.tenant_id for RLS, so an id another
	// tenant put in a payload is invisible either way.
	rows, err := s.repo.GetEntriesByIDs(ctx, sub.TenantID, newIDs, aud == audienceDelivery)
	if err != nil {
		return nil, err
	}
	index := make(map[uuid.UUID]*domain.Entry, len(rows))
	for _, r := range rows {
		index[r.ID] = r
	}

	out := make(map[uuid.UUID]map[string]RelatedEntries, len(entries))
	for _, e := range entries {
		out[e.ID] = make(map[string]RelatedEntries, len(specs))
	}

	for _, sp := range specs {
		type occurrence struct {
			parent uuid.UUID
			row    *domain.Entry
		}
		var occurrences []occurrence
		recurseSeen := map[uuid.UUID]struct{}{}
		var recurseRows []*domain.Entry

		for _, e := range entries {
			for _, id := range wanted[e.ID][sp.field.Key] {
				target, ok := index[id]
				if !ok {
					// Not live: deleted, never published, retracted, or another
					// tenant's. Silence is the ADR-006 answer — delivery sees
					// the live world and nothing else, so a dangling reference
					// is not an error condition, it is Tuesday. A 4xx here
					// would let one unpublished entry take down every page that
					// happens to link to it.
					continue
				}
				// A row whose type is not the one the field declares cannot be
				// rendered with that type's field permissions, so it is treated
				// as absent rather than projected with the wrong mask. Reaching
				// this needs a relation_entity edited out from under stored
				// values; the check costs a comparison and removes the class.
				if target.ContentTypeID != sp.target.ID {
					continue
				}
				// Own-only confinement, at whatever level this is. A role the
				// TARGET type confines may not read a colleague's row through
				// the parent's relation any more than through the target's own
				// list — the list applies it as a WHERE clause (confinedAuthor),
				// and this is the same rule where the rows arrive by id
				// instead. Absent rather than refused, exactly as an
				// unpublished target is for delivery: guardOwned's whole point
				// is that a confined editor cannot tell a colleague's row from
				// an id that names nothing.
				//
				// A no-op for delivery — confinedAuthor answers nil for a
				// public credential — so this costs the delivery path a nil
				// check and changes none of its bytes.
				if guardOwned(sp.target, sub, target) != nil {
					continue
				}
				occurrences = append(occurrences, occurrence{parent: e.ID, row: target})
				if len(sp.children) == 0 {
					continue
				}
				if _, onPath := path[target.ID]; onPath {
					// This exact entry is already being rendered above it on
					// this branch — expanding it again would only recompute a
					// subtree this walk already has. It still renders as the
					// object at THIS level; it just does not grow a `related`
					// of its own here.
					continue
				}
				if _, dup := recurseSeen[target.ID]; !dup {
					recurseSeen[target.ID] = struct{}{}
					recurseRows = append(recurseRows, target)
				}
			}
		}

		var nested map[uuid.UUID]map[string]RelatedEntries
		if len(recurseRows) > 0 {
			nested, err = s.populateLevel(ctx, sp.children, recurseRows, sub, path, budget)
			if err != nil {
				return nil, err
			}
		}

		byParent := make(map[uuid.UUID][]EntryDTO, len(entries))
		for _, occ := range occurrences {
			// The SAME projector and the SAME subject as the parent, so the
			// related entry gets the delivery shape (snapshot as `data`, no
			// updated_at, no authorship) and the TARGET type's field-level
			// masking. Deliberately NOT narrowedTo: ?fields= is the ROOT
			// caller's projection, and applying it to a different type's
			// payload at ANY depth would drop every key whose name happens
			// not to be in it.
			dto := ProjectEntry(sp.target, occ.row, sub)
			if rel, ok := nested[occ.row.ID]; ok {
				dto = dto.withRelated(rel)
			}
			byParent[occ.parent] = append(byParent[occ.parent], dto)
		}
		for _, e := range entries {
			out[e.ID][sp.field.Key] = RelatedEntries{multiple: sp.field.Multiple, items: byParent[e.ID]}
		}
	}
	return out, nil
}

// populateSourcePayload is the copy whose relation ids get expanded — the same
// choice ProjectEntry makes for `data`, kept in one place so the ids that get
// expanded can never come from a different document than the ids that get
// rendered. Used at EVERY level: a nested entry's own relations are read from
// its published snapshot for delivery and its working copy for admin, the
// identical rule as the root entry's.
//
// Delivery reads the snapshot and admin reads the working copy. Preview is
// listed with delivery only because guardPopulateAudience refused it long
// before this runs; if that ever changes, the answer here has to be decided,
// not inherited.
func populateSourcePayload(aud EntryAudience, e *domain.Entry) json.RawMessage {
	if aud == audienceAdmin {
		return e.Payload
	}
	if len(e.PublishedPayload) == 0 {
		return nil
	}
	return e.PublishedPayload
}

// decodeObject reads a payload into its top-level keys. An undecodable document
// yields no keys rather than an error: the entry is still served (its `data` is
// the stored bytes), and the honest report for a relation field nobody can read
// is "nothing live at the other end".
//
// Top-level only, deliberately: a relation field nested inside a component or a
// dynamic-zone block's payload is not reachable this way, and ?populate= does
// not reach it either — see the package doc comment above and
// TestComponentQueryRefusals.
func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc
}

// relationIDs reads one relation field's stored value as the ids it names, in
// order. Cardinality comes from the SCHEMA (f.Multiple), not from what the JSON
// happens to look like, so a scalar field holding an array — which validation
// does not permit — yields nothing rather than quietly changing this field's
// response shape.
//
// A value that is absent, JSON null, or not a uuid yields no id. All three are
// "nothing to expand", and none of them is worth failing a page over: the write
// path already refuses them, so reaching here means stored data that predates
// or bypassed it, and a 500 on read would make that entry permanently
// unservable rather than merely unexpandable.
func relationIDs(raw json.RawMessage, multiple bool) []uuid.UUID {
	if len(raw) == 0 {
		return nil
	}
	parse := func(s string) (uuid.UUID, bool) {
		id, err := uuid.Parse(s)
		return id, err == nil
	}
	if !multiple {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil
		}
		if id, ok := parse(s); ok {
			return []uuid.UUID{id}
		}
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make([]uuid.UUID, 0, len(list))
	for _, s := range list {
		if id, ok := parse(s); ok {
			out = append(out, id)
		}
	}
	return out
}
