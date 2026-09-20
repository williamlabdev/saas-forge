package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

func f64(v float64) *float64 { return &v }

func seoComponentInput() CreateComponentInput {
	return CreateComponentInput{
		Name: "seo", Label: "SEO",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true, FieldConstraints: domain.FieldConstraints{Max: f64(60)}},
			{Key: "kind", Type: domain.FieldTypeEnum, EnumValues: []string{"article", "product"}},
			{Key: "tags", Type: domain.FieldTypeString, Multiple: true},
		},
	}
}

// seedSeo is the fixture every component test shares: one component, and TWO
// types that embed it — page.meta as a single object, post.blocks as a list —
// because the rename / delete / guard verbs all promise to visit every
// referrer in both shapes, and a fixture with one referrer would let a verb
// that stopped at the first pass.
func seedSeo(t *testing.T) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateComponent(ctx, seoComponentInput())
	require.NoError(t, err)
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "page", Fields: []FieldInput{
		{Key: "title", Type: domain.FieldTypeString, Required: true},
		{Key: "meta", Type: domain.FieldTypeComponent, Component: "seo"},
	}})
	require.NoError(t, err)
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "post", Fields: []FieldInput{
		{Key: "title", Type: domain.FieldTypeString},
		{Key: "blocks", Type: domain.FieldTypeComponent, Component: "seo", Multiple: true},
	}})
	require.NoError(t, err)
	return svc, repo, ctx
}

func componentFieldKeys(dto ComponentDTO) []string {
	out := make([]string, 0, len(dto.Fields))
	for _, f := range dto.Fields {
		out = append(out, f.Key)
	}
	return out
}

// --- definition ---------------------------------------------------------------

func TestCreateComponent_ShapeAndRefusals(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		dto, err := svc.CreateComponent(ctx, seoComponentInput())
		require.NoError(t, err)
		assert.Equal(t, "seo", dto.Name)
		assert.Equal(t, "SEO", dto.Label)
		assert.Equal(t, []string{"title", "kind", "tags"}, componentFieldKeys(dto))
		assert.Empty(t, dto.UsedBy)
		for _, f := range dto.Fields {
			assert.Empty(t, f.Supported, "a sub-field is not filterable (ADR-020 §3)")
		}
		list, err := svc.ListComponents(ctx)
		require.NoError(t, err)
		require.Len(t, list, 1)
	})
	t.Run("duplicate name is 409", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		_, err := svc.CreateComponent(ctx, seoComponentInput())
		require.NoError(t, err)
		_, err = svc.CreateComponent(ctx, seoComponentInput())
		mustCode(t, err, "CONTENT_COMPONENT_EXISTS", 409)
	})
	for _, tc := range []struct {
		name   string
		mutate func(in *CreateComponentInput)
		code   string
	}{
		{"invalid name", func(in *CreateComponentInput) { in.Name = "Seo-Block" }, "CONTENT_COMPONENT_NAME_INVALID"},
		{"no fields", func(in *CreateComponentInput) { in.Fields = nil }, "CONTENT_COMPONENT_NO_FIELDS"},
		{"nested component", func(in *CreateComponentInput) {
			in.Fields = append(in.Fields, FieldInput{Key: "inner", Type: domain.FieldTypeComponent, Component: "x"})
		}, "CONTENT_COMPONENT_NESTING_UNSUPPORTED"},
		{"unique sub-field", func(in *CreateComponentInput) {
			in.Fields[0].Unique = true
		}, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED"},
		{"read_roles sub-field", func(in *CreateComponentInput) {
			in.Fields[0].ReadRoles = []string{"admin"}
		}, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED"},
		{"write_roles sub-field", func(in *CreateComponentInput) {
			in.Fields[0].WriteRoles = []string{"admin"}
		}, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED"},
		{"duplicate key", func(in *CreateComponentInput) {
			in.Fields = append(in.Fields, FieldInput{Key: "title", Type: domain.FieldTypeString})
		}, "CONTENT_FIELD_DUPLICATE"},
		{"too many sub-fields", func(in *CreateComponentInput) {
			for i := 0; i <= domain.MaxComponentFields; i++ {
				in.Fields = append(in.Fields, FieldInput{Key: fmt.Sprintf("f%d", i), Type: domain.FieldTypeString})
			}
		}, "CONTENT_QUOTA_EXCEEDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSvc()
			in := seoComponentInput()
			tc.mutate(&in)
			_, err := svc.CreateComponent(ctxTenant("t1"), in)
			assert.Equal(t, tc.code, codeOf(t, err))
		})
	}
	t.Run("per-tenant ceiling", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		for i := 0; i < domain.MaxComponentsPerTenant; i++ {
			in := seoComponentInput()
			in.Name = fmt.Sprintf("c%d", i)
			_, err := svc.CreateComponent(ctx, in)
			require.NoError(t, err)
		}
		_, err := svc.CreateComponent(ctx, seoComponentInput())
		ae := mustCode(t, err, "CONTENT_QUOTA_EXCEEDED", 429)
		assert.Equal(t, "components", ae.Details["resource"])
	})
}

