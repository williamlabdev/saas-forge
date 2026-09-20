package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Schema mutation. The governing rule for every decision in this file:
//
//	After a schema change commits, every stored entry of that type — in BOTH
//	payload and published_payload — must still validate against the new schema.
//	The API either migrates the data in the same transaction, or it refuses.
//
// This is not fastidiousness. validatePayload checks the WHOLE document
// (validate.go), so data left invalid does not merely block writes to the
// offending field: the entry becomes un-PATCHable, and the error names a field
// the caller never touched. Worse, SetEntryStatus does not re-validate, so that
// same entry can still be published and served.
//
// Two shapes recur below and are worth naming once:
//
//   - Renames are their OWN verb, never a property on the PATCH. Same reasoning
//     as publish/unpublish: a routine label edit must not be able to rewrite
//     every stored document because someone put the wrong string in a field.
//   - Refusals are NAMED. Every rejected property has its own code rather than
//     relying on DisallowUnknownFields to produce a generic 400 — ADR-006
//     Amendment 1b spent a whole ruling closing that pattern.

// UpdateTypeInput is the mutable surface of a content type. `name` is absent on
// purpose: renaming is a separate verb.
//
// The three permission lists are pointers and Label is not, and the asymmetry is
// not an oversight. Label has always been a plain string here — an omitted label
// clears it, which is the behaviour this endpoint shipped with. A permission list
// cannot work that way: nil must mean "not sent, leave the stored list alone"
// while an explicit `[]` means "unrestrict this", because collapsing those two
// would make dropping a key from a PATCH silently open a collection.
type UpdateTypeInput struct {
	Label string `json:"label"`

	ReadRoles    *[]string `json:"read_roles"`
	WriteRoles   *[]string `json:"write_roles"`
	OwnOnlyRoles *[]string `json:"own_only_roles"`
}

// touchesPermissions reports whether the PATCH names any permission list, which
// is the question the escalation guard asks. PRESENCE, not difference: "it was
// already what I sent" is not something the caller gets to establish, because
// asking the question at all is the administrative act.
func (in UpdateTypeInput) touchesPermissions() bool {
	return in.ReadRoles != nil || in.WriteRoles != nil || in.OwnOnlyRoles != nil
}

type RenameInput struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// UpdateFieldInput is the mutable surface of a field. The three immutable
// properties are DECLARED here and rejected by name; leaving them out would
// still refuse them (via DisallowUnknownFields) but with an opaque message, and
// would hide the refusal from the next reader of this struct.
//
// read_roles and write_roles are MUTABLE and belong in the top group, which is
// the point of putting them there rather than beside the one-way doors below:
// the immutable three are immutable because changing them invalidates stored
// VALUES, and a permission change invalidates none — it changes who may see or
// send a value that stays exactly as it is. Granting and revoking is the whole
// feature, so a one-way door would make the first typo permanent.
//
// Pointers, like the rest: nil means "not sent" and leaves the stored list
// alone, while an explicit `[]` is a real instruction meaning "unrestrict this
// field". Collapsing those two onto one plain slice would make dropping the key
// from a PATCH silently open the field.
type UpdateFieldInput struct {
	Label      *string   `json:"label"`
	Required   *bool     `json:"required"`
	EnumValues *[]string `json:"enum_values"`
	ReadRoles  *[]string `json:"read_roles"`
	WriteRoles *[]string `json:"write_roles"`

	Type           *string `json:"type"`
	Multiple       *bool   `json:"multiple"`
	RelationEntity *string `json:"relation_entity"`
	Component      *string `json:"component"`

	// Components is the dynamic zone's allowed list and is MUTABLE, unlike the
	// four one-way doors above it. It sits here rather than with them because
	// the reason those are immutable does not apply to it in one direction:
	// ADDING a name invalidates no stored value (no item can already name a
	// component the list did not allow). Removing one can, and that half is
	// guarded by a count rather than by a refusal — the same shape enum
	// narrowing has (ADR-020 Amendment 1).
	Components *[]string `json:"components"`

	// Description is MUTABLE and unguarded — documentation, so no stored value
	// can be made invalid by rewording it. A pointer for the reason Label is:
	// absent means "leave it", and `""` is a real instruction meaning "clear
	// the sentence".
	Description *string `json:"description"`

	// The constraint attributes. Pointers for the same reason the fields above
	// are: absent means "leave it". Min and Max need a third state — `null`
	// clears a bound, and a *float64 cannot tell null from absent — hence
	// OptionalNumber.
	Unique  *bool          `json:"unique"`
	Format  *string        `json:"format"`
	Pattern *string        `json:"pattern"`
	Min     OptionalNumber `json:"min"`
	Max     OptionalNumber `json:"max"`
}

