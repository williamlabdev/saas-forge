package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// Dynamic zones (ADR-020 Amendment 1). The fixture is deliberately the shape
// the cross-cutting rules are hardest on: TWO components with a COLLIDING
// sub-field key ("body"), and TWO types holding zones over them — one type
// also embedding a component the plain way. A fixture with one component and
// one type would let every rule that stops at the first match pass.
func seedZone(t *testing.T) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateComponent(ctx, CreateComponentInput{
		Name: "hero", Label: "Hero",
		Fields: []FieldInput{
			{Key: "headline", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeText},
			{Key: "image", Type: domain.FieldTypeFile},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateComponent(ctx, CreateComponentInput{
		Name: "cta", Label: "Call to action",
		Fields: []FieldInput{
			{Key: "label", Type: domain.FieldTypeString},
			{Key: "body", Type: domain.FieldTypeText},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "page", Fields: []FieldInput{
		{Key: "title", Type: domain.FieldTypeString, Required: true},
		{Key: "zone", Type: domain.FieldTypeDynamicZone, Components: []string{"hero", "cta"}},
	}})
	require.NoError(t, err)
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "post", Fields: []FieldInput{
		{Key: "title", Type: domain.FieldTypeString},
		{Key: "sections", Type: domain.FieldTypeDynamicZone, Components: []string{"hero"}},
	}})
	require.NoError(t, err)
	return svc, repo, ctx
}

func zoneItem(name string, kv map[string]any) map[string]any {
	item := map[string]any{domain.ZoneDiscriminator: name}
	for k, v := range kv {
		item[k] = v
	}
	return item
}

// seedZoneEntries writes one entry per type and publishes both, so every rule
// below is asserted against the working copy AND the live snapshot.
func seedZoneEntries(t *testing.T, svc ContentService, ctx context.Context) (page, post EntryDTO) {
	t.Helper()
	page, err := svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{
		"title": "p", "zone": []any{
			zoneItem("hero", map[string]any{"headline": "h1", "body": "hero body"}),
			zoneItem("cta", map[string]any{"label": "go", "body": "cta body"}),
		}}))
	require.NoError(t, err)
	post, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
		"title": "q", "sections": []any{
			zoneItem("hero", map[string]any{"headline": "h2", "body": "other hero body"}),
		}}))
	require.NoError(t, err)
	for _, e := range []struct {
		typ string
		dto EntryDTO
	}{{"page", page}, {"post", post}} {
		_, err := svc.SetEntryStatus(ctx, e.typ, e.dto.ID, domain.StatusPublished, e.dto.Version)
		require.NoError(t, err)
	}
	return page, post
}

// --- definition ---------------------------------------------------------------

func TestDynamicZone_DefinitionRefusals(t *testing.T) {
	base := func() FieldInput {
		return FieldInput{Key: "extra", Type: domain.FieldTypeDynamicZone, Components: []string{"hero"}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(f *FieldInput)
		code   string
		status int
	}{
		{"no components", func(f *FieldInput) { f.Components = nil }, "CONTENT_ZONE_COMPONENTS_REQUIRED", 422},
		{"empty components", func(f *FieldInput) { f.Components = []string{} }, "CONTENT_ZONE_COMPONENTS_REQUIRED", 422},
		{"duplicate component", func(f *FieldInput) { f.Components = []string{"hero", "hero"} }, "CONTENT_ZONE_COMPONENT_DUPLICATE", 422},
		{"unknown component", func(f *FieldInput) { f.Components = []string{"ghost"} }, "CONTENT_COMPONENT_NOT_FOUND", 422},
		// multiple is refused rather than quietly ignored: a zone IS a list,
		// and a list of lists is not a shape any value in this system has.
		{"multiple", func(f *FieldInput) { f.Multiple = true }, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED", 422},
		{"unique", func(f *FieldInput) { f.FieldConstraints = domain.FieldConstraints{Unique: true} }, "CONTENT_FIELD_CONSTRAINT_NOT_APPLICABLE", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, ctx := seedZone(t)
			f := base()
			tc.mutate(&f)
			_, err := svc.AddField(ctx, "page", f)
			mustCode(t, err, tc.code, tc.status)
		})
	}
	t.Run("components on a non-zone field", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		_, err := svc.AddField(ctx, "page", FieldInput{Key: "s", Type: domain.FieldTypeString, Components: []string{"hero"}})
		mustCode(t, err, "CONTENT_ZONE_COMPONENTS_NOT_APPLICABLE", 422)
	})
	// A zone inside a component is nesting, and shares the component case's
	// code — one refusal, one name.
	t.Run("a zone inside a component is refused", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		_, err := svc.AddComponentField(ctx, "hero", FieldInput{
			Key: "inner", Type: domain.FieldTypeDynamicZone, Components: []string{"cta"}})
		mustCode(t, err, "CONTENT_COMPONENT_NESTING_UNSUPPORTED", 422)
	})
}