func TestTypeField_ComponentReference(t *testing.T) {
	svc, _, ctx := seedSeo(t)

	t.Run("dto names the component and offers no operators", func(t *testing.T) {
		dto, err := svc.GetContentType(ctx, "page")
		require.NoError(t, err)
		require.Len(t, dto.Fields, 2)
		meta := dto.Fields[1]
		assert.Equal(t, domain.FieldTypeComponent, meta.Type)
		assert.Equal(t, "seo", meta.Component)
		assert.Empty(t, meta.Supported)
		assert.Empty(t, dto.Fields[0].Component)
		comp, err := svc.GetComponent(ctx, "seo")
		require.NoError(t, err)
		assert.Equal(t, []string{"page", "post"}, comp.UsedBy)
	})
	for _, tc := range []struct {
		name string
		in   FieldInput
		code string
	}{
		{"component type needs a component", FieldInput{Key: "x", Type: domain.FieldTypeComponent}, "CONTENT_COMPONENT_REQUIRED"},
		{"component on a string field", FieldInput{Key: "x", Type: domain.FieldTypeString, Component: "seo"}, "CONTENT_COMPONENT_NOT_APPLICABLE"},
		{"unknown component", FieldInput{Key: "x", Type: domain.FieldTypeComponent, Component: "ghost"}, "CONTENT_COMPONENT_NOT_FOUND"},
		{"unique on a component field", FieldInput{Key: "x", Type: domain.FieldTypeComponent, Component: "seo",
			FieldConstraints: domain.FieldConstraints{Unique: true}}, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AddField(ctx, "page", tc.in)
			ae := mustCode(t, err, tc.code, 422)
			_ = ae
		})
	}
	t.Run("not found is a 404 that names the component", func(t *testing.T) {
		_, err := svc.GetComponent(ctx, "ghost")
		ae := mustCode(t, err, "CONTENT_COMPONENT_NOT_FOUND", 404)
		assert.Equal(t, "ghost", ae.Details["component"])
	})
}

// --- values -------------------------------------------------------------------

