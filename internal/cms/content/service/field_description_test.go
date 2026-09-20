package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// A field's description end to end through the verbs (ADR-007 Amendment 3).
// It is documentation rather than a rule, so there is no guard to test and no
// backfill to count — what these pin is that it ARRIVES, that it can be
// cleared, and that clearing it is distinguishable from leaving it alone.

func TestFieldDescription_CreateCarriesItAndTrims(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")

	dto, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name: "page",
		Fields: []FieldInput{
			{Key: "slug", Type: domain.FieldTypeString, Description: "  Appears in the URL.  ",
				FieldConstraints: domain.FieldConstraints{Format: domain.FieldFormatSlug}},
			{Key: "title", Type: domain.FieldTypeString},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Appears in the URL.", dto.Fields[0].Description, "description is trimmed the way label is")
	assert.Equal(t, "", dto.Fields[1].Description)
}

func TestFieldDescription_PatchSetsClearsAndLeavesAlone(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:   "page",
		Fields: []FieldInput{{Key: "slug", Type: domain.FieldTypeString, Description: "Appears in the URL."}},
	})
	require.NoError(t, err)

	// Absent means "leave it" — the same bargain every other pointer on
	// UpdateFieldInput makes, and the one a plain string could not express.
	dto, err := svc.UpdateField(ctx, "page", "slug", UpdateFieldInput{Label: strPtr("Slug")})
	require.NoError(t, err)
	assert.Equal(t, "Appears in the URL.", dto.Fields[0].Description)

	dto, err = svc.UpdateField(ctx, "page", "slug", UpdateFieldInput{Description: strPtr("The URL segment.")})
	require.NoError(t, err)
	assert.Equal(t, "The URL segment.", dto.Fields[0].Description)

	// An explicit "" is a real instruction. Nothing guards it: no stored value
	// can be invalidated by deleting a sentence.
	dto, err = svc.UpdateField(ctx, "page", "slug", UpdateFieldInput{Description: strPtr("")})
	require.NoError(t, err)
	assert.Equal(t, "", dto.Fields[0].Description)
}

func TestFieldDescription_ComponentSubFieldCarriesIt(t *testing.T) {
	svc, _, ctx := seedSeo(t)

	dto, err := svc.AddComponentField(ctx, "seo", FieldInput{
		Key: "canonical", Type: domain.FieldTypeString, Description: "Absolute URL, https only.",
	})
	require.NoError(t, err)
	require.Equal(t, "canonical", dto.Fields[len(dto.Fields)-1].Key)
	assert.Equal(t, "Absolute URL, https only.", dto.Fields[len(dto.Fields)-1].Description)

	dto, err = svc.UpdateComponentField(ctx, "seo", "canonical", UpdateFieldInput{Description: strPtr("")})
	require.NoError(t, err)
	assert.Equal(t, "", dto.Fields[len(dto.Fields)-1].Description,
		"a sub-field's description could not be cleared — it travels through its own UPDATE")
}

// The artifact half: export carries it, and an apply of a file that DROPS the
// key clears the stored one. That direction is the one a "send it only when
// non-empty" shortcut would break, and it is the same rule the permission
// lists and the constraint values already follow — an artifact describes a
// state, so a missing sentence means "no description", not "leave the old one".
func TestFieldDescription_ArtifactRoundTripAndClear(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:   "page",
		Fields: []FieldInput{{Key: "slug", Type: domain.FieldTypeString, Description: "Appears in the URL."}},
	})
	require.NoError(t, err)

	art, err := svc.ExportSchema(ctx)
	require.NoError(t, err)
	require.Len(t, art.Types, 1)
	require.Len(t, art.Types[0].Fields, 1)
	require.Equal(t, "Appears in the URL.", art.Types[0].Fields[0].Description)

	// Re-applying the exported file is a no-op, which is what makes the diff
	// below about the description and nothing else.
	plan, err := svc.PlanSchema(ctx, art, false)
	require.NoError(t, err)
	assert.Empty(t, plan.Steps, "an unchanged artifact planned work: %+v", plan.Steps)

	art.Types[0].Fields[0].Description = ""
	plan, err = svc.PlanSchema(ctx, art, false)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.Equal(t, domain.GradeAdditive, plan.Steps[0].Grade, "a reworded description is not a guarded change")
	assert.Contains(t, plan.Steps[0].Detail, "description", "the plan did not say what changed")

	_, err = svc.ApplySchema(ctx, art, false)
	require.NoError(t, err)
	dto, err := svc.GetContentType(ctx, "page")
	require.NoError(t, err)
	assert.Equal(t, "", dto.Fields[0].Description, "the artifact dropped the description and the apply kept it")
}
