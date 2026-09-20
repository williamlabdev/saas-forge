package domain

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// FieldConstraints are the value-level rules a field may carry on top of its
// type: uniqueness, a named format, a regular expression, and a range. They are
// ATTRIBUTES, not types, and that is the whole design. A `slug` field type would
// have bought the same thing while touching the vendored contract's type enum,
// which four hand-synced copies and a parity test all depend on (see Field.
// Multiple, which made the same call for the same reason). A slug is a string
// with `format: slug` and `unique: true`; nothing else in the system has to
// learn a new type for that to be true.
//
// All five are MUTABLE, and each of them is a one-way door in exactly one
// direction. Tightening a constraint can invalidate stored values — and because
// validatePayload checks the WHOLE document, a single stored value that no
// longer passes makes every entry holding it un-PATCHable, with an error naming
// a field the caller never touched. So tightening is GUARDED (the database is
// asked whether any stored value would fail, and the change is refused with a
// count while one would), and relaxing is free. That is the same shape
// `required` and enum removal already have, and the diff grades it the same way.
//
// Which types may carry which constraint is fixed here so that buildField, the
// artifact diff and the validator all agree:
//
//	unique   string, number, date, datetime — scalar only. Uniqueness of a list
//	         has three readings (whole list, each element, each element across
//	         rows) and none of them is obviously the one meant.
//	format   string only; the one format today is `slug`.
//	pattern  string and text; applied to every element of a multi-valued string.
//	min/max  string and text (length, in characters), number (value); applied to
//	         every element of a multi-valued field, never to its length.
type FieldConstraints struct {
	// Unique makes the value one-of-a-kind among the entries of its type, PER
	// LOCALE, counting both the working copy and the live snapshot of every
	// entry: a value is taken while ANY entry still holds it in either copy, so
	// an editor never learns at publish time that a draft they saved a week ago
	// collides with something live. A retracted snapshot (ADR-014 §5.1) holds
	// nothing — it is not live and not being edited.
	//
	// Enforced by the database (entry_unique_values, migration 000040), not by a
	// read-then-write in the service, so two concurrent creates cannot both win.
	Unique bool `json:"unique,omitempty"`
	// Format names a fixed rule the value must satisfy. "" means none. The one
	// format is FieldFormatSlug.
	Format string `json:"format,omitempty"`
	// Pattern is an RE2 regular expression (Go syntax) the WHOLE value must
	// match; the anchors are implied, so `[a-z]+` means `^[a-z]+$`. "" means
	// none. Compiled at definition time and refused if it does not compile, so
	// an editor never meets a regexp error on a content save.
	Pattern string `json:"pattern,omitempty"`
	// Min / Max bound the value: a length in characters (runes) for string and
	// text, the numeric value for number. nil means unbounded on that side.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// FieldFormatSlug is the one named format: lowercase ASCII letters and digits in
// hyphen-separated runs, no leading, trailing or doubled hyphens — the shape a
// URL path segment has after every framework's slugify. The rule is fixed rather
// than tenant-configurable on purpose: a slug is something two systems agree on,
// and a tenant who wants a different shape wants `pattern`, not a different
// meaning for the word.
const FieldFormatSlug = "slug"

// MaxPatternLength bounds a pattern's SOURCE, not what it matches. RE2 has no
// catastrophic backtracking, so this is about the definition staying readable
// in an artifact and in an error message, not about safety.
const MaxPatternLength = 512

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// IsSlug reports whether s satisfies FieldFormatSlug.
func IsSlug(s string) bool { return slugRE.MatchString(s) }

// AllowedFieldFormats is the closed set `format` accepts.
func AllowedFieldFormats() []string { return []string{FieldFormatSlug} }

// UniqueAllowedFor reports whether type t may be declared unique.
func UniqueAllowedFor(t string) bool {
	switch t {
	case FieldTypeString, FieldTypeNumber, FieldTypeDate, FieldTypeDateTime:
		return true
	}
	return false
}

// FormatAllowedFor reports whether type t may carry a named format.
func FormatAllowedFor(t string) bool { return t == FieldTypeString }

// PatternAllowedFor reports whether type t may carry a pattern.
func PatternAllowedFor(t string) bool { return t == FieldTypeString || t == FieldTypeText }

// RangeAllowedFor reports whether type t may carry min/max.
func RangeAllowedFor(t string) bool {
	return t == FieldTypeString || t == FieldTypeText || t == FieldTypeNumber
}

// IsZero reports whether no constraint is set.
func (c FieldConstraints) IsZero() bool {
	return !c.Unique && c.Format == "" && c.Pattern == "" && c.Min == nil && c.Max == nil
}

// Equal compares two constraint sets by value (Min/Max by the number they point
// at, not by pointer).
func (c FieldConstraints) Equal(o FieldConstraints) bool {
	return c.Unique == o.Unique && c.Format == o.Format && c.Pattern == o.Pattern &&
		floatPtrEqual(c.Min, o.Min) && floatPtrEqual(c.Max, o.Max)
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Describe renders the set the way a plan or an error message would show it,
// e.g. `unique, format=slug, min=1, max=80`. Empty for the zero set.
func (c FieldConstraints) Describe() string {
	var parts []string
	if c.Unique {
		parts = append(parts, "unique")
	}
	if c.Format != "" {
		parts = append(parts, "format="+c.Format)
	}
	if c.Pattern != "" {
		parts = append(parts, fmt.Sprintf("pattern=%q", c.Pattern))
	}
	if c.Min != nil {
		parts = append(parts, "min="+formatBound(*c.Min))
	}
	if c.Max != nil {
		parts = append(parts, "max="+formatBound(*c.Max))
	}
	return strings.Join(parts, ", ")
}

func formatBound(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// ConstraintDefinitionError is a constraint set that cannot be stored — not one
// the data disagrees with, one that is wrong on its own terms. Code is the
// stable error code the service returns as 422 and the artifact diff reports as
// a refusal, so a plan and a PATCH disagree about nothing.
type ConstraintDefinitionError struct {
	Code   string
	Detail string
	// Attr names the offending attribute (unique, format, pattern, min, max).
	Attr string
}

func (e *ConstraintDefinitionError) Error() string { return e.Detail }

// ValidateFieldConstraints decides whether c may be stored on a field of type t
// with the given cardinality. Every refusal is by name.
func ValidateFieldConstraints(t string, multiple bool, c FieldConstraints) *ConstraintDefinitionError {
	notApplicable := func(attr string) *ConstraintDefinitionError {
		return &ConstraintDefinitionError{
			Code: "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", Attr: attr,
			Detail: fmt.Sprintf("%s fields cannot carry %s", t, attr),
		}
	}
	if c.Unique {
		if !UniqueAllowedFor(t) {
			return notApplicable("unique")
		}
		if multiple {
			return &ConstraintDefinitionError{
				Code: "CONTENT_FIELD_UNIQUE_MULTIPLE", Attr: "unique",
				Detail: "a multi-valued field cannot be unique",
			}
		}
	}
	if c.Format != "" {
		if !FormatAllowedFor(t) {
			return notApplicable("format")
		}
		known := false
		for _, f := range AllowedFieldFormats() {
			known = known || f == c.Format
		}
		if !known {
			return &ConstraintDefinitionError{
				Code: "CONTENT_FIELD_FORMAT_UNKNOWN", Attr: "format",
				Detail: fmt.Sprintf("unknown format %q", c.Format),
			}
		}
	}
	if c.Pattern != "" {
		if !PatternAllowedFor(t) {
			return notApplicable("pattern")
		}
		if utf8.RuneCountInString(c.Pattern) > MaxPatternLength {
			return &ConstraintDefinitionError{
				Code: "CONTENT_FIELD_PATTERN_INVALID", Attr: "pattern",
				Detail: fmt.Sprintf("pattern is longer than %d characters", MaxPatternLength),
			}
		}
		if _, err := compilePattern(c.Pattern); err != nil {
			return &ConstraintDefinitionError{
				Code: "CONTENT_FIELD_PATTERN_INVALID", Attr: "pattern",
				Detail: "pattern does not compile: " + err.Error(),
			}
		}
	}
	if c.Min != nil || c.Max != nil {
		if !RangeAllowedFor(t) {
			if c.Min != nil {
				return notApplicable("min")
			}
			return notApplicable("max")
		}
		for _, b := range []struct {
			attr string
			v    *float64
		}{{"min", c.Min}, {"max", c.Max}} {
			if b.v == nil {
				continue
			}
			if math.IsNaN(*b.v) || math.IsInf(*b.v, 0) {
				return &ConstraintDefinitionError{
					Code: "CONTENT_FIELD_RANGE_INVALID", Attr: b.attr,
					Detail: b.attr + " must be a finite number",
				}
			}
			// A length is a count of characters: whole and non-negative, or the
			// bound means nothing a value could satisfy or fail on purpose.
			if t != FieldTypeNumber && (*b.v < 0 || *b.v != math.Trunc(*b.v)) {
				return &ConstraintDefinitionError{
					Code: "CONTENT_FIELD_RANGE_INVALID", Attr: b.attr,
					Detail: b.attr + " is a length on a " + t + " field and must be a non-negative whole number",
				}
			}
		}
		if c.Min != nil && c.Max != nil && *c.Min > *c.Max {
			return &ConstraintDefinitionError{
				Code: "CONTENT_FIELD_RANGE_INVALID", Attr: "min",
				Detail: fmt.Sprintf("min %s is greater than max %s", formatBound(*c.Min), formatBound(*c.Max)),
			}
		}
	}
	return nil
}

// ConstraintViolation is a stored or offered value that fails one constraint.
// Uniqueness is not among these: it is a fact about OTHER rows, decided by the
// database, and surfaces as its own error from the repository.
type ConstraintViolation struct {
	// Attr is the constraint that failed: format, pattern, min or max.
	Attr string
	// Expected is the rule in the words an error message shows: the format
	// name, the pattern source, or the bound.
	Expected string
}

// CheckValue tests one SCALAR value (already known to be of the field's type —
// a string for string/text, a float64 for number) against the format, pattern
// and range. The type check stays with the validator; this is only the part
// that depends on the constraint set, so the write-time validator and the
// tightening guard share one rule and cannot drift.
func (c FieldConstraints) CheckValue(t string, v any) *ConstraintViolation {
	switch t {
	case FieldTypeString, FieldTypeText:
		s, ok := v.(string)
		if !ok {
			return nil
		}
		if c.Format == FieldFormatSlug && !IsSlug(s) {
			return &ConstraintViolation{Attr: "format", Expected: FieldFormatSlug}
		}
		if c.Pattern != "" {
			re, err := compilePattern(c.Pattern)
			if err != nil || !re.MatchString(s) {
				return &ConstraintViolation{Attr: "pattern", Expected: c.Pattern}
			}
		}
		n := float64(utf8.RuneCountInString(s))
		if c.Min != nil && n < *c.Min {
			return &ConstraintViolation{Attr: "min", Expected: formatBound(*c.Min) + " characters"}
		}
		if c.Max != nil && n > *c.Max {
			return &ConstraintViolation{Attr: "max", Expected: formatBound(*c.Max) + " characters"}
		}
	case FieldTypeNumber:
		f, ok := v.(float64)
		if !ok {
			return nil
		}
		if c.Min != nil && f < *c.Min {
			return &ConstraintViolation{Attr: "min", Expected: formatBound(*c.Min)}
		}
		if c.Max != nil && f > *c.Max {
			return &ConstraintViolation{Attr: "max", Expected: formatBound(*c.Max)}
		}
	}
	return nil
}

// Tightens reports whether moving from `from` to `c` can invalidate a value that
// `from` accepted — the direction the schema diff grades as guarded. Any change
// to format or pattern that leaves one in place counts: two regular expressions
// are not comparable, so the only honest answer is to ask the data.
func (c FieldConstraints) Tightens(from FieldConstraints) bool {
	if c.Format != "" && c.Format != from.Format {
		return true
	}
	if c.Pattern != "" && c.Pattern != from.Pattern {
		return true
	}
	if c.Min != nil && (from.Min == nil || *c.Min > *from.Min) {
		return true
	}
	if c.Max != nil && (from.Max == nil || *c.Max < *from.Max) {
		return true
	}
	return false
}

// UniqueValue renders a scalar the way entry_unique_values stores it: the
// jsonb `->>` text form, so a number written as 10 and one written as 10.0
// collide the way Postgres says they do. Returns "" for a value that reserves
// nothing — absent, null, the empty string, or a shape the type does not have.
func UniqueValue(t string, v any) string {
	switch t {
	case FieldTypeString, FieldTypeDate, FieldTypeDateTime:
		s, _ := v.(string)
		return s
	case FieldTypeNumber:
		f, ok := v.(float64)
		if !ok {
			return ""
		}
		return formatJSONNumber(f)
	}
	return ""
}

// formatJSONNumber matches jsonb's canonical numeric rendering closely enough
// for the in-memory repository: integers without a fraction, everything else
// shortest round-trip.
func formatJSONNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

var patternCache sync.Map // pattern source → *regexp.Regexp

// compilePattern anchors and compiles a pattern, caching the result: every
// write to a constrained field would otherwise recompile it.
func compilePattern(p string) (*regexp.Regexp, error) {
	if re, ok := patternCache.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(`^(?:` + p + `)$`)
	if err != nil {
		return nil, err
	}
	patternCache.Store(p, re)
	return re, nil
}
