package domain

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seoComponent() *Component {
	id := uuid.New()
	return &Component{
		ID: id, TenantID: "t", Name: "seo", Label: "SEO",
		Fields: []Field{
			{ID: uuid.New(), Key: "title", Type: FieldTypeString, Required: true, FieldConstraints: FieldConstraints{Max: ptrF(60)}},
			{ID: uuid.New(), Key: "noindex", Type: FieldTypeBoolean},
		},
	}
}

func ptrF(v float64) *float64 { return &v }

func pageType(seo *Component) *ContentType {
	f := Field{Key: "meta", Type: FieldTypeComponent, Multiple: true, ComponentID: &seo.ID, ComponentName: seo.Name}
	return ct("page", Field{Key: "title", Type: FieldTypeString}, f)
}

// ADR-008 Amendment 2: components come BEFORE types on the wire, a component
// field names its component, and the whole thing survives a round trip.
func TestSchemaArtifact_ComponentsPrecedeTypesAndRoundTrip(t *testing.T) {
	seo := seoComponent()
	art := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})

	b, err := MarshalArtifact(art)
	require.NoError(t, err)
	assert.Less(t, bytes.Index(b, []byte(`"components"`)), bytes.Index(b, []byte(`"types"`)),
		"components must be written before the types that embed them")
	assert.Contains(t, string(b), `"component": "seo"`)

	var back Artifact
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, art, back)
	require.Len(t, back.Components, 1)
	assert.Equal(t, []string{"title", "noindex"}, []string{back.Components[0].Fields[0].Key, back.Components[0].Fields[1].Key},
		"sub-field order is part of the definition and is preserved, not sorted")
}

// A tenant without components exports exactly what it exported before this
// feature: no key, no null, not even an empty list.
func TestSchemaArtifact_NoComponentsIsByteIdentical(t *testing.T) {
	types := []*ContentType{ct("post", Field{Key: "title", Type: FieldTypeString})}
	before, err := MarshalArtifact(NewArtifact(types))
	require.NoError(t, err)
	after, err := MarshalArtifact(NewSchemaArtifact(types, nil))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.NotContains(t, string(after), "components")
}

func TestDiffSchemas_ComponentOpsAreOrderedAroundTheTypesThatEmbedThem(t *testing.T) {
	seo := seoComponent()
	full := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})
	empty := NewArtifact(nil)

	t.Run("creating: component first, then the type that embeds it", func(t *testing.T) {
		got := DiffSchemas(empty, full)
		require.Len(t, got, 2)
		assert.Equal(t, OpCreateComponent, got[0].Op)
		assert.Equal(t, "seo", got[0].Component)
		assert.Equal(t, GradeAdditive, got[0].Grade)
		assert.Equal(t, OpCreateType, got[1].Op)
		assert.Equal(t, "page", got[1].Type)
	})
	t.Run("removing: the type first, the component last and guarded", func(t *testing.T) {
		got := DiffSchemas(full, empty)
		require.Len(t, got, 2)
		assert.Equal(t, OpDeleteType, got[0].Op)
		assert.Equal(t, OpDeleteComponent, got[1].Op)
		assert.Equal(t, "seo", got[1].Component)
		assert.Equal(t, GradeGuarded, got[1].Grade)
		assert.Equal(t, "CONTENT_COMPONENT_IN_USE", got[1].Code)
	})
	t.Run("a new field on a kept type comes after a new sub-field on its component", func(t *testing.T) {
		next := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})
		next.Components[0].Fields = append(next.Components[0].Fields, ArtifactField{Key: "canonical", Type: FieldTypeString})
		next.Types[0].Fields = append(next.Types[0].Fields, ArtifactField{Key: "slug", Type: FieldTypeString})
		got := DiffSchemas(full, next)
		require.Len(t, got, 2)
		assert.Equal(t, OpAddComponentField, got[0].Op)
		assert.Equal(t, OpAddField, got[1].Op)
	})
}

// A type field of type component must name a component THE ARTIFACT declares:
// an artifact is a whole state, and a reference into something it does not
// carry is one the apply could not honour in order.
func TestDiffSchemas_ComponentFieldReferencesMustBeDeclared(t *testing.T) {
	empty := NewArtifact(nil)
	for _, tc := range []struct {
		name string
		f    ArtifactField
		code string
	}{
		{"undeclared component", ArtifactField{Key: "meta", Type: FieldTypeComponent, Component: "ghost"}, "CONTENT_COMPONENT_NOT_FOUND"},
		{"no component named", ArtifactField{Key: "meta", Type: FieldTypeComponent}, "CONTENT_COMPONENT_REQUIRED"},
		{"component on a string field", ArtifactField{Key: "meta", Type: FieldTypeString, Component: "seo"}, "CONTENT_COMPONENT_NOT_APPLICABLE"},
		{"unique on a component field", ArtifactField{Key: "meta", Type: FieldTypeComponent, Component: "seo", FieldConstraints: FieldConstraints{Unique: true}}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE"},
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
			assert.Equal(t, "meta", refused[0].Field)
		})
	}
	t.Run("a declared component is additive", func(t *testing.T) {
		seo := seoComponent()
		got := DiffSchemas(empty, NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo}))
		for _, c := range got {
			assert.NotEqual(t, GradeRefused, c.Grade, c.Detail)
		}
	})
}

