package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func f64(v float64) *float64 { return &v }

func TestValidateFieldConstraints_RefusesByName(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		multiple bool
		c        FieldConstraints
		code     string
		attr     string
	}{
		{"unique on text", FieldTypeText, false, FieldConstraints{Unique: true}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", "unique"},
		{"unique on a list", FieldTypeString, true, FieldConstraints{Unique: true}, "CONTENT_FIELD_UNIQUE_MULTIPLE", "unique"},
		{"format on number", FieldTypeNumber, false, FieldConstraints{Format: FieldFormatSlug}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", "format"},
		{"unknown format", FieldTypeString, false, FieldConstraints{Format: "email"}, "CONTENT_FIELD_FORMAT_UNKNOWN", "format"},
		{"pattern on number", FieldTypeNumber, false, FieldConstraints{Pattern: "x"}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", "pattern"},
		{"pattern that does not compile", FieldTypeString, false, FieldConstraints{Pattern: "("}, "CONTENT_FIELD_PATTERN_INVALID", "pattern"},
		{"range on boolean", FieldTypeBoolean, false, FieldConstraints{Max: f64(1)}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", "max"},
		{"negative length", FieldTypeString, false, FieldConstraints{Min: f64(-1)}, "CONTENT_FIELD_RANGE_INVALID", "min"},
		{"fractional length", FieldTypeText, false, FieldConstraints{Max: f64(2.5)}, "CONTENT_FIELD_RANGE_INVALID", "max"},
		{"inverted range", FieldTypeNumber, false, FieldConstraints{Min: f64(10), Max: f64(1)}, "CONTENT_FIELD_RANGE_INVALID", "min"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFieldConstraints(tc.typ, tc.multiple, tc.c)
			require.NotNil(t, err)
			assert.Equal(t, tc.code, err.Code)
			assert.Equal(t, tc.attr, err.Attr)
		})
	}

	t.Run("the legal combinations", func(t *testing.T) {
		assert.Nil(t, ValidateFieldConstraints(FieldTypeString, false, FieldConstraints{Unique: true, Format: FieldFormatSlug, Min: f64(1), Max: f64(80)}))
		assert.Nil(t, ValidateFieldConstraints(FieldTypeString, true, FieldConstraints{Pattern: `[a-z]+`, Max: f64(12)}))
		assert.Nil(t, ValidateFieldConstraints(FieldTypeText, false, FieldConstraints{Pattern: `(?s).*`, Min: f64(0)}))
		assert.Nil(t, ValidateFieldConstraints(FieldTypeNumber, false, FieldConstraints{Unique: true, Min: f64(-1.5), Max: f64(2.5)}))
		assert.Nil(t, ValidateFieldConstraints(FieldTypeDate, false, FieldConstraints{Unique: true}))
		assert.Nil(t, ValidateFieldConstraints(FieldTypeRelation, true, FieldConstraints{}))
	})
}

func TestFieldConstraints_CheckValue(t *testing.T) {
	slug := FieldConstraints{Format: FieldFormatSlug}
	assert.Nil(t, slug.CheckValue(FieldTypeString, "hello-world-2"))
	for _, bad := range []string{"Hello", "hello world", "-hello", "hello-", "a--b", "", "héllo"} {
		v := slug.CheckValue(FieldTypeString, bad)
		require.NotNil(t, v, "%q should not be a slug", bad)
		assert.Equal(t, "format", v.Attr)
		assert.Equal(t, "slug", v.Expected)
	}

	// The pattern is anchored: a match somewhere inside is not a match.
	pat := FieldConstraints{Pattern: `[A-Z]{3}-\d+`}
	assert.Nil(t, pat.CheckValue(FieldTypeString, "ABC-12"))
	v := pat.CheckValue(FieldTypeString, "xABC-12x")
	require.NotNil(t, v)
	assert.Equal(t, "pattern", v.Attr)
	assert.Equal(t, `[A-Z]{3}-\d+`, v.Expected)

	// Lengths are counted in characters, not bytes.
	length := FieldConstraints{Min: f64(2), Max: f64(3)}
	assert.Nil(t, length.CheckValue(FieldTypeText, "日本語"))
	assert.Equal(t, "max", length.CheckValue(FieldTypeText, "日本語文").Attr)
	assert.Equal(t, "min", length.CheckValue(FieldTypeString, "a").Attr)
	assert.Equal(t, "3 characters", length.CheckValue(FieldTypeText, "日本語文").Expected)

	num := FieldConstraints{Min: f64(0), Max: f64(9.5)}
	assert.Nil(t, num.CheckValue(FieldTypeNumber, 9.5))
	assert.Equal(t, "max", num.CheckValue(FieldTypeNumber, 9.6).Attr)
	assert.Equal(t, "min", num.CheckValue(FieldTypeNumber, -0.1).Attr)
	assert.Equal(t, "9.5", num.CheckValue(FieldTypeNumber, 10.0).Expected)

	// The wrong shape is the validator's problem, not this rule's.
	assert.Nil(t, num.CheckValue(FieldTypeNumber, "ten"))
	assert.Nil(t, slug.CheckValue(FieldTypeString, 3.0))
	// Types that carry no rules pass whatever they hold.
	assert.Nil(t, FieldConstraints{Min: f64(1)}.CheckValue(FieldTypeBoolean, true))
}

func TestFieldConstraints_Tightens(t *testing.T) {
	none := FieldConstraints{}
	assert.True(t, FieldConstraints{Format: FieldFormatSlug}.Tightens(none))
	assert.True(t, FieldConstraints{Pattern: "a"}.Tightens(FieldConstraints{Pattern: "b"}), "two patterns are not comparable; ask the data")
	assert.False(t, none.Tightens(FieldConstraints{Pattern: "b"}), "dropping a pattern relaxes")
	assert.True(t, FieldConstraints{Min: f64(2)}.Tightens(FieldConstraints{Min: f64(1)}))
	assert.False(t, FieldConstraints{Min: f64(1)}.Tightens(FieldConstraints{Min: f64(2)}))
	assert.True(t, FieldConstraints{Max: f64(1)}.Tightens(FieldConstraints{Max: f64(2)}))
	assert.False(t, FieldConstraints{Max: f64(3)}.Tightens(FieldConstraints{Max: f64(2)}))
	assert.False(t, none.Tightens(FieldConstraints{Min: f64(1), Max: f64(2)}))
	// unique is graded on its own line and is not a "tightening" here.
	assert.False(t, FieldConstraints{Unique: true}.Tightens(none))
}

func TestUniqueValue_RendersLikeJSONB(t *testing.T) {
	assert.Equal(t, "10", UniqueValue(FieldTypeNumber, 10.0))
	assert.Equal(t, "10.5", UniqueValue(FieldTypeNumber, 10.5))
	assert.Equal(t, "x", UniqueValue(FieldTypeString, "x"))
	assert.Equal(t, "", UniqueValue(FieldTypeString, nil))
	assert.Equal(t, "", UniqueValue(FieldTypeText, "held by no ledger"))
}

// The diff grades constraint changes the way it grades `required`: a
// tightening is guarded by the code the apply would fail with, carrying the
// target set so the planner can ask the database; a relaxation is additive; a
// set that could not be stored is refused by the same code buildField uses.
func TestDiffSchemas_GradesConstraintChanges(t *testing.T) {
	art := func(c FieldConstraints) Artifact {
		return Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema, Types: []ArtifactType{{
			Name: "post", Fields: []ArtifactField{{Key: "slug", Type: FieldTypeString, FieldConstraints: c}},
		}}}
	}
	only := func(t *testing.T, changes []SchemaChange, n int) []SchemaChange {
		t.Helper()
		require.Len(t, changes, n, "%+v", changes)
		return changes
	}

	t.Run("switching unique on is guarded by the duplicate check", func(t *testing.T) {
		c := only(t, DiffSchemas(art(FieldConstraints{}), art(FieldConstraints{Unique: true})), 1)[0]
		assert.Equal(t, GradeGuarded, c.Grade)
		assert.Equal(t, "CONTENT_FIELD_UNIQUE_DUPLICATES", c.Code)
		require.NotNil(t, c.Constraints)
		assert.True(t, c.Constraints.Unique)
	})
	t.Run("switching unique off is additive", func(t *testing.T) {
		c := only(t, DiffSchemas(art(FieldConstraints{Unique: true}), art(FieldConstraints{})), 1)[0]
		assert.Equal(t, GradeAdditive, c.Grade)
		assert.Empty(t, c.Code)
	})
	t.Run("adding a format is guarded by the backfill count", func(t *testing.T) {
		c := only(t, DiffSchemas(art(FieldConstraints{}), art(FieldConstraints{Format: FieldFormatSlug, Max: f64(80)})), 1)[0]
		assert.Equal(t, GradeGuarded, c.Grade)
		assert.Equal(t, "CONTENT_FIELD_CONSTRAINT_BACKFILL", c.Code)
		require.NotNil(t, c.Constraints)
		assert.Equal(t, FieldFormatSlug, c.Constraints.Format)
		assert.Contains(t, c.Detail, "none → format=slug, max=80")
	})
	t.Run("raising max is additive", func(t *testing.T) {
		c := only(t, DiffSchemas(art(FieldConstraints{Max: f64(10)}), art(FieldConstraints{Max: f64(20)})), 1)[0]
		assert.Equal(t, GradeAdditive, c.Grade)
		assert.Contains(t, c.Detail, "(relaxed)")
	})
	t.Run("unique and a rule change are two lines", func(t *testing.T) {
		cs := only(t, DiffSchemas(art(FieldConstraints{}), art(FieldConstraints{Unique: true, Pattern: "x"})), 2)
		assert.Equal(t, "CONTENT_FIELD_UNIQUE_DUPLICATES", cs[0].Code)
		assert.Equal(t, "CONTENT_FIELD_CONSTRAINT_BACKFILL", cs[1].Code)
	})
	t.Run("an unstorable set is refused, not planned", func(t *testing.T) {
		c := only(t, DiffSchemas(art(FieldConstraints{}), art(FieldConstraints{Pattern: "("})), 1)[0]
		assert.Equal(t, GradeRefused, c.Grade)
		assert.Equal(t, "CONTENT_FIELD_PATTERN_INVALID", c.Code)
	})
	t.Run("no change, no line", func(t *testing.T) {
		only(t, DiffSchemas(art(FieldConstraints{Min: f64(1)}), art(FieldConstraints{Min: f64(1)})), 0)
	})
	t.Run("a new field with a bad set is refused by the same code", func(t *testing.T) {
		empty := Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema, Types: []ArtifactType{{Name: "post"}}}
		c := only(t, DiffSchemas(empty, art(FieldConstraints{Unique: true, Format: "email"})), 1)[0]
		assert.Equal(t, OpAddField, c.Op)
		assert.Equal(t, GradeRefused, c.Grade)
		assert.Equal(t, "CONTENT_FIELD_FORMAT_UNKNOWN", c.Code)
		ok := only(t, DiffSchemas(empty, art(FieldConstraints{Unique: true, Format: FieldFormatSlug})), 1)[0]
		assert.Equal(t, GradeAdditive, ok.Grade)
		assert.Contains(t, ok.Detail, "(unique, format=slug)")
	})
}