// --- values -------------------------------------------------------------------

func TestDynamicZone_ValueRules(t *testing.T) {
	svc, _, ctx := seedZone(t)
	write := func(zone any) error {
		_, err := svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{"title": "x", "zone": zone}))
		return err
	}
	t.Run("an object is not a zone value", func(t *testing.T) {
		// Always an array, even for one item: a caller that may send either
		// shape makes every reader of the value branch on it forever.
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH",
			codeOf(t, write(zoneItem("hero", map[string]any{"headline": "h"}))))
	})
	t.Run("an item must be an object", func(t *testing.T) {
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, write([]any{"hero"})))
	})
	t.Run("an item must name its component", func(t *testing.T) {
		err := write([]any{map[string]any{"headline": "h"}})
		ae := mustCode(t, err, "CONTENT_ZONE_COMPONENT_MISSING", 422)
		assert.Equal(t, domain.ZoneDiscriminator, ae.Details["discriminator"])
	})
	t.Run("the component must be on the allowed list", func(t *testing.T) {
		err := write([]any{zoneItem("seo", map[string]any{})})
		ae := mustCode(t, err, "CONTENT_ZONE_COMPONENT_NOT_ALLOWED", 422)
		assert.Equal(t, "seo", ae.Details["component"])
		assert.Equal(t, []string{"hero", "cta"}, ae.Details["allowed"])
	})
	t.Run("an item is validated as that component's own payload", func(t *testing.T) {
		// The failure names "zone.headline" and the item INDEX, which is the
		// only way an editor finds the one bad card in a list of thirty.
		err := write([]any{
			zoneItem("cta", map[string]any{"label": "ok"}),
			zoneItem("hero", map[string]any{"body": "no headline"}),
		})
		ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED", 422)
		assert.Equal(t, "zone.headline", ae.Details["field"])
		assert.EqualValues(t, 1, ae.Details["item"])
	})
	t.Run("an unknown sub-key is refused", func(t *testing.T) {
		err := write([]any{zoneItem("hero", map[string]any{"headline": "h", "nope": 1})})
		ae := mustCode(t, err, "CONTENT_FIELD_UNKNOWN", 422)
		assert.Equal(t, "zone.nope", ae.Details["field"])
	})
	t.Run("a sub-key of the OTHER allowed component is still unknown", func(t *testing.T) {
		// The allowed list is not a union of shapes: an item is validated
		// against the component it named, and nothing else.
		err := write([]any{zoneItem("cta", map[string]any{"headline": "wrong component"})})
		assert.Equal(t, "CONTENT_FIELD_UNKNOWN", codeOf(t, err))
	})
	t.Run("too many items", func(t *testing.T) {
		xs := make([]any, domain.MaxMultipleElements+1)
		for i := range xs {
			xs[i] = zoneItem("cta", map[string]any{"label": "x"})
		}
		mustCode(t, write(xs), "CONTENT_FIELD_TOO_MANY_VALUES", 422)
	})
	t.Run("required means at least one item", func(t *testing.T) {
		_, err := svc.AddField(ctx, "post", FieldInput{
			Key: "req", Type: domain.FieldTypeDynamicZone, Components: []string{"cta"}, Required: true})
		require.NoError(t, err)
		_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "x", "req": []any{}}))
		ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED", 422)
		assert.Equal(t, "req", ae.Details["field"])
	})
	t.Run("a well-formed heterogeneous list is stored verbatim", func(t *testing.T) {
		dto, err := svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{
			"title": "x", "zone": []any{
				zoneItem("hero", map[string]any{"headline": "h"}),
				zoneItem("cta", map[string]any{"label": "l"}),
				zoneItem("hero", map[string]any{"headline": "h2"}),
			}}))
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(dto.Data, &doc))
		items := doc["zone"].([]any)
		require.Len(t, items, 3, "order and repetition are the caller's, not ours")
		assert.Equal(t, "hero", items[0].(map[string]any)[domain.ZoneDiscriminator])
		assert.Equal(t, "cta", items[1].(map[string]any)[domain.ZoneDiscriminator])
	})
}

// --- the hard line: a zone is never a query surface ---------------------------