func TestDiffSchemas_ComponentDefinitionRules(t *testing.T) {
	empty := NewArtifact(nil)
	diff := func(c ArtifactComponent) []SchemaChange {
		return DiffSchemas(empty, Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema, Components: []ArtifactComponent{c}})
	}
	codes := func(cs []SchemaChange) []string {
		var out []string
		for _, c := range cs {
			if c.Grade == GradeRefused {
				out = append(out, c.Code)
			}
		}
		return out
	}
	assert.Equal(t, []string{"CONTENT_COMPONENT_NESTING_UNSUPPORTED"},
		codes(diff(ArtifactComponent{Name: "seo", Fields: []ArtifactField{{Key: "inner", Type: FieldTypeComponent, Component: "x"}}})))
	assert.Equal(t, []string{"CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED"},
		codes(diff(ArtifactComponent{Name: "seo", Fields: []ArtifactField{{Key: "k", Type: FieldTypeString, ReadRoles: []string{"admin"}}}})))
	assert.Equal(t, []string{"CONTENT_COMPONENT_NO_FIELDS"},
		codes(diff(ArtifactComponent{Name: "seo"})))
	assert.Equal(t, []string{"CONTENT_COMPONENT_NAME_INVALID"},
		codes(diff(ArtifactComponent{Name: "Bad-Name", Fields: []ArtifactField{{Key: "k", Type: FieldTypeString}}})))
}

func TestDiffSchemas_ComponentSubFieldChangesCarryTheTypeGuards(t *testing.T) {
	seo := seoComponent()
	base := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})
	mutate := func(fn func(c *ArtifactComponent)) Artifact {
		next := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})
		fn(&next.Components[0])
		return next
	}
	only := func(cs []SchemaChange) SchemaChange {
		require.Len(t, cs, 1)
		return cs[0]
	}

	t.Run("label is additive", func(t *testing.T) {
		c := only(DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Label = "Search" })))
		assert.Equal(t, OpUpdateComponent, c.Op)
		assert.Equal(t, GradeAdditive, c.Grade)
	})
	t.Run("required tightened is guarded", func(t *testing.T) {
		c := only(DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Fields[1].Required = true })))
		assert.Equal(t, OpUpdateComponentField, c.Op)
		assert.Equal(t, "seo", c.Component)
		assert.Equal(t, "noindex", c.Field)
		assert.Equal(t, "CONTENT_FIELD_REQUIRED_BACKFILL", c.Code)
	})
	t.Run("constraint tightened is guarded with the target constraints", func(t *testing.T) {
		c := only(DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Fields[0].Max = ptrF(30) })))
		assert.Equal(t, "CONTENT_FIELD_CONSTRAINT_BACKFILL", c.Code)
		require.NotNil(t, c.Constraints)
		assert.Equal(t, 30.0, *c.Constraints.Max)
	})
	t.Run("type change is refused", func(t *testing.T) {
		c := only(DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Fields[1].Type = FieldTypeString })))
		assert.Equal(t, GradeRefused, c.Grade)
		assert.Equal(t, "CONTENT_FIELD_TYPE_IMMUTABLE", c.Code)
	})
	t.Run("acquiring unique is refused as the sub-field rule, not the type guard", func(t *testing.T) {
		c := only(DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Fields[0].Unique = true })))
		assert.Equal(t, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", c.Code)
	})
	t.Run("a dropped sub-field is guarded and prune-only", func(t *testing.T) {
		c := only(DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Fields = c.Fields[:1] })))
		assert.Equal(t, OpDeleteComponentField, c.Op)
		assert.Equal(t, "CONTENT_FIELD_HAS_DATA", c.Code)
		assert.Equal(t, GradeGuarded, c.Grade)
	})
	t.Run("add plus drop on one component gets the rename hint", func(t *testing.T) {
		got := DiffSchemas(base, mutate(func(c *ArtifactComponent) { c.Fields[1].Key = "no_index" }))
		require.Len(t, got, 2)
		for _, c := range got {
			assert.Contains(t, c.Hint, "RENAME")
		}
	})
	t.Run("a type field changing which component it embeds is refused", func(t *testing.T) {
		next := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})
		next.Components = append(next.Components, ArtifactComponent{Name: "og", Fields: []ArtifactField{{Key: "k", Type: FieldTypeString}}})
		next.Types[0].Fields[1].Component = "og"
		var refused []SchemaChange
		for _, c := range DiffSchemas(base, next) {
			if c.Grade == GradeRefused {
				refused = append(refused, c)
			}
		}
		require.Len(t, refused, 1)
		assert.Equal(t, "CONTENT_FIELD_COMPONENT_IMMUTABLE", refused[0].Code)
	})
}

// An artifact written before components existed still reads and plans: no
// components key means no components, which against a tenant that has some is
// a guarded, prune-only deletion — never an error and never a silent drop.
func TestDiffSchemas_OldArtifactWithoutComponentsKey(t *testing.T) {
	seo := seoComponent()
	live := NewSchemaArtifact([]*ContentType{pageType(seo)}, []*Component{seo})

	var old Artifact
	require.NoError(t, json.Unmarshal([]byte(`{"artifact_version":"1","kind":"content-schema","types":[{"name":"page","label":"page label","fields":[{"key":"title","type":"string","required":false,"multiple":false}]}]}`), &old))
	assert.Nil(t, old.Components)

	got := DiffSchemas(live, old)
	require.Len(t, got, 2)
	assert.Equal(t, OpDeleteField, got[0].Op)
	assert.Equal(t, "meta", got[0].Field)
	assert.Equal(t, OpDeleteComponent, got[1].Op)
	for _, c := range got {
		assert.Equal(t, GradeGuarded, c.Grade)
	}
}
