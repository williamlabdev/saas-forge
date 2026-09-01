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