func TestComponentValue_Validation(t *testing.T) {
	svc, _, ctx := seedSeo(t)
	create := func(typeName string, doc map[string]any) error {
		_, err := svc.CreateEntry(ctx, typeName, mustJSON(t, doc))
		return err
	}
	fieldOf := func(err error) string {
		var ae interface{ Details() map[string]any }
		_ = ae
		return fmt.Sprint(mustCode(t, err, codeOf(t, err), 422).Details["field"])
	}

	t.Run("single: an object is accepted, an array is not", func(t *testing.T) {
		require.NoError(t, create("page", map[string]any{"title": "p", "meta": map[string]any{"title": "hello"}}))
		err := create("page", map[string]any{"title": "p", "meta": []any{map[string]any{"title": "hello"}}})
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
		assert.Equal(t, "meta", fieldOf(err))
	})
	t.Run("multiple: an array is accepted, an object is not", func(t *testing.T) {
		require.NoError(t, create("post", map[string]any{"blocks": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}}}))
		err := create("post", map[string]any{"blocks": map[string]any{"title": "a"}})
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
	})
	t.Run("unknown sub-field key is refused with a dotted path", func(t *testing.T) {
		err := create("page", map[string]any{"title": "p", "meta": map[string]any{"title": "x", "bogus": 1}})
		assert.Equal(t, "CONTENT_FIELD_UNKNOWN", codeOf(t, err))
		assert.Equal(t, "meta.bogus", fieldOf(err))
	})
	t.Run("required sub-field", func(t *testing.T) {
		err := create("page", map[string]any{"title": "p", "meta": map[string]any{"kind": "article"}})
		assert.Equal(t, "CONTENT_FIELD_REQUIRED", codeOf(t, err))
		assert.Equal(t, "meta.title", fieldOf(err))
	})
	t.Run("per-type validators run on sub-fields", func(t *testing.T) {
		err := create("page", map[string]any{"title": "p", "meta": map[string]any{"title": strings.Repeat("x", 61)}})
		assert.Equal(t, "meta.title", fieldOf(err))
		err = create("page", map[string]any{"title": "p", "meta": map[string]any{"title": "x", "kind": "nope"}})
		assert.Equal(t, "meta.kind", fieldOf(err))
		err = create("page", map[string]any{"title": "p", "meta": map[string]any{"title": "x", "tags": "not-a-list"}})
		assert.Equal(t, "meta.tags", fieldOf(err))
	})
	t.Run("the failing item is named for a list", func(t *testing.T) {
		err := create("post", map[string]any{"blocks": []any{map[string]any{"title": "a"}, map[string]any{"kind": "article"}}})
		ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED", 422)
		assert.Equal(t, "blocks.title", ae.Details["field"])
		assert.EqualValues(t, 1, ae.Details["item"])
	})
	t.Run("a list item that is not an object", func(t *testing.T) {
		err := create("post", map[string]any{"blocks": []any{"nope"}})
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
	})
	t.Run("the list ceiling applies", func(t *testing.T) {
		items := make([]any, domain.MaxMultipleElements+1)
		for i := range items {
			items[i] = map[string]any{"title": "t"}
		}
		err := create("post", map[string]any{"blocks": items})
		require.Error(t, err)
		mustCode(t, err, codeOf(t, err), 422)
	})
	t.Run("absent component value is fine when the field is optional", func(t *testing.T) {
		require.NoError(t, create("page", map[string]any{"title": "p"}))
	})
}

// ADR-020 §3, the hard line: a component field and its sub-fields are never a
// filter, sort, unique or populate target — in any spelling, including the
// dotted one — and the repository never hears about them.
func TestComponentQueryRefusals(t *testing.T) {
	svc, repo, ctx := seedSeo(t)
	_, err := svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{"title": "p", "meta": map[string]any{"title": "hello"}}))
	require.NoError(t, err)

	for _, op := range []string{"eq", "neq", "contains", "in", "has", "gt"} {
		_, err := svc.ListEntries(ctx, "page", ListEntriesInput{Filters: []string{"meta:" + op + ":x"}})
		assert.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", codeOf(t, err), op)
	}
	_, err = svc.ListEntries(ctx, "page", ListEntriesInput{Filters: []string{"meta.title:eq:hello"}})
	assert.Equal(t, "CONTENT_FILTER_FIELD_UNKNOWN", codeOf(t, err))
	_, err = svc.ListEntries(ctx, "page", ListEntriesInput{Sort: "meta:asc"})
	assert.Equal(t, "CONTENT_SORT_FIELD_UNSORTABLE", codeOf(t, err))
	_, err = svc.ListEntries(ctx, "page", ListEntriesInput{Sort: "meta.title:asc"})
	assert.Equal(t, "CONTENT_SORT_FIELD_UNKNOWN", codeOf(t, err))
	// Both audiences that may populate reach the SAME refusal (ADR-006
	// Amendment 7): a component field is not a relation, whichever side asks.
	for _, as := range []context.Context{ctxDelivery("t1"), ctx} {
		_, err = svc.ListEntries(as, "page", ListEntriesInput{Populate: []string{"meta"}})
		assert.Equal(t, "CONTENT_POPULATE_NOT_RELATION", codeOf(t, err))
		// "meta" fails the relation check on its OWN before the second segment
		// is ever looked up — the dotted-path walk resolves left to right, and
		// meta is a component whichever position it names.
		_, err = svc.ListEntries(as, "page", ListEntriesInput{Populate: []string{"meta.title"}})
		assert.Equal(t, "CONTENT_POPULATE_NOT_RELATION", codeOf(t, err))
	}

	assert.Empty(t, repo.lastList.Filters, "no refused filter may reach the repository")
	assert.Nil(t, repo.lastList.Sort, "no refused sort may reach the repository")

	// The sibling scalar field still works, and is the ONLY thing the SQL
	// builder is ever handed.
	res, err := svc.ListEntries(ctx, "page", ListEntriesInput{Filters: []string{"title:eq:p"}, Sort: "title:asc"})
	require.NoError(t, err)
	assert.Equal(t, 1, *res.Total)
	require.Len(t, repo.lastList.Filters, 1)
	assert.Equal(t, "title", repo.lastList.Filters[0].Field.Key)
}

