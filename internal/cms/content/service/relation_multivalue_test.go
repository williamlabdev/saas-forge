package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// countingTypeRepo counts how many times a related type is resolved. The
// dedup it guards is not observable from the response — a write that resolves
// the same type four times returns exactly what one that resolves it once
// returns — so nothing but the call count can hold it.
type countingTypeRepo struct {
	*memRepo
	byName map[string]int
}

func (r *countingTypeRepo) GetContentTypeByName(ctx context.Context, tenantID, name string) (*domain.ContentType, error) {
	if r.byName == nil {
		r.byName = map[string]int{}
	}
	r.byName[name]++
	return r.memRepo.GetContentTypeByName(ctx, tenantID, name)
}

// relationFixture builds `author` with two entries, plus a `post` type whose
// relation fields are supplied by the caller.
func relationFixture(t *testing.T, svc ContentService, ctx context.Context, postFields ...FieldInput) (string, string) {
	t.Helper()
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name: "author", Label: "Author",
		Fields: []FieldInput{{Key: "name", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)
	a1, err := svc.CreateEntry(ctx, "author", mustJSON(t, map[string]any{"name": "one"}))
	require.NoError(t, err)
	a2, err := svc.CreateEntry(ctx, "author", mustJSON(t, map[string]any{"name": "two"}))
	require.NoError(t, err)

	fields := append([]FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}}, postFields...)
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "post", Label: "Post", Fields: fields})
	require.NoError(t, err)
	return a1.ID.String(), a2.ID.String()
}

// `multiple` is legal on relation (domain.AllowedMultipleTypes), so the field
// could be declared — but checkRelations read every relation value as a bare
// string, so an array yielded "" and the write was refused as a malformed uuid.
// The field was declarable and permanently unwritable, and no test noticed
// because the multi-value suite is all refusals that fire before the repository
// is reached.
//
// Referential integrity has to survive the fix: the point of checking each
// element is that a bad id in position 2 is still caught. A version that simply
// stopped inspecting arrays would pass "two real ids" and fail here.
func TestMultiValuedRelation_WritesAndStillChecksEveryElement(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	a1, a2 := relationFixture(t, svc, ctx,
		FieldInput{Key: "authors", Type: domain.FieldTypeRelation, RelationEntity: "author", Multiple: true})

	t.Run("real ids write and round-trip in order", func(t *testing.T) {
		got, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
			"title": "T", "authors": []any{a1, a2},
		}))
		require.NoError(t, err)
		assert.JSONEq(t, `{"title":"T","authors":["`+a1+`","`+a2+`"]}`, string(got.Data))
	})

	t.Run("an id that does not exist is refused, with its position", func(t *testing.T) {
		missing := "6f1a4b6e-0000-4000-8000-000000000000"
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
			"title": "T", "authors": []any{a1, missing},
		}))
		assert.Equal(t, "CONTENT_RELATION_NOT_FOUND", codeOf(t, err))
		d := detailsOf(t, err)
		assert.Equal(t, 1, d["index"])
		assert.Equal(t, missing, d["value"])
	})

	t.Run("a malformed uuid is refused, with its position", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
			"title": "T", "authors": []any{a1, "not-a-uuid"},
		}))
		assert.Equal(t, "CONTENT_RELATION_INVALID", codeOf(t, err))
		assert.Equal(t, 1, detailsOf(t, err)["index"])
	})

	t.Run("an empty array names nobody and is accepted", func(t *testing.T) {
		got, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
			"title": "T", "authors": []any{},
		}))
		require.NoError(t, err)
		assert.Contains(t, string(got.Data), `"authors":[]`)
	})
}

// The scalar shape is the one that already worked, so the fix has to leave it
// exactly as it was — including the absence of an "index" detail, which on a
// field that can hold only one value would read as "the first of several".
func TestScalarRelation_UnchangedAndUnindexed(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	a1, _ := relationFixture(t, svc, ctx,
		FieldInput{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"})

	got, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "author": a1}))
	require.NoError(t, err)
	assert.Contains(t, string(got.Data), a1)

	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
		"title": "T", "author": "6f1a4b6e-0000-4000-8000-000000000000",
	}))
	assert.Equal(t, "CONTENT_RELATION_NOT_FOUND", codeOf(t, err))
	_, hasIndex := detailsOf(t, err)["index"]
	assert.False(t, hasIndex, "a scalar relation error must not claim an element position")
}

// Two fields naming the same type, one of them multi-valued: four relation
// values in total, all resolving to `author`. The type is loaded once.
//
// The three counts are distinguishable, which is what makes this test worth
// having: 4 means resolution happens per VALUE, 2 means per FIELD (the old
// behaviour), 1 means per TYPE. The values are distinct because a multi-valued
// field refuses repeats — deduping identical ids would be a different property
// from the one under test here.
func TestRelationCheck_ResolvesEachRelatedTypeOnce(t *testing.T) {
	repo := &countingTypeRepo{memRepo: &memRepo{}}
	svc := NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	ctx := ctxTenant("tenant-a")
	a1, a2 := relationFixture(t, svc, ctx,
		FieldInput{Key: "authors", Type: domain.FieldTypeRelation, RelationEntity: "author", Multiple: true},
		FieldInput{Key: "reviewer", Type: domain.FieldTypeRelation, RelationEntity: "author"})
	a3, err := svc.CreateEntry(ctx, "author", mustJSON(t, map[string]any{"name": "three"}))
	require.NoError(t, err)

	repo.byName = map[string]int{}
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{
		"title": "T", "authors": []any{a1, a2, a3.ID.String()}, "reviewer": a2,
	}))
	require.NoError(t, err)

	assert.Equal(t, 1, repo.byName["author"], "related type resolved %d times, want 1", repo.byName["author"])
}