// OptionalNumber is a JSON number that distinguishes absent (Set false) from
// null (Set true, Value nil) from a value.
type OptionalNumber struct {
	Set   bool
	Value *float64
}

func (o *OptionalNumber) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	o.Value = &f
	return nil
}

func (o OptionalNumber) MarshalJSON() ([]byte, error) {
	if !o.Set || o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.Value)
}

func (s *contentService) UpdateContentType(ctx context.Context, typeName string, in UpdateTypeInput) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaAmend, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	// Changing a collection's permission is a SECOND, stricter decision — the
	// same guard UpdateField carries over the field lists, and it is load-bearing
	// for the same reason: this endpoint runs on content:update, which an EDITOR
	// holds, so without it an editor could PATCH read_roles to [] and hand
	// themselves a collection restricted to admins. A content verb changes
	// content; deciding who may see it is administration.
	if in.touchesPermissions() {
		if _, err := s.authorize(ctx, ActionContentSchemaWrite, typeName, typeName); err != nil {
			return ContentTypeDTO{}, err
		}
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	// A copy, so the stored lists are carried forward for anything the PATCH did
	// not name. UpdateContentTypeDefinition writes all three unconditionally, and
	// that contract only holds if the caller of it has already decided what every
	// one of them should be.
	next := *ct
	next.Label = strings.TrimSpace(in.Label)

	// read_roles and write_roles have no data guard and need none: no count of
	// entries can make a grant or a revoke unsafe to store, because neither
	// touches a stored value. A revoke takes effect on the next read, including
	// for a session already open — the correct direction to be abrupt in.
	if in.ReadRoles != nil {
		roles, err := normalizeTypeRoles(typeName, "read_roles", *in.ReadRoles)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		next.ReadRoles = roles
	}
	if in.WriteRoles != nil {
		roles, err := normalizeTypeRoles(typeName, "write_roles", *in.WriteRoles)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		next.WriteRoles = roles
	}
	if in.OwnOnlyRoles != nil {
		roles, err := normalizeTypeRoles(typeName, "own_only_roles", *in.OwnOnlyRoles)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		// Confinement is the ONE list here that is checked against stored data,
		// and only when it TIGHTENS. Entries with no recorded author match no
		// author, so a newly confined role would find them simply gone — the only
		// refusal in this whole permission stack that is indistinguishable from
		// data loss, and therefore the only one worth refusing up front. Relaxing
		// confinement, or leaving it where it is, can hide nothing new.
		if added := missingFrom(roles, ct.OwnOnlyRoles); len(added) > 0 {
			n, err := s.repo.CountEntriesWithoutAuthor(ctx, sub.TenantID, ct.ID)
			if err != nil {
				return ContentTypeDTO{}, err
			}
			if n > 0 {
				return ContentTypeDTO{}, errOwnOnlyBackfill(typeName, added, n)
			}
		}
		next.OwnOnlyRoles = roles
	}

	now := time.Now().UTC()
	next.UpdatedAt = now
	if err := s.repo.UpdateContentTypeDefinition(ctx, sub.TenantID, &next, now); err != nil {
		return ContentTypeDTO{}, err
	}
	return s.contentTypeDTO(ctx, &next, sub), nil
}

