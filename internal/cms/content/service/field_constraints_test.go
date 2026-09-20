package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

func fnum(v float64) *float64 { return &v }

// postTypeInput carries one field per constraint attribute so each rule is
// exercised on its own and a failure names the attribute, not the fixture.
func postTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:  "post",
		Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "slug", Type: domain.FieldTypeString, Required: true,
				FieldConstraints: domain.FieldConstraints{Unique: true, Format: domain.FieldFormatSlug, Max: fnum(40)}},
			{Key: "sku", Type: domain.FieldTypeString,
				FieldConstraints: domain.FieldConstraints{Pattern: `[A-Z]{3}-\d+`}},
			{Key: "price", Type: domain.FieldTypeNumber,
				FieldConstraints: domain.FieldConstraints{Min: fnum(0), Max: fnum(100)}},
			{Key: "tags", Type: domain.FieldTypeString, Multiple: true,
				FieldConstraints: domain.FieldConstraints{Pattern: `[a-z]+`}},
			{Key: "summary", Type: domain.FieldTypeText,
				FieldConstraints: domain.FieldConstraints{Min: fnum(10)}},
		},
	}
}

func seedConstrainedPostType(t *testing.T) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, postTypeInput())
	require.NoError(t, err)
	return svc, repo, ctx
}

func TestCreateContentType_RefusesConstraintDefinitionsByCode(t *testing.T) {
	cases := []struct {
		name  string
		field FieldInput
		code  string
	}{
		{"unique on text", FieldInput{Key: "f", Type: domain.FieldTypeText, FieldConstraints: domain.FieldConstraints{Unique: true}}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE"},
		{"unique on a list", FieldInput{Key: "f", Type: domain.FieldTypeString, Multiple: true, FieldConstraints: domain.FieldConstraints{Unique: true}}, "CONTENT_FIELD_UNIQUE_MULTIPLE"},
		{"unknown format", FieldInput{Key: "f", Type: domain.FieldTypeString, FieldConstraints: domain.FieldConstraints{Format: "email"}}, "CONTENT_FIELD_FORMAT_UNKNOWN"},
		{"pattern that does not compile", FieldInput{Key: "f", Type: domain.FieldTypeString, FieldConstraints: domain.FieldConstraints{Pattern: "("}}, "CONTENT_FIELD_PATTERN_INVALID"},
		{"pattern on number", FieldInput{Key: "f", Type: domain.FieldTypeNumber, FieldConstraints: domain.FieldConstraints{Pattern: "x"}}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE"},
		{"min above max", FieldInput{Key: "f", Type: domain.FieldTypeNumber, FieldConstraints: domain.FieldConstraints{Min: fnum(5), Max: fnum(1)}}, "CONTENT_FIELD_RANGE_INVALID"},
		{"fractional length", FieldInput{Key: "f", Type: domain.FieldTypeString, FieldConstraints: domain.FieldConstraints{Max: fnum(1.5)}}, "CONTENT_FIELD_RANGE_INVALID"},
		{"range on relation", FieldInput{Key: "f", Type: domain.FieldTypeRelation, RelationEntity: "post", FieldConstraints: domain.FieldConstraints{Min: fnum(1)}}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSvc()
			_, err := svc.CreateContentType(ctxTenant("t1"), CreateTypeInput{Name: "x", Label: "X", Fields: []FieldInput{tc.field}})
			ae := mustCode(t, err, tc.code, 422)
			assert.Equal(t, "f", ae.Details["field"])
		})
	}

	t.Run("format and pattern are trimmed before they are judged", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		_, err := svc.CreateContentType(ctx, CreateTypeInput{Name: "x", Label: "X", Fields: []FieldInput{
			{Key: "f", Type: domain.FieldTypeString, FieldConstraints: domain.FieldConstraints{Format: " slug ", Pattern: " [a-z]+ "}},
		}})
		require.NoError(t, err)
		dto, err := svc.GetContentType(ctx, "x")
		require.NoError(t, err)
		assert.Equal(t, domain.FieldFormatSlug, dto.Fields[0].Format)
		assert.Equal(t, "[a-z]+", dto.Fields[0].Pattern)
	})
}

