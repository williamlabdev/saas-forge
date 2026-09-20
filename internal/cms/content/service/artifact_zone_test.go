package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// Apply, for a dynamic zone's allowed-list (cross-cutting item 6). The
// grading is decided in the domain (artifact_zone_test.go there); what is
// asserted HERE is the half only the service can answer — how many entries
// stand in the way of a removal, and whether prune is required.

func zoneComponentArtifact(allowed ...string) domain.Artifact {
	return domain.Artifact{
		ArtifactVersion: domain.ArtifactVersion1, Kind: domain.KindContentSchema,
		Components: []domain.ArtifactComponent{
			{Name: "cta", Label: "CTA", Fields: []domain.ArtifactField{
				{Key: "label", Type: domain.FieldTypeString}}},
			{Name: "hero", Label: "Hero", Fields: []domain.ArtifactField{
				{Key: "headline", Type: domain.FieldTypeString}}},
		},
		Types: []domain.ArtifactType{{Name: "page", Label: "Page", Fields: []domain.ArtifactField{
			titleField,
			{Key: "zone", Type: domain.FieldTypeDynamicZone, Label: "Zone", Components: allowed},
		}}},
	}
}

func TestApplySchema_ZoneRoundTripsAndGrowsFreely(t *testing.T) {
	svc, ctx := applySvc(t)
	base := zoneComponentArtifact("hero", "cta")
	res, err := svc.ApplySchema(ctx, base, false)
	require.NoError(t, err)
	require.Equal(t, 3, res.Applicable, "two components, then the type that names them")

	// The exporter has to carry the allowed-list, in order, or every artifact
	// in git is a permanent diff. Round-tripping through the EXPORT is what
	// catches a property the writer drops on the way out.
	exported, err := svc.ExportSchema(ctx)
	require.NoError(t, err)
	plan, err := svc.PlanSchema(ctx, exported, false)
	require.NoError(t, err)
	require.Empty(t, plan.Steps, "the exported artifact does not describe the zone it came from: %+v", plan.Steps)

	// Adding is free — nothing stored can be invalidated by a wider list — and
	// it takes effect: an item of the new component validates immediately.
	wider := zoneComponentArtifact("hero", "cta", "quote")
	wider.Components = append(wider.Components, domain.ArtifactComponent{
		Name: "quote", Label: "Quote", Fields: []domain.ArtifactField{{Key: "text", Type: domain.FieldTypeString}}})
	res, err = svc.ApplySchema(ctx, wider, false)
	require.NoError(t, err)
	require.Zero(t, res.Refused+res.Blocked)
	_, err = svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{
		"title": "p", "zone": []any{zoneItem("quote", map[string]any{"text": "hi"})}}))
	require.NoError(t, err, "the added component is usable without a second write")
}

func TestApplySchema_ZoneRemovalIsBlockedByStoredItems(t *testing.T) {
	svc, ctx := applySvc(t)
	base := zoneComponentArtifact("hero", "cta")
	_, err := svc.ApplySchema(ctx, base, false)
	require.NoError(t, err)

	// With no data, a removal is graded guarded but nothing blocks it, so it
	// applies. Guarded means "ask the database", not "require prune".
	narrower := zoneComponentArtifact("hero")
	plan, err := svc.PlanSchema(ctx, narrower, false)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.Equal(t, "CONTENT_ZONE_COMPONENT_IN_USE", plan.Steps[0].Code)
	assert.False(t, plan.Steps[0].Blocked)
	assert.Zero(t, plan.Steps[0].Entries)

	// Put an item of each component in, and the same removal is blocked with
	// the number of entries an editor has to migrate.
	_, err = svc.ApplySchema(ctx, base, false)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{
			"title": "p", "zone": []any{zoneItem("cta", map[string]any{"label": "go"})}}))
		require.NoError(t, err)
	}
	_, err = svc.CreateEntry(ctx, "page", mustJSON(t, map[string]any{
		"title": "h", "zone": []any{zoneItem("hero", map[string]any{"headline": "x"})}}))
	require.NoError(t, err)

	plan, err = svc.PlanSchema(ctx, narrower, false)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.True(t, plan.Steps[0].Blocked)
	assert.Equal(t, 2, plan.Steps[0].Entries, "only the entries holding a cta ITEM count")
	assert.Equal(t, 1, plan.Blocked)

	// Prune does NOT unblock it: prune authorises DELETIONS the document is
	// silent about, and this document is not silent — it says the component is
	// no longer accepted while entries still hold items of it.
	_, err = svc.ApplySchema(ctx, narrower, true)
	mustCode(t, err, "CONTENT_SCHEMA_NOT_APPLICABLE", 409)
	dto, err := svc.GetContentType(ctx, "page")
	require.NoError(t, err)
	f, ok := fieldDTOByKey(dto, "zone")
	require.True(t, ok)
	assert.Equal(t, []string{"hero", "cta"}, f.Components, "a blocked plan changed the schema anyway")
}