func (s *contentService) RenameContentType(ctx context.Context, typeName string, in RenameInput) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	newName := strings.TrimSpace(in.Name)
	if !domain.ValidName(newName) {
		return ContentTypeDTO{}, apperrors.New("CONTENT_TYPE_NAME_INVALID", "invalid content type name", 422).
			WithDetails(map[string]any{"name": in.Name})
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	if newName == ct.Name {
		return s.contentTypeDTO(ctx, ct, sub), nil
	}
	now := time.Now().UTC()
	if err := s.repo.RenameContentType(ctx, sub.TenantID, ct.ID, ct.Name, newName, now); err != nil {
		return ContentTypeDTO{}, err
	}
	ct.Name, ct.UpdatedAt = newName, now
	return s.contentTypeDTO(ctx, ct, sub), nil
}

// DeleteContentType refuses rather than cascading. content_type_fields and
// entries both carry ON DELETE CASCADE, so one DELETE would silently destroy a
// tenant's entire collection.
//
// There is deliberately no force flag, and the asymmetry with DeleteField is the
// point: deleting a field destroys one key per entry while ids, relations, media
// links and publish state all survive — it is re-enterable. Deleting a type
// destroys the ids that relation fields and external consumers hold, and cannot
// be re-entered. A force flag on the irreversible one is the kind of thing that
// ends up in a script.
func (s *contentService) DeleteContentType(ctx context.Context, typeName string) (err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, typeName, typeName)
	if err != nil {
		return err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return err
	}
	n, err := s.repo.CountEntriesForType(ctx, sub.TenantID, ct.ID)
	if err != nil {
		return err
	}
	if n > 0 {
		return apperrors.New("CONTENT_TYPE_HAS_ENTRIES", "content type still has entries", 409).
			WithDetails(map[string]any{"type": ct.Name, "entries": n})
	}
	// A referrer's own schema gives no hint that its writes are about to start
	// failing — relation_entity names a type that would no longer exist, and
	// checkRelations resolves it per write.
	refs, err := s.repo.ListRelationReferrers(ctx, sub.TenantID, ct.Name)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		by := make([]map[string]string, 0, len(refs))
		for _, r := range refs {
			by = append(by, map[string]string{"type": r.TypeName, "field": r.FieldKey})
		}
		return apperrors.New("CONTENT_TYPE_REFERENCED", "content type is referenced by a relation field", 409).
			WithDetails(map[string]any{"type": ct.Name, "referenced_by": by})
	}
	return s.repo.DeleteContentType(ctx, sub.TenantID, ct.ID)
}

