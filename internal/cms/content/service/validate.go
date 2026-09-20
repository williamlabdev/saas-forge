package service

import (
	"encoding/json"
	"time"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// validatePayload is a pure, data-driven check of an entry payload against a
// content type's fields. It enforces: no undefined keys, required presence, and
// per-type value shape (including enum membership). Relation existence is a
// stateful check done by the service, not here. It returns the first violation
// it finds, iterating fields in definition order so messages are deterministic.
func validatePayload(fields []domain.Field, payload map[string]any) error {
	defined := make(map[string]domain.Field, len(fields))
	for _, f := range fields {
		defined[f.Key] = f
	}

	// Reject keys that are not part of the schema.
	for k := range payload {
		if _, ok := defined[k]; !ok {
			return errFieldUnknown(k)
		}
	}

	for _, f := range fields {
		v, present := payload[f.Key]
		if !present || v == nil {
			if f.Required {
				return errFieldRequired(f.Key)
			}
			continue
		}
		if err := validateValue(f, v); err != nil {
			return err
		}
	}
	return nil
}

// validateValue dispatches on CARDINALITY first, then delegates every element to
// the same scalar rules. One arm set serves both shapes, so a scalar rule and an
// element rule cannot drift apart.
func validateValue(f domain.Field, v any) error {
	// A component value is an object (or an array of them) shaped by the
	// component's own fields, so it takes the recursive path before the
	// cardinality split below: the multi arm's duplicate check hashes each
	// element, and an object is not hashable.
	if f.Type == domain.FieldTypeComponent {
		return validateComponentValue(f, v)
	}
	// A dynamic zone is array-SHAPED but not `multiple` (a list of lists is not
	// a thing an editor or an error path can express), so it takes its own arm
	// before the cardinality split for the same reason the component arm does.
	if f.Type == domain.FieldTypeDynamicZone {
		return validateZoneValue(f, v)
	}
	if !f.Multiple {
		// A rich text value is array-SHAPED but scalar-CARDINALITY (one document);
		// its empty form is [] rather than absence, so `required` must catch it
		// here, exactly as the multi arm below catches an empty array. Without
		// this, a required body field is satisfiable by a document with nothing
		// in it — the constraint dodged through the exact channel it polices.
		if f.Type == domain.FieldTypeRichText && f.Required {
			if xs, ok := v.([]any); ok && len(xs) == 0 {
				return errFieldRequired(f.Key)
			}
		}
		return validateScalar(f, v)
	}
	xs, ok := v.([]any)
	if !ok {
		return errFieldTypeMismatch(f.Key, f.Type)
	}
	// An empty array is not a value. Letting it satisfy `required` would make the
	// constraint unenforceable through the exact channel it exists to police, so
	// it reports the same error as an absent key — callers should not need two
	// branches for "you gave me nothing".
	if f.Required && len(xs) == 0 {
		return errFieldRequired(f.Key)
	}
	if len(xs) > domain.MaxMultipleElements {
		return errFieldTooManyValues(f.Key, len(xs), domain.MaxMultipleElements)
	}
	seen := make(map[any]struct{}, len(xs))
	for i, x := range xs {
		if err := validateScalar(f, x); err != nil {
			return withIndex(err, i)
		}
		// Rejected, not deduped. Silently deduping returns bytes the caller did
		// not send — the thing a byte-identical round-trip standard exists to
		// catch — and containment queries cannot tell ["a","a"] from ["a"] anyway,
		// so a duplicate is meaningless to every question this system can answer.
		// Rejecting stays reversible; deduping does not, because callers come to
		// depend on it.
		if _, dup := seen[x]; dup {
			return errFieldDuplicateValue(f.Key, i, x)
		}
		seen[x] = struct{}{}
	}
	return nil
}

// validateComponentValue applies ADR-020 §4: one object when the field is
// scalar, an array of at most MaxMultipleElements objects when it is
// multiple, every object validated against the component's sub-fields by
// the SAME validatePayload the top level uses — unknown keys refused,
// required sub-fields enforced, every value through validateScalar. Errors
// name the sub-field as "parent.child" and, for a multiple field, carry the
// item index under "item".
func validateComponentValue(f domain.Field, v any) error {
	if !f.Multiple {
		item, ok := v.(map[string]any)
		if !ok {
			return errFieldTypeMismatch(f.Key, "object")
		}
		return validatePayload(prefixed(f.Key, f.ComponentFields, item))
	}
	xs, ok := v.([]any)
	if !ok {
		return errFieldTypeMismatch(f.Key, "array of objects")
	}
	if f.Required && len(xs) == 0 {
		return errFieldRequired(f.Key)
	}
	if len(xs) > domain.MaxMultipleElements {
		return errFieldTooManyValues(f.Key, len(xs), domain.MaxMultipleElements)
	}
	for i, x := range xs {
		item, ok := x.(map[string]any)
		if !ok {
			return withItem(errFieldTypeMismatch(f.Key, "object"), f, i)
		}
		if err := validatePayload(prefixed(f.Key, f.ComponentFields, item)); err != nil {
			return withItem(err, f, i)
		}
	}
	return nil
}

// validateZoneValue applies ADR-020 Amendment 1: the value is ALWAYS an array
// of at most MaxMultipleElements objects, each naming its component under
// ZoneDiscriminator, each body validated against THAT component's sub-fields
// by the same validatePayload / prefixed pair a component item goes through.
// Two items of different components in one zone are therefore checked by two
// different field sets, which is the whole point of the type.
//
// The discriminator is stripped before the body is validated. Leaving it in
// would earn CONTENT_FIELD_UNKNOWN for "zone.__component" — an error naming a
// key the schema deliberately does not define, for a value the caller was
// required to send.
func validateZoneValue(f domain.Field, v any) error {
	xs, ok := v.([]any)
	if !ok {
		return errFieldTypeMismatch(f.Key, "array of objects")
	}
	// Empty is not a value, exactly as for a multi-valued field: `required`
	// means at least one item, or the constraint is dodged through the one
	// channel it exists to police.
	if f.Required && len(xs) == 0 {
		return errFieldRequired(f.Key)
	}
	if len(xs) > domain.MaxMultipleElements {
		return errFieldTooManyValues(f.Key, len(xs), domain.MaxMultipleElements)
	}
	for i, x := range xs {
		item, ok := x.(map[string]any)
		if !ok {
			return withItem(errFieldTypeMismatch(f.Key, "object"), f, i)
		}
		name, _ := item[domain.ZoneDiscriminator].(string)
		if name == "" {
			return withItem(errZoneComponentMissing(f.Key), f, i)
		}
		if !f.ZoneAllows(name) {
			return withItem(errZoneComponentNotAllowed(f.Key, name, f.ZoneComponents), f, i)
		}
		if err := validatePayload(prefixed(f.Key, f.ZoneFields[name], zoneItemBody(item))); err != nil {
			return withItem(err, f, i)
		}
	}
	return nil
}

// zoneItems returns the item objects a dynamic zone's value holds, skipping
// anything that is not an object — the collectors walk items without
// re-deciding the shape validateZoneValue already accepted, the same contract
// componentItems keeps.
func zoneItems(v any) []map[string]any {
	if v == nil {
		return nil
	}
	xs, _ := v.([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if item, ok := x.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// zoneItemComponent returns the component an item names, or "" when it names
// none. Kept next to zoneItemBody so the two agree on which key is the
// discriminator.
func zoneItemComponent(item map[string]any) string {
	name, _ := item[domain.ZoneDiscriminator].(string)
	return name
}

// zoneItemBody is the item without its discriminator: the object that is
// shaped by the component's sub-fields, and the only part any component-level
// code should see. A copy, not a delete — the caller's document is the thing
// being validated, not rewritten.
func zoneItemBody(item map[string]any) map[string]any {
	body := make(map[string]any, len(item))
	for k, v := range item {
		if k == domain.ZoneDiscriminator {
			continue
		}
		body[k] = v
	}
	return body
}

// componentItems returns the objects a component field's value holds — one
// for a scalar field, each element for a multiple one, none when absent —
// so the collectors can walk items without re-deciding the shape
// validateComponentValue already accepted.
func componentItems(f domain.Field, v any) []map[string]any {
	if v == nil {
		return nil
	}
	if !f.Multiple {
		if item, ok := v.(map[string]any); ok {
			return []map[string]any{item}
		}
		return nil
	}
	xs, _ := v.([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if item, ok := x.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// prefixed returns a component's sub-fields AND one item with their keys
// prefixed by the referencing field's key, so every error raised one level
// down names the path ("hero.title") an editor can find, without a second
// set of error constructors. Both sides are prefixed together on purpose: a
// prefixed field list over an unprefixed item would report every key of the
// item as unknown, and a lookup of data["hero.title"] would find nothing.
func prefixed(parent string, subs []domain.Field, item map[string]any) ([]domain.Field, map[string]any) {
	fields := make([]domain.Field, len(subs))
	for i, sf := range subs {
		fields[i] = sf
		fields[i].Key = parent + "." + sf.Key
	}
	doc := make(map[string]any, len(item))
	for k, v := range item {
		doc[parent+"."+k] = v
	}
	return fields, doc
}

// pruneUndefined drops keys the schema no longer defines. It is applied to the
// MERGE BASE of an update, never to the caller's patch, so a client typo still
// earns CONTENT_FIELD_UNKNOWN.
//
// What it is FOR is residual drift: a row hand-repaired in SQL, a restored
// backup, an import that went around the service. Any of those leaves a key the
// schema does not define, and without pruning the entry is un-PATCHable with an
// error naming a field the caller never sent.
//
// It is deliberately NOT justified as closing the delete-vs-write race, which
// an earlier version of this comment claimed. That race does not reach here:
// DeleteField bumps `version` on every row it strips, so a write built on a
// pre-delete read is rejected by the optimistic lock before validation runs —
// and a row that never held the key has nothing to prune. Keeping the weaker
// argument would have made this function look load-bearing for a case it never
// sees.
func pruneUndefined(payload json.RawMessage, fields []domain.Field) json.RawMessage {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return payload
	}
	defined := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		defined[f.Key] = struct{}{}
	}
	pruned := false
	for k := range doc {
		if _, ok := defined[k]; !ok {
			delete(doc, k)
			pruned = true
		}
	}
	if !pruned {
		return payload
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

func validateScalar(f domain.Field, v any) error {
	switch f.Type {
	case domain.FieldTypeString, domain.FieldTypeText, domain.FieldTypeFile:
		if _, ok := v.(string); !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
		// Shape first, then the rules on top of it. CheckValue is the same
		// function the tightening guard runs over stored values, which is what
		// keeps "would this be refused" and "is this refused" one question.
		if vio := f.CheckValue(f.Type, v); vio != nil {
			return errFieldConstraint(f.Key, vio)
		}
	case domain.FieldTypeNumber:
		// JSON numbers decode to float64.
		if _, ok := v.(float64); !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
		if vio := f.CheckValue(f.Type, v); vio != nil {
			return errFieldConstraint(f.Key, vio)
		}
	case domain.FieldTypeBoolean:
		if _, ok := v.(bool); !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
	case domain.FieldTypeEnum:
		s, ok := v.(string)
		if !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
		if !f.InEnum(s) {
			return errFieldEnumInvalid(f.Key, f.EnumValues)
		}
	case domain.FieldTypeDate:
		s, ok := v.(string)
		if !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
	case domain.FieldTypeDateTime:
		s, ok := v.(string)
		if !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
	case domain.FieldTypeRelation:
		// Relation values are UUID strings; existence is checked by the service.
		if _, ok := v.(string); !ok {
			return errFieldTypeMismatch(f.Key, f.Type)
		}
	case domain.FieldTypeRichText:
		// Shape only — whether an image block's media_id names a live, uploaded
		// asset is a stateful check done by the service (collectMediaRefs),
		// exactly as relation existence is.
		if vio := domain.ValidateRichText(v); vio != nil {
			return errRichTextInvalid(f.Key, vio)
		}
	default:
		// Fail closed. Until multi-valued fields landed, an unrecognised type was
		// unreachable — buildField rejects unknown types at definition time — so
		// falling through to nil was merely unused. It is worth closing now
		// because the hole would no longer be one unvalidated value per field but
		// one per ELEMENT, and because nothing in the DATABASE constrains
		// field_type, so a row written around the service reaches here.
		return errFieldTypeMismatch(f.Key, f.Type)
	}
	return nil
}