// --- schema mutation across referrers ----------------------------------------

func seedSeoEntries(t *testing.T, svc ContentService, repo *memRepo, ctx context.Context) (page, post EntryDTO) {
	t.Helper()
	page, err := svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{
		"title": "p", "meta": map[string]any{"title": "page seo", "kind": "article"}}))
	require.NoError(t, err)
	post, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
		"title": "q", "blocks": []any{
			map[string]any{"title": "one", "kind": "product"},
			map[string]any{"title": "two"},
		}}))
	require.NoError(t, err)
	// Publish both so the live snapshot exists — the verbs promise BOTH copies.
	for _, e := range []struct {
		typ string
		dto EntryDTO
	}{{"page", page}, {"post", post}} {
		_, err := svc.SetEntryStatus(ctx, e.typ, e.dto.ID, domain.StatusPublished, e.dto.Version)
		require.NoError(t, err)
	}
	return page, post
}

func docOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc
}

func TestComponentField_RenameRewritesEveryReferrerInBothCopies(t *testing.T) {
	svc, repo, ctx := seedSeo(t)
	page, post := seedSeoEntries(t, svc, repo, ctx)

	dto, err := svc.RenameComponentField(ctx, "seo", "title", RenameInput{Key: "headline"})
	require.NoError(t, err)
	assert.Equal(t, []string{"headline", "kind", "tags"}, componentFieldKeys(dto), "sub-field order survives the rename")

	for _, e := range repo.entries {
		for _, raw := range []json.RawMessage{e.Payload, e.PublishedPayload} {
			doc := docOf(t, raw)
			switch e.ID {
			case page.ID:
				meta := doc["meta"].(map[string]any)
				assert.Equal(t, "page seo", meta["headline"])
				assert.NotContains(t, meta, "title")
			case post.ID:
				blocks := doc["blocks"].([]any)
				require.Len(t, blocks, 2)
				assert.Equal(t, "one", blocks[0].(map[string]any)["headline"])
				assert.Equal(t, "two", blocks[1].(map[string]any)["headline"])
				assert.NotContains(t, blocks[0].(map[string]any), "title")
			}
			// The TYPE's own title field is untouched: the rewrite descends
			// into the component value, never the document.
			assert.Contains(t, doc, "title")
		}
	}
	// The referring types see the renamed sub-field without a reload.
	ct, err := svc.GetContentType(ctx, "post")
	require.NoError(t, err)
	assert.Equal(t, "seo", ct.Fields[1].Component)
	// And a new entry is validated against the NEW key.
	_, err = svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{"title": "x", "meta": map[string]any{"title": "old key"}}))
	assert.Equal(t, "CONTENT_FIELD_UNKNOWN", codeOf(t, err))
	_, err = svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{"title": "x", "meta": map[string]any{"headline": "new key"}}))
	require.NoError(t, err)
}

func TestComponentField_DeletePrunesEveryReferrer(t *testing.T) {
	svc, repo, ctx := seedSeo(t)
	page, post := seedSeoEntries(t, svc, repo, ctx)

	_, err := svc.DeleteComponentField(ctx, "seo", "kind", false)
	mustCode(t, err, "CONTENT_FIELD_HAS_DATA", 409)

	dto, err := svc.DeleteComponentField(ctx, "seo", "kind", true)
	require.NoError(t, err)
	assert.Equal(t, []string{"title", "tags"}, componentFieldKeys(dto))
	for _, e := range repo.entries {
		for _, raw := range []json.RawMessage{e.Payload, e.PublishedPayload} {
			doc := docOf(t, raw)
			switch e.ID {
			case page.ID:
				assert.NotContains(t, doc["meta"].(map[string]any), "kind")
			case post.ID:
				for _, b := range doc["blocks"].([]any) {
					assert.NotContains(t, b.(map[string]any), "kind")
				}
			}
		}
	}
	// A sub-field nobody stored a value for deletes without consent.
	_, err = svc.DeleteComponentField(ctx, "seo", "tags", false)
	require.NoError(t, err)
}