func (s *contentService) UpdateField(ctx context.Context, typeName, key string, in UpdateFieldInput) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaAmend, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	if err := refuseImmutableFieldProps(key, in); err != nil {
		return ContentTypeDTO{}, err
	}
	// Changing a permission list is a SECOND, stricter decision, and skipping it
	// would make this endpoint a privilege escalation rather than a schema edit:
	// UpdateField runs on content:update, which an EDITOR holds, so an editor
	// could hand themselves a field restricted to admins by PATCHing read_roles
	// to []. The same asymmetry the destructive verbs already draw — a content
	// verb changes content, deciding who may see it is administration.
	//
	// Checked on PRESENCE, not on whether the value differs. "It was already
	// what I sent" is not a thing the caller gets to establish; asking the
	// question at all is the administrative act.
	if in.ReadRoles != nil || in.WriteRoles != nil {
		if _, err := s.authorize(ctx, ActionContentSchemaWrite, typeName, typeName); err != nil {
			return ContentTypeDTO{}, err
		}
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	f, ok := ct.FieldByKey(key)
	if !ok {
		return ContentTypeDTO{}, errFieldNotFound(key)
	}

	if in.Label != nil {
		f.Label = strings.TrimSpace(*in.Label)
	}
	if in.Description != nil {
		f.Description = strings.TrimSpace(*in.Description)
	}
	if in.EnumValues != nil {
		if f.Type != domain.FieldTypeEnum {
			return ContentTypeDTO{}, errEnumNotApplicable(key, f.Type)
		}
		vals := *in.EnumValues
		if len(vals) == 0 {
			return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_ENUM_EMPTY", "enum field needs enum_values", 422).
				WithDetails(map[string]any{"field": key})
		}
		if dup, ok := firstDuplicate(vals); ok {
			return ContentTypeDTO{}, errEnumDuplicate(key, dup)
		}
		// Narrowing the set is the bricking case: an entry still holding a
		// removed value fails its next PATCH on a field the caller did not send.
		// Adding and reordering are always safe, so only the removals are
		// counted.
		if removed := missingFrom(f.EnumValues, vals); len(removed) > 0 {
			n, err := s.repo.CountEntriesWithValuesOutside(ctx, sub.TenantID, ct.ID, f, vals)
			if err != nil {
				return ContentTypeDTO{}, err
			}
			if n > 0 {
				return ContentTypeDTO{}, apperrors.New("CONTENT_ENUM_VALUE_IN_USE", "enum values are still in use", 409).
					WithDetails(map[string]any{"field": key, "values": removed, "entries": n})
			}
		}
		f.EnumValues = vals
	}
	if in.Components != nil {
		next, err := normalizeZoneComponents(key, f.Type, *in.Components)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		// Every name must still name a live component. Checked for the WHOLE
		// list, not just the additions: a list can also arrive naming a
		// component deleted since it was written, and accepting it would store
		// a name the validator then has no sub-fields for — every item of that
		// type refused with no field named.
		// The sub-fields are re-attached from the same lookup, not left as
		// they were: ZoneFields is DERIVED from the list, and a field carrying
		// the new list with the old fields would validate the next item of an
		// added component against nothing at all.
		zoneFields := make(map[string][]domain.Field, len(next))
		for _, name := range next {
			c, err := s.repo.GetComponentByName(ctx, sub.TenantID, name)
			if err != nil {
				if errors.Is(err, apperrors.ErrNotFound) {
					return ContentTypeDTO{}, errComponentNotFound(key, name)
				}
				return ContentTypeDTO{}, err
			}
			zoneFields[name] = c.Fields
		}
		// Removals are the bricking half, exactly as with enum narrowing: an
		// item whose __component is no longer allowed fails its entry's next
		// PATCH on a field the caller did not send. Counted per removed name so
		// the error can say WHICH one is still in use.
		ref := repository.ComponentRef{TypeID: ct.ID, TypeName: ct.Name, FieldKey: key, Zone: true}
		for _, name := range missingFrom(f.ZoneComponents, next) {
			n, err := s.repo.CountEntriesWithZoneComponent(ctx, sub.TenantID, ref, name)
			if err != nil {
				return ContentTypeDTO{}, err
			}
			if n > 0 {
				return ContentTypeDTO{}, errZoneComponentInUse(key, name, n)
			}
		}
		f.ZoneComponents, f.ZoneFields = next, zoneFields
	}
	if in.Required != nil {
		// Relaxing is always safe. Tightening over rows that lack the field
		// would make every one of them un-PATCHable.
		if *in.Required && !f.Required {
			n, err := s.repo.CountEntriesMissingField(ctx, sub.TenantID, ct.ID, key)
			if err != nil {
				return ContentTypeDTO{}, err
			}
			if n > 0 {
				return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_REQUIRED_BACKFILL", "entries are missing this field", 409).
					WithDetails(map[string]any{"field": key, "entries": n})
			}
		}
		f.Required = *in.Required
	}
	if in.Unique != nil || in.Format != nil || in.Pattern != nil || in.Min.Set || in.Max.Set {
		next := f.FieldConstraints
		if in.Unique != nil {
			next.Unique = *in.Unique
		}
		if in.Format != nil {
			next.Format = *in.Format
		}
		if in.Pattern != nil {
			next.Pattern = *in.Pattern
		}
		if in.Min.Set {
			next.Min = in.Min.Value
		}
		if in.Max.Set {
			next.Max = in.Max.Value
		}
		next = normalizeConstraints(next)
		if cerr := domain.ValidateFieldConstraints(f.Type, f.Multiple, next); cerr != nil {
			return ContentTypeDTO{}, errConstraintDefinition(key, cerr)
		}
		// Uniqueness is a fact about OTHER rows and is asked of the database;
		// the other four are facts about each value and are asked of the same
		// rule the validator applies. Relaxing either is free.
		if next.Unique && !f.Unique {
			dups, err := s.repo.ListDuplicateFieldValues(ctx, sub.TenantID, ct.ID, f)
			if err != nil {
				return ContentTypeDTO{}, err
			}
			if len(dups) > 0 {
				return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_UNIQUE_DUPLICATES", "entries already share a value in this field", 409).
					WithDetails(map[string]any{"field": key, "duplicates": dups})
			}
		}
		if next.Tightens(f.FieldConstraints) {
			probe := f
			probe.FieldConstraints = next
			n, err := s.countConstraintViolations(ctx, sub.TenantID, ct.ID, probe)
			if err != nil {
				return ContentTypeDTO{}, err
			}
			if n > 0 {
				return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_CONSTRAINT_BACKFILL", "entries hold values that would fail the new constraints", 409).
					WithDetails(map[string]any{"field": key, "entries": n, "constraints": next.Describe()})
			}
		}
		f.FieldConstraints = next
	}
	// Permission lists have no data guard and need none: no count of entries can
	// make a grant or a revoke unsafe to store, because neither touches a stored
	// value. The only refusals are the definition-time ones normalizeRoles
	// raises — an unknown role, or a repeat.
	//
	// A revoke takes effect on the next read, including for a session already
	// open. That is the correct direction to be abrupt in; the reverse (a grant
	// that needs a re-login) is the one that gets worked around.
	if in.ReadRoles != nil {
		roles, err := normalizeRoles(key, *in.ReadRoles)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		f.ReadRoles = roles
	}
	if in.WriteRoles != nil {
		roles, err := normalizeRoles(key, *in.WriteRoles)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		f.WriteRoles = roles
	}

	now := time.Now().UTC()
	if err := s.repo.UpdateFieldDefinition(ctx, sub.TenantID, ct, f, now); err != nil {
		return ContentTypeDTO{}, err
	}
	return s.contentTypeDTO(ctx, withField(ct, f, now), sub), nil
}

