package domain

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The artifact half of ADR-020 Amendment 1 (cross-cutting item 6): a zone's
// allowed-list travels as EntityField.components, survives the round trip in
// the author's order, and the diff grades a change to it exactly as it grades
// an enum narrowing — additions and reorders free, removals guarded.

func zoneComponents() []*Component {
	return []*Component{
		{ID: uuid.New(), TenantID: "t", Name: "cta", Label: "CTA",
			Fields: []Field{{ID: uuid.New(), Key: "label", Type: FieldTypeString}}},
		{ID: uuid.New(), TenantID: "t", Name: "hero", Label: "Hero",
			Fields: []Field{{ID: uuid.New(), Key: "headline", Type: FieldTypeString}}},
	}
}

func zonePageType(names ...string) *ContentType {
	f := Field{Key: "zone", Type: FieldTypeDynamicZone, ZoneComponents: names}
	return ct("page", Field{Key: "title", Type: FieldTypeString}, f)
}

func zoneArtifact(names ...string) Artifact {
	return NewSchemaArtifact([]*ContentType{zonePageType(names...)}, zoneComponents())
}

func TestSchemaArtifact_ZoneAllowedListRoundTrips(t *testing.T) {
	// Deliberately NOT alphabetical: the list is the order an editor's block
	// menu offers, so it must come back exactly as written.
	art := zoneArtifact("hero", "cta")

	b, err := MarshalArtifact(art)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"components": [`)
	assert.Less(t, bytes.Index(b, []byte(`"hero"`)), bytes.LastIndex(b, []byte(`"cta"`)),
		"the allowed-list keeps the author's order, it is not sorted")

	var back Artifact
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, art, back)
	assert.Equal(t, []string{"hero", "cta"}, back.Types[0].Fields[1].Components)

	// And a schema with no zone in it exports exactly what it exported before
	// zones existed: no key, no null, not even an empty list.
	plain, err := MarshalArtifact(NewArtifact([]*ContentType{ct("post", Field{Key: "title", Type: FieldTypeString})}))
	require.NoError(t, err)
	assert.NotContains(t, string(plain), "components")
}

func TestDiffSchemas_ZoneAllowedListMustBeDeclared(t *testing.T) {
	empty := NewArtifact(nil)
	for _, tc := range []struct {
		name string
		f    ArtifactField
		code string
	}{
		{"no components named", ArtifactField{Key: "zone", Type: FieldTypeDynamicZone}, "CONTENT_ZONE_COMPONENTS_REQUIRED"},
		{"an undeclared component", ArtifactField{Key: "zone", Type: FieldTypeDynamicZone, Components: []string{"ghost"}}, "CONTENT_COMPONENT_NOT_FOUND"},
		{"the same component twice", ArtifactField{Key: "zone", Type: FieldTypeDynamicZone, Components: []string{"seo", "seo"}}, "CONTENT_ZONE_COMPONENT_DUPLICATE"},
		{"a list on a string field", ArtifactField{Key: "zone", Type: FieldTypeString, Components: []string{"seo"}}, "CONTENT_ZONE_COMPONENTS_NOT_APPLICABLE"},
		{"multiple on a zone", ArtifactField{Key: "zone", Type: FieldTypeDynamicZone, Components: []string{"seo"}, Multiple: true}, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED"},
		{"unique on a zone", ArtifactField{Key: "zone", Type: FieldTypeDynamicZone, Components: []string{"seo"},
			FieldConstraints: FieldConstraints{Unique: true}}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			to := Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema,
				Components: []ArtifactComponent{{Name: "seo", Fields: []ArtifactField{{Key: "title", Type: FieldTypeString}}}},
				Types:      []ArtifactType{{Name: "page", Fields: []ArtifactField{tc.f}}}}
			var refused []SchemaChange
			for _, c := range DiffSchemas(empty, to) {
				if c.Grade == GradeRefused {
					refused = append(refused, c)
				}
			}
			require.Len(t, refused, 1)
			assert.Equal(t, tc.code, refused[0].Code)
			assert.Equal(t, "page", refused[0].Type)
			assert.Equal(t, "zone", refused[0].Field)
		})
	}

	t.Run("a zone naming declared components is additive, component ops first", func(t *testing.T) {
		got := DiffSchemas(empty, zoneArtifact("hero", "cta"))
		require.Len(t, got, 3)
		assert.Equal(t, OpCreateComponent, got[0].Op)
		assert.Equal(t, OpCreateComponent, got[1].Op)
		assert.Equal(t, OpCreateType, got[2].Op)
		for _, c := range got {
			assert.NotEqual(t, GradeRefused, c.Grade, c.Detail)
		}
	})
}

// The grading table for an allowed-list change. It mirrors the enum one on
// purpose: only removals can strand stored data, so only removals are guarded.
func TestDiffSchemas_ZoneAllowedListChanges(t *testing.T) {
	base := zoneArtifact("hero", "cta")

	t.Run("adding a component is additive", func(t *testing.T) {
		next := zoneArtifact("hero", "cta", "hero2")
		next.Components = append(next.Components, ArtifactComponent{
			Name: "hero2", Fields: []ArtifactField{{Key: "headline", Type: FieldTypeString}}})
		got := DiffSchemas(base, next)
		require.Len(t, got, 2)
		assert.Equal(t, OpCreateComponent, got[0].Op)
		assert.Equal(t, OpUpdateField, got[1].Op)
		assert.Equal(t, GradeAdditive, got[1].Grade)
		assert.Empty(t, got[1].RemovedComponents)
	})

	t.Run("reordering is additive but is reported", func(t *testing.T) {
		got := DiffSchemas(base, zoneArtifact("cta", "hero"))
		require.Len(t, got, 1)
		assert.Equal(t, OpUpdateField, got[0].Op)
		assert.Equal(t, GradeAdditive, got[0].Grade)
		assert.Contains(t, got[0].Detail, "reordered")
	})

	t.Run("removing a component is guarded and names what it removes", func(t *testing.T) {
		got := DiffSchemas(base, zoneArtifact("hero"))
		require.Len(t, got, 1)
		assert.Equal(t, OpUpdateField, got[0].Op)
		assert.Equal(t, GradeGuarded, got[0].Grade)
		assert.Equal(t, "CONTENT_ZONE_COMPONENT_IN_USE", got[0].Code)
		assert.Equal(t, []string{"cta"}, got[0].RemovedComponents,
			"the names travel structured: the planner asks the database about them one at a time")
	})

	t.Run("a removal alongside an addition is still guarded", func(t *testing.T) {
		next := zoneArtifact("hero", "seo")
		next.Components = append(next.Components, ArtifactComponent{
			Name: "seo", Fields: []ArtifactField{{Key: "title", Type: FieldTypeString}}})
		got := DiffSchemas(base, next)
		var upd []SchemaChange
		for _, c := range got {
			if c.Op == OpUpdateField {
				upd = append(upd, c)
			}
		}
		require.Len(t, upd, 1, "one change carries the whole allowed-list move")
		assert.Equal(t, GradeGuarded, upd[0].Grade)
		assert.Equal(t, []string{"cta"}, upd[0].RemovedComponents)
	})

	t.Run("no change at all", func(t *testing.T) {
		assert.Empty(t, DiffSchemas(base, zoneArtifact("hero", "cta")))
	})
}