func TestDynamicZone_NotAQuerySurface(t *testing.T) {
	svc, _, ctx := seedZone(t)
	ct, err := svc.GetContentType(ctx, "page")
	require.NoError(t, err)
	for _, f := range ct.Fields {
		if f.Key == "zone" {
			assert.Empty(t, f.Supported, "a zone advertises no operator")
			assert.Equal(t, []string{"hero", "cta"}, f.Components)
		}
	}
	_, err = svc.ListEntries(ctx, "page", ListEntriesInput{Filters: []string{"zone:eq:x"}})
	mustCode(t, err, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", 400)
	_, err = svc.ListEntries(ctx, "page", ListEntriesInput{Filters: []string{"zone.headline:eq:x"}})
	mustCode(t, err, "CONTENT_FILTER_FIELD_UNKNOWN", 400)
	_, err = svc.ListEntries(ctx, "page", ListEntriesInput{Sort: "zone"})
	mustCode(t, err, "CONTENT_SORT_FIELD_UNSORTABLE", 400)
}

// --- allowed-list edits (item 6's runtime half) -------------------------------

func TestDynamicZone_AllowedListEdits(t *testing.T) {
	t.Run("adding a name is always allowed", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		seedZoneEntries(t, svc, ctx)
		dto, err := svc.UpdateField(ctx, "post", "sections", UpdateFieldInput{Components: &[]string{"hero", "cta"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"hero", "cta"}, dto.Fields[1].Components)
		// And the newly allowed component's items validate immediately.
		_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
			"title": "x", "sections": []any{zoneItem("cta", map[string]any{"label": "l"})}}))
		require.NoError(t, err)
	})
	t.Run("adding an unknown name is refused", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		_, err := svc.UpdateField(ctx, "post", "sections", UpdateFieldInput{Components: &[]string{"hero", "ghost"}})
		mustCode(t, err, "CONTENT_COMPONENT_NOT_FOUND", 422)
	})
	t.Run("removing a name in use is 409", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		seedZoneEntries(t, svc, ctx)
		_, err := svc.UpdateField(ctx, "page", "zone", UpdateFieldInput{Components: &[]string{"hero"}})
		ae := mustCode(t, err, "CONTENT_ZONE_COMPONENT_IN_USE", 409)
		assert.Equal(t, "cta", ae.Details["component"])
		assert.EqualValues(t, 1, ae.Details["entries"])
	})
	t.Run("removing an unused name is allowed", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		// post.sections allows only hero, so widen it and narrow it back with
		// nothing of cta ever written.
		_, err := svc.UpdateField(ctx, "post", "sections", UpdateFieldInput{Components: &[]string{"hero", "cta"}})
		require.NoError(t, err)
		seedZoneEntries(t, svc, ctx)
		dto, err := svc.UpdateField(ctx, "post", "sections", UpdateFieldInput{Components: &[]string{"hero"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"hero"}, dto.Fields[1].Components)
	})
	t.Run("components is refused on a component sub-field", func(t *testing.T) {
		svc, _, ctx := seedZone(t)
		_, err := svc.UpdateComponentField(ctx, "hero", "body", UpdateFieldInput{Components: &[]string{"cta"}})
		mustCode(t, err, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", 422)
	})
}

// --- cross-cutting: the component verbs must see zone usage -------------------

// TestDynamicZone_ComponentDeleteRefusedByZone is cross-cutting item 1. There
// is no foreign key behind a zone's allowed list (it stores names), so the
// refusal is the only thing standing between a delete and a field naming a
// component that no longer exists.
func TestDynamicZone_ComponentDeleteRefusedByZone(t *testing.T) {
	svc, _, ctx := seedZone(t)
	err := svc.DeleteComponent(ctx, "cta")
	ae := mustCode(t, err, "CONTENT_COMPONENT_IN_USE", 409)
	assert.Equal(t, []string{"page"}, ae.Details["used_by"])
	// hero is named by BOTH zones, and used_by says so.
	err = svc.DeleteComponent(ctx, "hero")
	ae = mustCode(t, err, "CONTENT_COMPONENT_IN_USE", 409)
	assert.Equal(t, []string{"page", "post"}, ae.Details["used_by"])
}