func (s *contentService) RenameField(ctx context.Context, typeName, key string, in RenameInput) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	newKey := strings.TrimSpace(in.Key)
	if !domain.ValidFieldKey(newKey) {
		return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_KEY_INVALID", "invalid field key", 422).
			WithDetails(map[string]any{"field": in.Key})
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	f, ok := ct.FieldByKey(key)
	if !ok {
		return ContentTypeDTO{}, errFieldNotFound(key)
	}
	if newKey == key {
		return s.contentTypeDTO(ctx, ct, sub), nil
	}
	if _, taken := ct.FieldByKey(newKey); taken {
		return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_EXISTS", "field already defined", 409).
			WithDetails(map[string]any{"field": newKey})
	}
	now := time.Now().UTC()
	if err := s.repo.RenameField(ctx, sub.TenantID, ct, key, newKey, writeActorOf(sub), now); err != nil {
		return ContentTypeDTO{}, err
	}
	f.Key = newKey
	return s.contentTypeDTO(ctx, withFieldAt(ct, key, f, now), sub), nil
}

// DeleteField strips the key from every entry, in both copies, in the same
// transaction as the definition removal — see the repository for why leaving
// orphan keys is the broken option rather than the cheap one.
func (s *contentService) DeleteField(ctx context.Context, typeName, key string, force bool) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	f, ok := ct.FieldByKey(key)
	if !ok {
		return ContentTypeDTO{}, errFieldNotFound(key)
	}
	// Otherwise the API could reach a state CreateContentType refuses to
	// produce.
	if len(ct.Fields) == 1 {
		return ContentTypeDTO{}, apperrors.New("CONTENT_TYPE_NO_FIELDS", "a content type needs at least one field", 422).
			WithDetails(map[string]any{"type": ct.Name})
	}
	if !force {
		// Deleting a field you just mistyped into existence stays frictionless;
		// deleting one holding four thousand values makes you say so.
		n, err := s.repo.CountEntriesWithField(ctx, sub.TenantID, ct.ID, key)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		if n > 0 {
			return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_HAS_DATA", "field still holds data", 409).
				WithDetails(map[string]any{"field": key, "entries": n})
		}
	}
	now := time.Now().UTC()
	if err := s.repo.DeleteField(ctx, sub.TenantID, ct, f, writeActorOf(sub), now); err != nil {
		return ContentTypeDTO{}, err
	}
	return s.contentTypeDTO(ctx, withoutField(ct, key, now), sub), nil
}

