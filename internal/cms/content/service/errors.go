package service

import (
	"errors"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Business exceptions for the content domain. Payload-validation errors carry
// the offending field in details so the API response ({code,message,details})
// pinpoints what to fix. All use 422 Unprocessable Entity: the request was
// well-formed JSON but violated the runtime schema.

func errFieldRequired(key string) error {
	return apperrors.New("CONTENT_FIELD_REQUIRED", "missing required field", 422).
		WithDetails(map[string]any{"field": key})
}

func errFieldTypeMismatch(key, expected string) error {
	return apperrors.New("CONTENT_FIELD_TYPE_MISMATCH", "field value has the wrong type", 422).
		WithDetails(map[string]any{"field": key, "expected": expected})
}

func errFieldEnumInvalid(key string, allowed []string) error {
	return apperrors.New("CONTENT_FIELD_ENUM_INVALID", "field value is not an allowed enum value", 422).
		WithDetails(map[string]any{"field": key, "allowed": allowed})
}

// errFieldConstraint is the 422 a value gets for failing its field's format,
// pattern or range. `constraint` names which, `expected` says the rule, so the
// message an editor sees ("pattern", "^[a-z]+$") is the definition they can
// go and read.
func errFieldConstraint(key string, v *domain.ConstraintViolation) error {
	return apperrors.New("CONTENT_FIELD_CONSTRAINT_VIOLATED", "field value does not satisfy the field's "+v.Attr, 422).
		WithDetails(map[string]any{"field": key, "constraint": v.Attr, "expected": v.Expected})
}

// errConstraintDefinition is the 422 a field DEFINITION gets for a constraint
// set that is wrong on its own terms — the same code the artifact diff reports
// as a refusal for the same input.
func errConstraintDefinition(key string, e *domain.ConstraintDefinitionError) error {
	return apperrors.New(e.Code, e.Detail, 422).
		WithDetails(map[string]any{"field": key, "constraint": e.Attr})
}

func errFieldUnknown(key string) error {
	return apperrors.New("CONTENT_FIELD_UNKNOWN", "field is not defined on this content type", 422).
		WithDetails(map[string]any{"field": key})
}

func errFieldNotFound(key string) error {
	return apperrors.New("CONTENT_FIELD_NOT_FOUND", "field is not defined on this content type", 404).
		WithDetails(map[string]any{"field": key})
}

// errFieldPropImmutable names WHICH property was refused and points at the
// lossless alternative. A generic "invalid request body" would leave the caller
// guessing which of the keys they sent was the problem.
func errFieldPropImmutable(code, key, prop string) error {
	return apperrors.New(code, "this field property cannot be changed after creation", 422).
		WithDetails(map[string]any{
			"field": key, "property": prop,
			"hint": "add a new field, migrate the values, then delete the old one",
		})
}

func errEnumDuplicate(key, value string) error {
	return apperrors.New("CONTENT_FIELD_ENUM_DUPLICATE", "enum_values contains a repeated value", 422).
		WithDetails(map[string]any{"field": key, "value": value})
}

func errEnumNotApplicable(key, fieldType string) error {
	return apperrors.New("CONTENT_FIELD_ENUM_NOT_APPLICABLE", "enum_values is meaningless on this field type", 422).
		WithDetails(map[string]any{"field": key, "type": fieldType})
}

func errFieldTooManyValues(key string, got, limit int) error {
	return apperrors.New("CONTENT_FIELD_TOO_MANY_VALUES", "multi-valued field has too many values", 422).
		WithDetails(map[string]any{"field": key, "count": got, "limit": limit})
}

func errFieldDuplicateValue(key string, index int, value any) error {
	return apperrors.New("CONTENT_FIELD_DUPLICATE_VALUE", "multi-valued field has a repeated value", 422).
		WithDetails(map[string]any{"field": key, "index": index, "value": value})
}

func errFieldMultipleUnsupported(key, fieldType string) error {
	return apperrors.New("CONTENT_FIELD_MULTIPLE_UNSUPPORTED", "this field type cannot hold multiple values", 422).
		WithDetails(map[string]any{"field": key, "type": fieldType, "allowed": domain.AllowedMultipleTypes()})
}

// withIndex points an element-level failure at the offending array position.
// Without it a forty-element list tells you something is wrong and gives you no
// way to find it. Implemented as a merge onto the existing details so the four
// element-level codes (type mismatch, enum, relation invalid, relation missing)
// all gain the position without four new codes.
func withIndex(err error, i int) error {
	var ae *apperrors.AppError
	if errors.As(err, &ae) {
		return ae.WithDetail("index", i)
	}
	return err
}

// errRichTextInvalid carries the violation's PATH into details, because the
// value under validation is a document: "field body is invalid" against a
// 400-block article is a needle-in-haystack error, "[17].children[2].marks[0]"
// is a fix.
func errRichTextInvalid(key string, v *domain.RichTextViolation) error {
	return apperrors.New("CONTENT_RICHTEXT_INVALID", "rich text value violates the block grammar", 422).
		WithDetails(map[string]any{"field": key, "path": v.Path, "reason": v.Reason})
}

func errRelationInvalid(key, value string) error {
	return apperrors.New("CONTENT_RELATION_INVALID", "relation value is not a valid UUID", 422).
		WithDetails(map[string]any{"field": key, "value": value})
}

func errRelationNotFound(key, value string) error {
	return apperrors.New("CONTENT_RELATION_NOT_FOUND", "related entry does not exist in this tenant", 422).
		WithDetails(map[string]any{"field": key, "value": value})
}

// withItem points an error raised inside a MULTIPLE component field, or
// inside a dynamic zone, at the item it came from (ADR-020 §4 and Amendment
// 1). "index" is taken — a multi-valued sub-field inside the item may already
// have set it — so the item position travels under its own key. A SCALAR
// component field has no position to report; a zone always does, even though
// it never carries Multiple, because its value is always a list.
func withItem(err error, f domain.Field, i int) error {
	if !f.Multiple && f.Type != domain.FieldTypeDynamicZone {
		return err
	}
	var ae *apperrors.AppError
	if errors.As(err, &ae) {
		return ae.WithDetail("item", i)
	}
	return err
}

func errComponentRequired(key string) error {
	return apperrors.New("CONTENT_COMPONENT_REQUIRED", "component field needs component", 422).
		WithDetails(map[string]any{"field": key})
}

func errComponentNotApplicable(key, fieldType string) error {
	return apperrors.New("CONTENT_COMPONENT_NOT_APPLICABLE", "component is meaningless on this field type", 422).
		WithDetails(map[string]any{"field": key, "type": fieldType})
}

func errComponentNotFound(key, name string) error {
	return apperrors.New("CONTENT_COMPONENT_NOT_FOUND", "component is not defined", 422).
		WithDetails(map[string]any{"field": key, "component": name})
}

// errComponentSubField is the 422 a sub-field DEFINITION gets for nesting or
// for an attribute a sub-field cannot carry — the same codes the artifact
// diff reports as refusals for the same input.
func errComponentSubField(key string, v *domain.ComponentSubFieldViolation) error {
	details := map[string]any{"field": key}
	if v.Attribute != "" {
		details["attribute"] = v.Attribute
	}
	return apperrors.New(v.Code, v.Detail, 422).WithDetails(details)
}

func errZoneComponentsRequired(key string) error {
	return apperrors.New("CONTENT_ZONE_COMPONENTS_REQUIRED", "dynamiczone field needs a non-empty components list", 422).
		WithDetails(map[string]any{"field": key})
}

func errZoneComponentsNotApplicable(key, fieldType string) error {
	return apperrors.New("CONTENT_ZONE_COMPONENTS_NOT_APPLICABLE", "components is meaningless on this field type", 422).
		WithDetails(map[string]any{"field": key, "type": fieldType})
}

func errZoneComponentDuplicate(key, name string) error {
	return apperrors.New("CONTENT_ZONE_COMPONENT_DUPLICATE", "components lists the same component twice", 422).
		WithDetails(map[string]any{"field": key, "component": name})
}

// errZoneComponentMissing is the item that never said what it is. It is
// separate from CONTENT_ZONE_COMPONENT_NOT_ALLOWED because the fixes differ:
// this one is "add __component", that one is "use one of these".
func errZoneComponentMissing(key string) error {
	return apperrors.New("CONTENT_ZONE_COMPONENT_MISSING", "dynamiczone item must name its component under "+domain.ZoneDiscriminator, 422).
		WithDetails(map[string]any{"field": key, "discriminator": domain.ZoneDiscriminator})
}

func errZoneComponentNotAllowed(key, name string, allowed []string) error {
	return apperrors.New("CONTENT_ZONE_COMPONENT_NOT_ALLOWED", "this dynamiczone does not accept that component", 422).
		WithDetails(map[string]any{"field": key, "component": name, "allowed": allowed})
}

// errZoneComponentInUse refuses REMOVING a component from a zone's
// allowed-list while entries still hold items of it — the same bargain
// CONTENT_COMPONENT_IN_USE strikes one level up, and the same 409. Details
// name the field and the component rather than listing entry ids: the count
// is what decides whether this is a mistake or a migration, and the ids are a
// query away through the zone's own filter-free listing.
func errZoneComponentInUse(key, name string, entries int) error {
	return apperrors.New("CONTENT_ZONE_COMPONENT_IN_USE", "entries still hold items of this component", 409).
		WithDetails(map[string]any{"field": key, "component": name, "entries": entries})
}