func TestContentTypeDTO_CarriesConstraints(t *testing.T) {
	svc, _, ctx := seedConstrainedPostType(t)
	dto, err := svc.GetContentType(ctx, "post")
	require.NoError(t, err)
	byKey := map[string]FieldDTO{}
	for _, f := range dto.Fields {
		byKey[f.Key] = f
	}
	assert.True(t, byKey["slug"].Unique)
	assert.Equal(t, domain.FieldFormatSlug, byKey["slug"].Format)
	assert.Equal(t, 40.0, *byKey["slug"].Max)
	assert.Nil(t, byKey["slug"].Min)
	assert.Equal(t, `[A-Z]{3}-\d+`, byKey["sku"].Pattern)
	assert.Equal(t, 0.0, *byKey["price"].Min)
	assert.False(t, byKey["title"].Unique)
	assert.Empty(t, byKey["title"].Format)

	// `unique` is always on the wire, so a client never has to guess whether
	// absence means false or means "this server predates the attribute".
	raw, err := json.Marshal(byKey["title"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"unique":false`)
	assert.NotContains(t, string(raw), `"format"`)

	// And the export carries them under the same keys, so an apply of an
	// export is a no-op (ADR-008).
	art, err := svc.ExportSchema(ctx)
	require.NoError(t, err)
	var slug domain.ArtifactField
	for _, f := range art.Types[0].Fields {
		if f.Key == "slug" {
			slug = f
		}
	}
	assert.True(t, slug.Unique)
	assert.Equal(t, domain.FieldFormatSlug, slug.Format)
	plan, err := svc.PlanSchema(ctx, art, false)
	require.NoError(t, err)
	assert.Empty(t, plan.Steps, "re-applying the export must plan nothing: %+v", plan.Steps)
}

func TestCreateEntry_EnforcesConstraintsAtWriteTime(t *testing.T) {
	svc, _, ctx := seedConstrainedPostType(t)
	ok := map[string]any{"title": "Hello", "slug": "hello-world", "sku": "ABC-12", "price": 99.5,
		"tags": []string{"go", "cms"}, "summary": "long enough text"}
	_, err := svc.CreateEntry(ctx, "post", mustJSON(t, ok))
	require.NoError(t, err)

	cases := []struct {
		name       string
		key        string
		value      any
		constraint string
		expected   string
	}{
		{"slug with a space", "slug", "Hello World", "format", "slug"},
		{"slug over max length", "slug", "a-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "max", "40 characters"},
		{"sku off pattern", "sku", "abc-12", "pattern", `[A-Z]{3}-\d+`},
		{"price below min", "price", -1, "min", "0"},
		{"price above max", "price", 100.5, "max", "100"},
		{"one bad tag in a list", "tags", []string{"go", "C++"}, "pattern", "[a-z]+"},
		{"summary too short", "summary", "short", "min", "10 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{}
			for k, v := range ok {
				payload[k] = v
			}
			payload["slug"] = "other-slug"
			payload[tc.key] = tc.value
			_, err := svc.CreateEntry(ctx, "post", mustJSON(t, payload))
			ae := mustCode(t, err, "CONTENT_FIELD_CONSTRAINT_VIOLATED", 422)
			assert.Equal(t, tc.key, ae.Details["field"])
			assert.Equal(t, tc.constraint, ae.Details["constraint"])
			assert.Equal(t, tc.expected, ae.Details["expected"])
		})
	}

	t.Run("an omitted optional field is not judged", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "slug": "only-required"}))
		require.NoError(t, err)
	})
	t.Run("the update path runs the same rules", func(t *testing.T) {
		e, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "slug": "to-edit"}))
		require.NoError(t, err)
		_, err = svc.UpdateEntry(ctx, "post", e.ID, mustJSON(t, map[string]any{"title": "T", "slug": "Not A Slug"}), e.Version)
		mustCode(t, err, "CONTENT_FIELD_CONSTRAINT_VIOLATED", 422)
	})
}

func TestUpdateField_ConstraintGuards(t *testing.T) {
	svc, _, ctx := seedConstrainedPostType(t)
	for _, slug := range []string{"alpha", "beta-two", "gamma-three-words"} {
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "Title", "slug": slug, "sku": "ABC-1"}))
		require.NoError(t, err)
	}
	field := func(t *testing.T, key string) FieldDTO {
		t.Helper()
		dto, err := svc.GetContentType(ctx, "post")
		require.NoError(t, err)
		for _, f := range dto.Fields {
			if f.Key == key {
				return f
			}
		}
		t.Fatalf("field %s not found", key)
		return FieldDTO{}
	}

	t.Run("tightening against stored data is a 409 that counts the entries", func(t *testing.T) {
		_, err := svc.UpdateField(ctx, "post", "slug", UpdateFieldInput{Max: OptionalNumber{Set: true, Value: fnum(6)}})
		ae := mustCode(t, err, "CONTENT_FIELD_CONSTRAINT_BACKFILL", 409)
		assert.Equal(t, "slug", ae.Details["field"])
		assert.Equal(t, 2, ae.Details["entries"], "beta-two and gamma-three-words are over 6")
		assert.Equal(t, 40.0, *field(t, "slug").Max, "the refused change must not land")
	})
	t.Run("tightening that the data already satisfies goes through", func(t *testing.T) {
		_, err := svc.UpdateField(ctx, "post", "slug", UpdateFieldInput{Max: OptionalNumber{Set: true, Value: fnum(20)}})
		require.NoError(t, err)
		assert.Equal(t, 20.0, *field(t, "slug").Max)
	})
	t.Run("relaxing never asks the data", func(t *testing.T) {
		_, err := svc.UpdateField(ctx, "post", "slug", UpdateFieldInput{Max: OptionalNumber{Set: true, Value: fnum(200)}})
		require.NoError(t, err)
		assert.Equal(t, 200.0, *field(t, "slug").Max)
	})
	t.Run("a JSON null clears the bound; an absent key leaves it", func(t *testing.T) {
		var in UpdateFieldInput
		require.NoError(t, json.Unmarshal([]byte(`{"max": null}`), &in))
		_, err := svc.UpdateField(ctx, "post", "slug", in)
		require.NoError(t, err)
		assert.Nil(t, field(t, "slug").Max)

		var other UpdateFieldInput
		require.NoError(t, json.Unmarshal([]byte(`{"label": "Price"}`), &other))
		_, err = svc.UpdateField(ctx, "post", "price", other)
		require.NoError(t, err)
		assert.Equal(t, 100.0, *field(t, "price").Max, "an absent key is not a null")
	})
	t.Run("a pattern change is graded by the data, not by the strings", func(t *testing.T) {
		_, err := svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Pattern: ptrTo(`XYZ-\d+`)})
		ae := mustCode(t, err, "CONTENT_FIELD_CONSTRAINT_BACKFILL", 409)
		assert.Equal(t, 3, ae.Details["entries"])
		_, err = svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Pattern: ptrTo(`[A-Z]+-\d`)})
		require.NoError(t, err)
		_, err = svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Pattern: ptrTo("")})
		require.NoError(t, err)
		assert.Empty(t, field(t, "sku").Pattern)
	})
	t.Run("a definition the type cannot carry is refused before any data is read", func(t *testing.T) {
		_, err := svc.UpdateField(ctx, "post", "summary", UpdateFieldInput{Unique: ptrTo(true)})
		mustCode(t, err, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", 422)
		_, err = svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Pattern: ptrTo("(")})
		mustCode(t, err, "CONTENT_FIELD_PATTERN_INVALID", 422)
		_, err = svc.UpdateField(ctx, "post", "tags", UpdateFieldInput{Unique: ptrTo(true)})
		mustCode(t, err, "CONTENT_FIELD_UNIQUE_MULTIPLE", 422)
	})
	t.Run("unique cannot be switched on over duplicates", func(t *testing.T) {
		_, err := svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Unique: ptrTo(true)})
		ae := mustCode(t, err, "CONTENT_FIELD_UNIQUE_DUPLICATES", 409)
		assert.Equal(t, "sku", ae.Details["field"])
		dups := ae.Details["duplicates"]
		raw, _ := json.Marshal(dups)
		assert.Contains(t, string(raw), `"value":"ABC-1"`)
		assert.Contains(t, string(raw), `"entries":3`)
		assert.False(t, field(t, "sku").Unique)
	})
	t.Run("and goes on once the data is distinct, then off again freely", func(t *testing.T) {
		list, err := svc.ListEntries(ctx, "post", ListEntriesInput{Limit: 10})
		require.NoError(t, err)
		for i, e := range list.Items {
			_, err := svc.UpdateEntry(ctx, "post", e.ID, mustJSON(t, map[string]any{
				"title": "Title", "slug": "distinct-" + string(rune('a'+i)), "sku": "ABC-" + string(rune('1'+i)),
			}), e.Version)
			require.NoError(t, err)
		}
		_, err = svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Unique: ptrTo(true)})
		require.NoError(t, err)
		assert.True(t, field(t, "sku").Unique)
		_, err = svc.UpdateField(ctx, "post", "sku", UpdateFieldInput{Unique: ptrTo(false)})
		require.NoError(t, err)
		assert.False(t, field(t, "sku").Unique)
	})
}

func TestOptionalNumber_JSON(t *testing.T) {
	var in struct {
		N OptionalNumber `json:"n"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{}`), &in))
	assert.False(t, in.N.Set)
	require.NoError(t, json.Unmarshal([]byte(`{"n": null}`), &in))
	assert.True(t, in.N.Set)
	assert.Nil(t, in.N.Value)
	require.NoError(t, json.Unmarshal([]byte(`{"n": 2.5}`), &in))
	assert.True(t, in.N.Set)
	assert.Equal(t, 2.5, *in.N.Value)
	assert.Error(t, json.Unmarshal([]byte(`{"n": "2"}`), &in))

	out, err := json.Marshal(struct {
		A OptionalNumber `json:"a"`
		B OptionalNumber `json:"b"`
	}{A: OptionalNumber{Set: true, Value: fnum(3)}, B: OptionalNumber{Set: true}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":3,"b":null}`, string(out))
}

// PlanSchema resolves the guarded constraint steps against the stored data
// with the same counts the verbs' 409s carry; a blocked plan is not applied.
func TestPlanSchema_ConstraintSteps(t *testing.T) {
	svc, _, ctx := seedConstrainedPostType(t)
	for _, slug := range []string{"alpha", "beta-two"} {
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "Title", "slug": slug, "sku": "ABC-1"}))
		require.NoError(t, err)
	}
	art, err := svc.ExportSchema(ctx)
	require.NoError(t, err)
	setField := func(key string, fn func(f *domain.ArtifactField)) {
		for i := range art.Types[0].Fields {
			if art.Types[0].Fields[i].Key == key {
				fn(&art.Types[0].Fields[i])
			}
		}
	}
	setField("slug", func(f *domain.ArtifactField) { f.Min = fnum(6) }) // "alpha" is 5
	setField("sku", func(f *domain.ArtifactField) { f.Unique = true })  // both hold ABC-1
	setField("price", func(f *domain.ArtifactField) { f.Max = fnum(1000) })

	plan, err := svc.PlanSchema(ctx, art, false)
	require.NoError(t, err)
	byKey := map[string]PlanStep{}
	for _, s := range plan.Steps {
		byKey[s.Field] = s
	}
	require.Len(t, byKey, 3, "%+v", plan.Steps)
	assert.True(t, byKey["slug"].Blocked)
	assert.Equal(t, 1, byKey["slug"].Entries)
	assert.Equal(t, "CONTENT_FIELD_CONSTRAINT_BACKFILL", byKey["slug"].Code)
	assert.True(t, byKey["sku"].Blocked)
	assert.Equal(t, 1, byKey["sku"].Entries, "one duplicate value, however many rows share it")
	assert.Equal(t, "CONTENT_FIELD_UNIQUE_DUPLICATES", byKey["sku"].Code)
	assert.False(t, byKey["price"].Blocked)
	assert.Equal(t, 2, plan.Blocked)
	assert.Equal(t, 1, plan.Applicable)

	_, err = svc.ApplySchema(ctx, art, false)
	require.Error(t, err, "a blocked plan must not apply")

	// With the blocked steps withdrawn the relaxation applies and lands.
	setField("slug", func(f *domain.ArtifactField) { f.Min = nil })
	setField("sku", func(f *domain.ArtifactField) { f.Unique = false })
	res, err := svc.ApplySchema(ctx, art, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Applicable)
	dto, err := svc.GetContentType(ctx, "post")
	require.NoError(t, err)
	for _, f := range dto.Fields {
		if f.Key == "price" {
			assert.Equal(t, 1000.0, *f.Max)
		}
	}
}