// --- helpers ----------------------------------------------------------------

// refuseImmutableFieldProps rejects the three properties that cannot change,
// each by name.
//
// `type` is the most concrete of the three: buildWhere emits
// `(payload ->> key)::numeric` for a number field, so a field flipped to number
// with any non-numeric value still stored makes the admin LIST page raise a cast
// error — a 500 on a page the operator never touched, from data they cannot
// see. Nothing is lost by refusing: add a new field, migrate the content, delete
// the old one is three explicit, inspectable steps the caller can already
// sequence. What is refused is the IMPLICIT migration.
// countConstraintViolations counts the entries of a type holding, in the
// working copy or the live snapshot, a value of f that fails f's format,
// pattern or range. One entry counts once however many of its values fail. It
// is the guard behind CONTENT_FIELD_CONSTRAINT_BACKFILL and it runs the SAME
// rule the validator runs (CheckValue), over the same decoded shapes.
func (s *contentService) countConstraintViolations(ctx context.Context, tenantID string, ctID uuid.UUID, f domain.Field) (int, error) {
	fails := func(v any) bool {
		if v == nil {
			return false
		}
		if !f.Multiple {
			return f.CheckValue(f.Type, v) != nil
		}
		xs, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range xs {
			if f.CheckValue(f.Type, x) != nil {
				return true
			}
		}
		return false
	}
	n := 0
	err := s.repo.ScanFieldValues(ctx, tenantID, ctID, f.Key, func(_ uuid.UUID, working, live any) error {
		if fails(working) || fails(live) {
			n++
		}
		return nil
	})
	return n, err
}

func refuseImmutableFieldProps(key string, in UpdateFieldInput) error {
	if in.Type != nil {
		return errFieldPropImmutable("CONTENT_FIELD_TYPE_IMMUTABLE", key, "type")
	}
	if in.Multiple != nil {
		return errFieldPropImmutable("CONTENT_FIELD_MULTIPLE_IMMUTABLE", key, "multiple")
	}
	if in.RelationEntity != nil {
		return errFieldPropImmutable("CONTENT_FIELD_RELATION_IMMUTABLE", key, "relation_entity")
	}
	if in.Component != nil {
		return errFieldPropImmutable("CONTENT_FIELD_COMPONENT_IMMUTABLE", key, "component")
	}
	return nil
}

func firstDuplicate(vals []string) (string, bool) {
	seen := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		if _, dup := seen[v]; dup {
			return v, true
		}
		seen[v] = struct{}{}
	}
	return "", false
}

// missingFrom returns the members of old that are absent from next.
func missingFrom(old, next []string) []string {
	keep := make(map[string]struct{}, len(next))
	for _, v := range next {
		keep[v] = struct{}{}
	}
	var gone []string
	for _, v := range old {
		if _, ok := keep[v]; !ok {
			gone = append(gone, v)
		}
	}
	return gone
}

// withField / withFieldAt / withoutField rebuild the in-memory type so the
// response reflects the write without a second round trip, mirroring how
// AddField appends. Order is preserved because it is the DTO's field order.
func withField(ct *domain.ContentType, f domain.Field, now time.Time) *domain.ContentType {
	return withFieldAt(ct, f.Key, f, now)
}

func withFieldAt(ct *domain.ContentType, atKey string, f domain.Field, now time.Time) *domain.ContentType {
	out := *ct
	out.Fields = make([]domain.Field, len(ct.Fields))
	copy(out.Fields, ct.Fields)
	for i := range out.Fields {
		if out.Fields[i].Key == atKey {
			out.Fields[i] = f
		}
	}
	out.UpdatedAt = now
	return &out
}

func withoutField(ct *domain.ContentType, key string, now time.Time) *domain.ContentType {
	out := *ct
	out.Fields = make([]domain.Field, 0, len(ct.Fields))
	for _, x := range ct.Fields {
		if x.Key != key {
			out.Fields = append(out.Fields, x)
		}
	}
	out.UpdatedAt = now
	return &out
}