// Deleting a component and dropping it from the zone that lists it is ONE
// artifact, and the order the diff emits (the field update first, the
// component delete last) is what makes it applicable at all.
func TestApplySchema_ZoneAndComponentDeleteInOneDocument(t *testing.T) {
	svc, ctx := applySvc(t)
	_, err := svc.ApplySchema(ctx, zoneComponentArtifact("hero", "cta"), false)
	require.NoError(t, err)

	// Dropping the component while the zone still names it is refused: the
	// list would name something that does not exist.
	orphan := zoneComponentArtifact("hero", "cta")
	orphan.Components = orphan.Components[1:] // drop "cta"
	plan, err := svc.PlanSchema(ctx, orphan, true)
	require.NoError(t, err)
	require.NotZero(t, plan.Blocked+plan.Refused, "a zone naming a deleted component must not apply: %+v", plan.Steps)

	// Doing both in the same document is fine, and needs prune only for the
	// component DELETE — the allowed-list edit is a change the document states.
	both := zoneComponentArtifact("hero")
	both.Components = both.Components[1:]
	res, err := svc.ApplySchema(ctx, both, true)
	require.NoError(t, err)
	require.Zero(t, res.Refused+res.Blocked)
	comps, err := svc.ListComponents(ctx)
	require.NoError(t, err)
	require.Len(t, comps, 1)
	assert.Equal(t, "hero", comps[0].Name)
	dto, err := svc.GetContentType(ctx, "page")
	require.NoError(t, err)
	f, ok := fieldDTOByKey(dto, "zone")
	require.True(t, ok)
	assert.Equal(t, []string{"hero"}, f.Components)
}

// Prune semantics for the FIELD, unchanged from #69: an artifact that simply
// stops mentioning a zone field is silent about it, not asking for its
// deletion.
func TestApplySchema_DroppingAZoneFieldWaitsForPrune(t *testing.T) {
	svc, ctx := applySvc(t)
	_, err := svc.ApplySchema(ctx, zoneComponentArtifact("hero", "cta"), false)
	require.NoError(t, err)

	trimmed := zoneComponentArtifact("hero", "cta")
	trimmed.Types[0].Fields = trimmed.Types[0].Fields[:1]

	plan, err := svc.PlanSchema(ctx, trimmed, false)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.True(t, plan.Steps[0].Skipped)
	_, err = svc.ApplySchema(ctx, trimmed, false)
	require.NoError(t, err)
	dto, err := svc.GetContentType(ctx, "page")
	require.NoError(t, err)
	require.Len(t, dto.Fields, 2, "apply removed a zone field prune had not authorised")

	_, err = svc.ApplySchema(ctx, trimmed, true)
	require.NoError(t, err)
	dto, err = svc.GetContentType(ctx, "page")
	require.NoError(t, err)
	require.Len(t, dto.Fields, 1)
	// The components stay: the document still declares them.
	comps, err := svc.ListComponents(ctx)
	require.NoError(t, err)
	require.Len(t, comps, 2)
}
