package repository

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// A field's description (migration 000046) against a real database. The
// memory repository in service/ cannot prove any of this: it hands back the
// same struct it was given, so a column that was never added, never selected
// or never updated reads back correct there and empty here.
//
// One table holds both kinds of field (000044), so both owners are exercised —
// a content type's field and a component's sub-field — because they travel
// through DIFFERENT insert, select and update statements.
func TestFieldDescription_RoundTripsForBothOwners(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "field_description")
	repo := NewPostgresContentRepository(pool, nil)
	tenant := "t-description"

	ct := &domain.ContentType{
		ID: uuid.New(), TenantID: tenant, Name: "page", Label: "Page",
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	slug := mkField(ct.ID, "slug", domain.FieldTypeString, 0)
	slug.Description = "Lower-case, dashes for spaces. Appears in the URL."
	slug.FieldConstraints = domain.FieldConstraints{Format: domain.FieldFormatSlug, Max: ptrF(80)}
	plain := mkField(ct.ID, "title", domain.FieldTypeString, 1)
	ct.Fields = []domain.Field{slug, plain}
	require.NoError(t, repo.CreateContentType(ctx, ct))

	loaded, err := repo.GetContentTypeByName(ctx, tenant, "page")
	require.NoError(t, err)
	require.Len(t, loaded.Fields, 2)
	assert.Equal(t, slug.Description, loaded.Fields[0].Description, "description did not survive insert + select")
	// A field written without one reads back as "" rather than NULL — the
	// column is NOT NULL DEFAULT '', so no caller has to handle a nil.
	assert.Equal(t, "", loaded.Fields[1].Description)

	// Rewriting and CLEARING both go through UPDATE, and clearing is the half
	// a mistyped SET clause still passes: writing only non-empty values would
	// leave the old sentence in place forever.
	edited := loaded.Fields[0]
	edited.Description = "The URL segment."
	require.NoError(t, repo.UpdateFieldDefinition(ctx, tenant, loaded, edited, baseTime))
	loaded, err = repo.GetContentTypeByName(ctx, tenant, "page")
	require.NoError(t, err)
	assert.Equal(t, "The URL segment.", loaded.Fields[0].Description)

	edited = loaded.Fields[0]
	edited.Description = ""
	require.NoError(t, repo.UpdateFieldDefinition(ctx, tenant, loaded, edited, baseTime))
	loaded, err = repo.GetContentTypeByName(ctx, tenant, "page")
	require.NoError(t, err)
	assert.Equal(t, "", loaded.Fields[0].Description, "an emptied description was not persisted")

	// The component half: a different INSERT, a different SELECT and a
	// different UPDATE, all of which had to learn the column separately.
	seo := mkComponent(t, ctx, repo, tenant, "seo", [2]string{"title", "string"})
	sub := seo.Fields[0]
	sub.Description = "Shown in search results."
	require.NoError(t, repo.UpdateComponentFieldDefinition(ctx, tenant, seo, sub, baseTime))
	got, err := repo.GetComponentByName(ctx, tenant, "seo")
	require.NoError(t, err)
	require.Len(t, got.Fields, 1)
	assert.Equal(t, "Shown in search results.", got.Fields[0].Description)

	withDesc := &domain.Component{
		ID: uuid.New(), TenantID: tenant, Name: "hero", Label: "Hero",
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	heading := mkField(uuid.Nil, "heading", domain.FieldTypeString, 0)
	heading.ContentTypeID = uuid.Nil
	heading.Description = "One line, no full stop."
	withDesc.Fields = []domain.Field{heading}
	require.NoError(t, repo.CreateComponent(ctx, withDesc))
	got, err = repo.GetComponentByName(ctx, tenant, "hero")
	require.NoError(t, err)
	require.Len(t, got.Fields, 1)
	assert.Equal(t, "One line, no full stop.", got.Fields[0].Description, "description did not survive the sub-field insert")
}