// TestDynamicZone_ComponentRenameRewritesItemsAndAllowedLists is item 2: the
// name is stored in two places at once — every item's discriminator and every
// zone's allowed list — and a rename that moved one without the other would
// leave every affected entry failing its next write.
func TestDynamicZone_ComponentRenameRewritesItemsAndAllowedLists(t *testing.T) {
	svc, repo, ctx := seedZone(t)
	page, post := seedZoneEntries(t, svc, ctx)

	dto, err := svc.RenameComponent(ctx, "hero", RenameInput{Name: "banner"})
	require.NoError(t, err)
	assert.Equal(t, "banner", dto.Name)

	for _, e := range repo.entries {
		for _, raw := range []json.RawMessage{e.Payload, e.PublishedPayload} {
			doc := docOf(t, raw)
			switch e.ID {
			case page.ID:
				items := doc["zone"].([]any)
				require.Len(t, items, 2)
				assert.Equal(t, "banner", items[0].(map[string]any)[domain.ZoneDiscriminator])
				assert.Equal(t, "cta", items[1].(map[string]any)[domain.ZoneDiscriminator],
					"an item of another component is not touched")
				assert.Equal(t, "hero body", items[0].(map[string]any)["body"],
					"only the discriminator moves; the body is the editor's text")
			case post.ID:
				items := doc["sections"].([]any)
				assert.Equal(t, "banner", items[0].(map[string]any)[domain.ZoneDiscriminator])
			}
		}
	}
	for _, typ := range []struct{ name, key string }{{"page", "zone"}, {"post", "sections"}} {
		ct, err := svc.GetContentType(ctx, typ.name)
		require.NoError(t, err)
		f, ok := fieldDTOByKey(ct, typ.key)
		require.True(t, ok)
		assert.Contains(t, f.Components, "banner")
		assert.NotContains(t, f.Components, "hero")
	}
	// And the new name is what a fresh write must use.
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
		"title": "x", "sections": []any{zoneItem("hero", map[string]any{"headline": "h"})}}))
	assert.Equal(t, "CONTENT_ZONE_COMPONENT_NOT_ALLOWED", codeOf(t, err))
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
		"title": "x", "sections": []any{zoneItem("banner", map[string]any{"headline": "h"})}}))
	require.NoError(t, err)
}

// TestDynamicZone_SubFieldRenameAndDeleteReachZoneItems is item 3. The two
// components share the sub-field key "body" on purpose: a rewrite that
// forgot to filter on __component would move cta's text too, which no error
// would ever surface.
func TestDynamicZone_SubFieldRenameAndDeleteReachZoneItems(t *testing.T) {
	svc, repo, ctx := seedZone(t)
	page, post := seedZoneEntries(t, svc, ctx)

	_, err := svc.RenameComponentField(ctx, "hero", "body", RenameInput{Key: "prose"})
	require.NoError(t, err)
	for _, e := range repo.entries {
		for _, raw := range []json.RawMessage{e.Payload, e.PublishedPayload} {
			doc := docOf(t, raw)
			switch e.ID {
			case page.ID:
				items := doc["zone"].([]any)
				hero, cta := items[0].(map[string]any), items[1].(map[string]any)
				assert.Equal(t, "hero body", hero["prose"])
				assert.NotContains(t, hero, "body")
				assert.Equal(t, "cta body", cta["body"], "the other component's colliding key is untouched")
			case post.ID:
				assert.Equal(t, "other hero body", doc["sections"].([]any)[0].(map[string]any)["prose"],
					"the SECOND referring type is rewritten too")
			}
		}
	}

	// The has-data guard counts zone items, so the delete is refused first.
	_, err = svc.DeleteComponentField(ctx, "hero", "prose", false)
	ae := mustCode(t, err, "CONTENT_FIELD_HAS_DATA", 409)
	assert.EqualValues(t, 2, ae.Details["entries"], "both referring types' entries are counted")

	_, err = svc.DeleteComponentField(ctx, "hero", "prose", true)
	require.NoError(t, err)
	for _, e := range repo.entries {
		for _, raw := range []json.RawMessage{e.Payload, e.PublishedPayload} {
			doc := docOf(t, raw)
			if e.ID == page.ID {
				items := doc["zone"].([]any)
				assert.NotContains(t, items[0].(map[string]any), "prose")
				assert.Equal(t, "cta body", items[1].(map[string]any)["body"])
			}
		}
	}
}

// TestDynamicZone_RequiredSubFieldBackfillCountsZoneItems is item 5: the
// cross-type gates fan out over zone referrers as well, and count only the
// items of the component being changed.
func TestDynamicZone_RequiredSubFieldBackfillCountsZoneItems(t *testing.T) {
	svc, _, ctx := seedZone(t)
	seedZoneEntries(t, svc, ctx)
	_, err := svc.AddComponentField(ctx, "hero", FieldInput{Key: "sub", Type: domain.FieldTypeString, Required: true})
	ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED_BACKFILL", 409)
	assert.EqualValues(t, 2, ae.Details["entries"])
	// cta appears in ONE entry, and only in the page zone, so a required
	// sub-field there is a backfill of one — not of every entry that happens
	// to hold a zone.
	_, err = svc.AddComponentField(ctx, "cta", FieldInput{Key: "sub", Type: domain.FieldTypeString, Required: true})
	ae = mustCode(t, err, "CONTENT_FIELD_REQUIRED_BACKFILL", 409)
	assert.EqualValues(t, 1, ae.Details["entries"])
}

func fieldDTOByKey(ct ContentTypeDTO, key string) (FieldDTO, bool) {
	for _, f := range ct.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return FieldDTO{}, false
}