func TestComponentField_GuardsRunOverTheItemsOfEveryReferrer(t *testing.T) {
	t.Run("required add is refused while any item lacks it", func(t *testing.T) {
		svc, repo, ctx := seedSeo(t)
		seedSeoEntries(t, svc, repo, ctx)
		_, err := svc.AddComponentField(ctx, "seo", FieldInput{Key: "canonical", Type: domain.FieldTypeString, Required: true})
		ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED_BACKFILL", 409)
		assert.EqualValues(t, 2, ae.Details["entries"], "one page + one post, each counted once")
		// Optional is additive.
		_, err = svc.AddComponentField(ctx, "seo", FieldInput{Key: "canonical", Type: domain.FieldTypeString})
		require.NoError(t, err)
	})
	t.Run("required tightening is refused while any item lacks it", func(t *testing.T) {
		svc, repo, ctx := seedSeo(t)
		seedSeoEntries(t, svc, repo, ctx)
		yes := true
		_, err := svc.UpdateComponentField(ctx, "seo", "kind", UpdateFieldInput{Required: &yes})
		mustCode(t, err, "CONTENT_FIELD_REQUIRED_BACKFILL", 409)
	})
	t.Run("enum removal is refused while any item uses the value", func(t *testing.T) {
		svc, repo, ctx := seedSeo(t)
		seedSeoEntries(t, svc, repo, ctx)
		fewer := []string{"article"}
		_, err := svc.UpdateComponentField(ctx, "seo", "kind", UpdateFieldInput{EnumValues: &fewer})
		mustCode(t, err, "CONTENT_ENUM_VALUE_IN_USE", 409)
		more := []string{"article", "product", "video"}
		_, err = svc.UpdateComponentField(ctx, "seo", "kind", UpdateFieldInput{EnumValues: &more})
		require.NoError(t, err)
	})
	t.Run("constraint tightening is refused while any item fails it", func(t *testing.T) {
		svc, repo, ctx := seedSeo(t)
		seedSeoEntries(t, svc, repo, ctx)
		_, err := svc.UpdateComponentField(ctx, "seo", "title", UpdateFieldInput{Max: OptionalNumber{Set: true, Value: f64(3)}})
		mustCode(t, err, "CONTENT_FIELD_CONSTRAINT_BACKFILL", 409)
		_, err = svc.UpdateComponentField(ctx, "seo", "title", UpdateFieldInput{Max: OptionalNumber{Set: true, Value: f64(80)}})
		require.NoError(t, err)
	})
	t.Run("the attributes a sub-field cannot carry are refused on update too", func(t *testing.T) {
		svc, _, ctx := seedSeo(t)
		yes := true
		_, err := svc.UpdateComponentField(ctx, "seo", "title", UpdateFieldInput{Unique: &yes})
		mustCode(t, err, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", 422)
		roles := []string{"admin"}
		_, err = svc.UpdateComponentField(ctx, "seo", "title", UpdateFieldInput{ReadRoles: &roles})
		mustCode(t, err, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", 422)
	})
	t.Run("type, multiple and relation are immutable", func(t *testing.T) {
		svc, _, ctx := seedSeo(t)
		typ := domain.FieldTypeText
		_, err := svc.UpdateComponentField(ctx, "seo", "title", UpdateFieldInput{Type: &typ})
		mustCode(t, err, "CONTENT_FIELD_TYPE_IMMUTABLE", 422)
		multi := true
		_, err = svc.UpdateComponentField(ctx, "seo", "title", UpdateFieldInput{Multiple: &multi})
		mustCode(t, err, "CONTENT_FIELD_MULTIPLE_IMMUTABLE", 422)
	})
	t.Run("a nested component cannot be added later either", func(t *testing.T) {
		svc, _, ctx := seedSeo(t)
		_, err := svc.AddComponentField(ctx, "seo", FieldInput{Key: "inner", Type: domain.FieldTypeComponent, Component: "seo"})
		mustCode(t, err, "CONTENT_COMPONENT_NESTING_UNSUPPORTED", 422)
	})
}

func TestDeleteComponent_RefusedWhileReferenced(t *testing.T) {
	svc, _, ctx := seedSeo(t)
	err := svc.DeleteComponent(ctx, "seo")
	ae := mustCode(t, err, "CONTENT_COMPONENT_IN_USE", 409)
	assert.Equal(t, []string{"page", "post"}, ae.Details["used_by"])

	_, err = svc.DeleteField(ctx, "page", "meta", true)
	require.NoError(t, err)
	err = svc.DeleteComponent(ctx, "seo")
	ae = mustCode(t, err, "CONTENT_COMPONENT_IN_USE", 409)
	assert.Equal(t, []string{"post"}, ae.Details["used_by"])

	_, err = svc.DeleteField(ctx, "post", "blocks", true)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteComponent(ctx, "seo"))
	_, err = svc.GetComponent(ctx, "seo")
	mustCode(t, err, "CONTENT_COMPONENT_NOT_FOUND", 404)

	// Label is the one thing PATCH changes.
	_, err = svc.CreateComponent(ctx, seoComponentInput())
	require.NoError(t, err)
	label := "Search engine"
	dto, err := svc.UpdateComponent(ctx, "seo", UpdateComponentInput{Label: &label})
	require.NoError(t, err)
	assert.Equal(t, "Search engine", dto.Label)
}

// --- permissions ----------------------------------------------------------------

// ADR-020 §5: read_roles / write_roles on the component FIELD gate the whole
// value; there is no per-sub-field gate.
func TestComponentField_RolesGateTheWholeValue(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	editor := ctxRole("t1", "editor")
	viewer := ctxRole("t1", "viewer")
	_, err := svc.CreateComponent(admin, seoComponentInput())
	require.NoError(t, err)
	_, err = svc.CreateContentType(admin, CreateTypeInput{Name: "page", Fields: []FieldInput{
		{Key: "title", Type: domain.FieldTypeString},
		{Key: "meta", Type: domain.FieldTypeComponent, Component: "seo", ReadRoles: []string{"editor"}, WriteRoles: []string{"editor"}},
	}})
	require.NoError(t, err)
	e, err := svc.CreateEntry(editor, "page", mustJSON(t, map[string]any{"title": "p", "meta": map[string]any{"title": "secret"}}))
	require.NoError(t, err)
	seen, err := svc.GetEntry(editor, "page", e.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.Equal(t, "secret", docOf(t, mustMarshalData(t, seen))["meta"].(map[string]any)["title"], "the role that holds it sees the whole value")

	got, err := svc.GetEntry(viewer, "page", e.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.NotContains(t, docOf(t, mustMarshalData(t, got)), "meta", "a role outside read_roles sees no part of the value")
	assert.Equal(t, "p", docOf(t, mustMarshalData(t, got))["title"], "and still sees the open field")
	_, err = svc.UpdateEntry(viewer, "page", e.ID, mustJSON(t, map[string]any{"title": "p", "meta": map[string]any{"title": "x"}}), e.Version)
	require.Error(t, err, "and cannot write any part of it")
}

// --- agent audience -------------------------------------------------------------

func TestComponent_AgentReadsThroughItsWhitelist(t *testing.T) {
	svc, _, admin := seedSeo(t)
	principal := uuid.New()
	pageBot := agentCtx("t1", principal, uuid.New(), "page")
	otherBot := agentCtx("t1", principal, uuid.New(), "other")

	dto, err := svc.GetComponent(pageBot, "seo")
	require.NoError(t, err)
	assert.Equal(t, []string{"page"}, dto.UsedBy, "used_by is narrowed to the whitelist; post exists but is not named")

	list, err := svc.ListComponents(pageBot)
	require.NoError(t, err)
	require.Len(t, list, 1)

	_, err = svc.GetComponent(otherBot, "seo")
	mustCode(t, err, "CONTENT_AGENT_COMPONENT_NOT_ALLOWED", 403)
	list, err = svc.ListComponents(otherBot)
	require.NoError(t, err)
	assert.Empty(t, list)

	// Writes: refused by construction, whatever the whitelist says.
	_, err = svc.CreateComponent(pageBot, CreateComponentInput{Name: "og", Fields: []FieldInput{{Key: "k", Type: domain.FieldTypeString}}})
	require.Error(t, err)
	mustCode(t, err, codeOf(t, err), 403)
	_, err = svc.AddComponentField(pageBot, "seo", FieldInput{Key: "k", Type: domain.FieldTypeString})
	require.Error(t, err)
	_ = admin
}

// --- artifact -------------------------------------------------------------------

func TestComponent_ArtifactExportPlanApply(t *testing.T) {
	svc, repo, ctx := seedSeo(t)
	art, err := svc.ExportSchema(ctx)
	require.NoError(t, err)
	require.Len(t, art.Components, 1)
	assert.Equal(t, "seo", art.Components[0].Name)
	assert.Equal(t, "seo", art.Types[0].Fields[1].Component)

	plan, err := svc.PlanSchema(ctx, art, true)
	require.NoError(t, err)
	assert.Empty(t, plan.Steps, "an export plans as a no-op against its own tenant")

	// Apply the export to a FRESH tenant: the component is created before the
	// types that embed it, through the same verbs.
	fresh := ctxTenant("t2")
	res, err := svc.ApplySchema(fresh, art, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Applicable)
	comp, err := svc.GetComponent(fresh, "seo")
	require.NoError(t, err)
	assert.Equal(t, []string{"page", "post"}, comp.UsedBy)

	// Prune with a referrer kept is blocked, not silently skipped.
	seedSeoEntries(t, svc, repo, ctx)
	pruned := art
	pruned.Components = nil
	plan, err = svc.PlanSchema(ctx, pruned, true)
	require.NoError(t, err)
	assert.Equal(t, 1, plan.Blocked)
	assert.Equal(t, 0, plan.Refused)
	_, err = svc.ApplySchema(ctx, pruned, true)
	mustCode(t, err, "CONTENT_SCHEMA_NOT_APPLICABLE", 409)

	// A sub-field rename through the artifact is add+drop with the hint; the
	// drop is guarded by the data the items hold.
	renamed := art
	renamed.Components = []domain.ArtifactComponent{{Name: "seo", Label: "SEO", Fields: []domain.ArtifactField{
		{Key: "headline", Type: domain.FieldTypeString, Required: false},
		art.Components[0].Fields[1], art.Components[0].Fields[2],
	}}}
	plan, err = svc.PlanSchema(ctx, renamed, true)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 2)
	assert.Equal(t, domain.OpAddComponentField, plan.Steps[0].Op)
	assert.Equal(t, domain.OpDeleteComponentField, plan.Steps[1].Op)
	assert.True(t, plan.Steps[1].Blocked)
	assert.Equal(t, 2, plan.Steps[1].Entries)
}

// The component a field embeds is fixed at creation: the stored items have
// the OLD component's shape, and neither door — the field PATCH nor the
// artifact — may re-point the reference (ADR-020, mirroring type/multiple).
func TestTypeField_ComponentReferenceIsImmutable(t *testing.T) {
	svc, _, ctx := seedSeo(t)
	og := seoComponentInput()
	og.Name, og.Label = "og", "Open Graph"
	_, err := svc.CreateComponent(ctx, og)
	require.NoError(t, err)

	t.Run("PATCH with a different component is 422", func(t *testing.T) {
		name := "og"
		_, err := svc.UpdateField(ctx, "page", "meta", UpdateFieldInput{Component: &name})
		ae := mustCode(t, err, "CONTENT_FIELD_COMPONENT_IMMUTABLE", 422)
		assert.Equal(t, "meta", ae.Details["field"])
	})
	t.Run("PATCH sending the component it already has is refused too", func(t *testing.T) {
		name := "seo"
		_, err := svc.UpdateField(ctx, "page", "meta", UpdateFieldInput{Component: &name})
		mustCode(t, err, "CONTENT_FIELD_COMPONENT_IMMUTABLE", 422)
	})
	t.Run("an artifact that re-points the reference plans as refused and cannot apply", func(t *testing.T) {
		art, err := svc.ExportSchema(ctx)
		require.NoError(t, err)
		require.Equal(t, "meta", art.Types[0].Fields[1].Key)
		art.Types[0].Fields[1].Component = "og"

		plan, err := svc.PlanSchema(ctx, art, false)
		require.NoError(t, err)
		require.Equal(t, 1, plan.Refused)
		require.Len(t, plan.Steps, 1)
		assert.Equal(t, domain.GradeRefused, plan.Steps[0].Grade)
		assert.Equal(t, "CONTENT_FIELD_COMPONENT_IMMUTABLE", plan.Steps[0].Code)
		assert.Equal(t, "page", plan.Steps[0].Type)

		_, err = svc.ApplySchema(ctx, art, false)
		mustCode(t, err, "CONTENT_SCHEMA_NOT_APPLICABLE", 409)
	})
}
