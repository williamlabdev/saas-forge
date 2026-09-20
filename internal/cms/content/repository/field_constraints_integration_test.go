package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The unique ledger (migration 000040) is what makes `unique` a promise rather
// than a check: the primary key on (field, locale, value) is the only thing two
// concurrent writers cannot both get past. These tests pin its semantics —
// per locale, across the working copy AND the live snapshot, released by an
// unpublish or a delete — through the repository verbs, on a real database.

func ledgerRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM entry_unique_values WHERE "+where, args...).Scan(&n))
	return n
}

func mustAppCode(t *testing.T, err error, code string, status int) *apperrors.AppError {
	t.Helper()
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok, "not an AppError: %v", err)
	assert.Equal(t, code, ae.Code)
	assert.Equal(t, status, ae.HTTPStatus)
	return ae
}

func TestFieldConstraints_UniqueLedger(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "field_constraints")
	repo := NewPostgresContentRepository(pool, nil)
	tenant := "t-unique"
	editor := uuid.New()
	human := domain.ActorKindHuman

	ct := &domain.ContentType{
		ID: uuid.New(), TenantID: tenant, Name: "page", Label: "Page",
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	slug := mkField(ct.ID, "slug", domain.FieldTypeString, 0)
	slug.FieldConstraints = domain.FieldConstraints{Unique: true, Format: domain.FieldFormatSlug, Min: ptrF(1), Max: ptrF(80)}
	code := mkField(ct.ID, "code", domain.FieldTypeString, 1)
	code.FieldConstraints = domain.FieldConstraints{Pattern: `[a-z]+`}
	ct.Fields = []domain.Field{slug, code}
	require.NoError(t, repo.CreateContentType(ctx, ct))

	newEntry := func(locale string, payload map[string]any) *domain.Entry {
		raw, _ := json.Marshal(payload)
		return &domain.Entry{
			ID: uuid.New(), TenantID: tenant, ContentTypeID: ct.ID,
			Payload: raw, Version: 1, Status: domain.StatusDraft, Locale: locale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &editor, UpdatedBy: &editor, UpdatedByKind: &human,
			CreatedAt: baseTime, UpdatedAt: baseTime,
		}
	}
	setSlug := func(t *testing.T, e *domain.Entry, v string) error {
		t.Helper()
		e.Payload, _ = json.Marshal(map[string]any{"slug": v})
		e.UpdatedAt = baseTime.Add(time.Minute)
		return repo.UpdateEntry(ctx, e)
	}

	t.Run("the attributes round-trip through the field columns", func(t *testing.T) {
		got, err := repo.GetContentTypeByName(ctx, tenant, "page")
		require.NoError(t, err)
		require.Len(t, got.Fields, 2)
		assert.True(t, got.Fields[0].Unique)
		assert.Equal(t, domain.FieldFormatSlug, got.Fields[0].Format)
		assert.Equal(t, 1.0, *got.Fields[0].Min)
		assert.Equal(t, 80.0, *got.Fields[0].Max)
		assert.False(t, got.Fields[1].Unique)
		assert.Equal(t, `[a-z]+`, got.Fields[1].Pattern)
		assert.Nil(t, got.Fields[1].Min)
	})

	a := newEntry(domain.DefaultLocale, map[string]any{"slug": "x", "code": "aa"})
	require.NoError(t, repo.CreateEntry(ctx, a))

	t.Run("a second entry cannot take a reserved value in the same locale", func(t *testing.T) {
		b := newEntry(domain.DefaultLocale, map[string]any{"slug": "x"})
		ae := mustAppCode(t, repo.CreateEntry(ctx, b), "CONTENT_FIELD_VALUE_TAKEN", 409)
		assert.Equal(t, "slug", ae.Details["field"])
		assert.Equal(t, domain.DefaultLocale, ae.Details["locale"])
		assert.Equal(t, "x", ae.Details["value"])
		assert.Equal(t, a.ID.String(), fmt.Sprint(ae.Details["entry"]))
		// The failed insert left nothing behind — not an entry, not a ledger row.
		assert.Equal(t, 0, ledgerRows(t, ctx, pool, "entry_id = $1", b.ID))
		_, err := repo.GetEntry(ctx, tenant, ct.ID, b.ID)
		assert.ErrorIs(t, err, apperrors.ErrNotFound)
	})
	t.Run("but may take it in another locale", func(t *testing.T) {
		fr := newEntry("fr", map[string]any{"slug": "x"})
		require.NoError(t, repo.CreateEntry(ctx, fr))
		assert.Equal(t, 1, ledgerRows(t, ctx, pool, "entry_id = $1", fr.ID))
	})
	t.Run("a non-unique field reserves nothing", func(t *testing.T) {
		c := newEntry(domain.DefaultLocale, map[string]any{"slug": "c-slug", "code": "aa"})
		require.NoError(t, repo.CreateEntry(ctx, c), "code is not unique; aa is fine twice")
		assert.Equal(t, 1, ledgerRows(t, ctx, pool, "entry_id = $1", c.ID))
		require.NoError(t, repo.DeleteEntry(ctx, tenant, ct.ID, c.ID))
	})

	t.Run("the live snapshot keeps its value reserved after the working copy moves on", func(t *testing.T) {
		published := baseTime.Add(time.Hour)
		require.NoError(t, repo.SetEntryPublishState(ctx, a, domain.StatusPublished, &published))
		require.NoError(t, setSlug(t, a, "y"))
		assert.Equal(t, 2, ledgerRows(t, ctx, pool, "entry_id = $1", a.ID), "x (live) and y (working)")

		c := newEntry(domain.DefaultLocale, map[string]any{"slug": "x"})
		mustAppCode(t, repo.CreateEntry(ctx, c), "CONTENT_FIELD_VALUE_TAKEN", 409)
		c.Payload, _ = json.Marshal(map[string]any{"slug": "y"})
		mustAppCode(t, repo.CreateEntry(ctx, c), "CONTENT_FIELD_VALUE_TAKEN", 409)

		// Re-publishing moves the snapshot to y and releases x.
		require.NoError(t, repo.SetEntryPublishState(ctx, a, domain.StatusPublished, &published))
		assert.Equal(t, 1, ledgerRows(t, ctx, pool, "entry_id = $1", a.ID))
		c.Payload, _ = json.Marshal(map[string]any{"slug": "x"})
		require.NoError(t, repo.CreateEntry(ctx, c))

		// And an unpublish releases the snapshot's value even though the
		// snapshot itself is retained (ADR-014 §5.1).
		require.NoError(t, setSlug(t, a, "z"))
		assert.Equal(t, 2, ledgerRows(t, ctx, pool, "entry_id = $1", a.ID))
		require.NoError(t, repo.SetEntryPublishState(ctx, a, domain.StatusDraft, nil))
		assert.Equal(t, 1, ledgerRows(t, ctx, pool, "entry_id = $1", a.ID))
		d := newEntry(domain.DefaultLocale, map[string]any{"slug": "y"})
		require.NoError(t, repo.CreateEntry(ctx, d))

		// ScanFieldValues reports the same picture the ledger holds: a's
		// working value, and no live value once it is retracted.
		seen := map[uuid.UUID][2]any{}
		require.NoError(t, repo.ScanFieldValues(ctx, tenant, ct.ID, "slug", func(id uuid.UUID, working, live any) error {
			seen[id] = [2]any{working, live}
			return nil
		}))
		assert.Equal(t, [2]any{"z", nil}, seen[a.ID])
		assert.Equal(t, [2]any{"x", nil}, seen[c.ID])

		// Taking a value back that another entry now holds is refused on
		// update exactly as on create, and the refused update lands nothing.
		ae := mustAppCode(t, setSlug(t, a, "y"), "CONTENT_FIELD_VALUE_TAKEN", 409)
		assert.Equal(t, d.ID.String(), fmt.Sprint(ae.Details["entry"]))
		got, err := repo.GetEntry(ctx, tenant, ct.ID, a.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"slug":"z"}`, string(got.Payload))

		// Deleting the holder releases the value.
		require.NoError(t, repo.DeleteEntry(ctx, tenant, ct.ID, d.ID))
		assert.Equal(t, 0, ledgerRows(t, ctx, pool, "entry_id = $1", d.ID))
		a.Version = got.Version
		require.NoError(t, setSlug(t, a, "y"))
	})

	t.Run("switching unique on scans the stored values and refuses duplicates", func(t *testing.T) {
		// a lost its code when setSlug rewrote its working copy; give two fresh entries the same code.
		e1 := newEntry(domain.DefaultLocale, map[string]any{"slug": "e-one", "code": "dup"})
		e2 := newEntry(domain.DefaultLocale, map[string]any{"slug": "e-two", "code": "dup"})
		e3 := newEntry(domain.DefaultLocale, map[string]any{"slug": "e-three", "code": ""})
		e4 := newEntry(domain.DefaultLocale, map[string]any{"slug": "e-four"})
		for _, e := range []*domain.Entry{e1, e2, e3, e4} {
			require.NoError(t, repo.CreateEntry(ctx, e))
		}
		dups, err := repo.ListDuplicateFieldValues(ctx, tenant, ct.ID, code)
		require.NoError(t, err)
		require.Len(t, dups, 1)
		assert.Equal(t, DuplicateValue{Locale: domain.DefaultLocale, Value: "dup", Entries: 2}, dups[0])

		on := code
		on.Unique = true
		ae := mustAppCode(t, repo.UpdateFieldDefinition(ctx, tenant, ct, on, baseTime.Add(time.Hour)), "CONTENT_FIELD_UNIQUE_DUPLICATES", 409)
		assert.Equal(t, "code", ae.Details["field"])
		got, err := repo.GetContentTypeByName(ctx, tenant, "page")
		require.NoError(t, err)
		assert.False(t, got.Fields[1].Unique, "the refused flip must not land")
		assert.Equal(t, 0, ledgerRows(t, ctx, pool, "field_id = $1", code.ID))

		// Distinct now: the flip backfills the ledger from BOTH copies, skipping
		// empty and absent values, which reserve nothing.
		e2.Payload, _ = json.Marshal(map[string]any{"slug": "e-two", "code": "other"})
		e2.UpdatedAt = baseTime.Add(time.Minute)
		require.NoError(t, repo.UpdateEntry(ctx, e2))
		require.NoError(t, repo.UpdateFieldDefinition(ctx, tenant, ct, on, baseTime.Add(time.Hour)))
		got, err = repo.GetContentTypeByName(ctx, tenant, "page")
		require.NoError(t, err)
		assert.True(t, got.Fields[1].Unique)
		assert.Equal(t, 2, ledgerRows(t, ctx, pool, "field_id = $1", code.ID), "dup and other; a lost its code when its working copy was rewritten and its snapshot is retracted")

		// From here the ledger is live for code too.
		e5 := newEntry(domain.DefaultLocale, map[string]any{"slug": "e-five", "code": "dup"})
		mustAppCode(t, repo.CreateEntry(ctx, e5), "CONTENT_FIELD_VALUE_TAKEN", 409)
		e5.Payload, _ = json.Marshal(map[string]any{"slug": "e-five", "code": ""})
		require.NoError(t, repo.CreateEntry(ctx, e5), "empty strings are never reserved")

		// Switching it off drops the reservations and nothing else.
		off := code
		off.Unique = false
		require.NoError(t, repo.UpdateFieldDefinition(ctx, tenant, ct, off, baseTime.Add(2*time.Hour)))
		assert.Equal(t, 0, ledgerRows(t, ctx, pool, "field_id = $1", code.ID))
		assert.NotEqual(t, 0, ledgerRows(t, ctx, pool, "field_id = $1", slug.ID), "slug's reservations are untouched")
	})

	t.Run("the other attributes are updatable in place", func(t *testing.T) {
		next := slug
		next.Min, next.Max, next.Format, next.Pattern = ptrF(2), nil, "", `[a-z-]+`
		require.NoError(t, repo.UpdateFieldDefinition(ctx, tenant, ct, next, baseTime.Add(3*time.Hour)))
		got, err := repo.GetContentTypeByName(ctx, tenant, "page")
		require.NoError(t, err)
		assert.Equal(t, 2.0, *got.Fields[0].Min)
		assert.Nil(t, got.Fields[0].Max)
		assert.Empty(t, got.Fields[0].Format)
		assert.Equal(t, `[a-z-]+`, got.Fields[0].Pattern)
		assert.True(t, got.Fields[0].Unique, "unique is untouched by a rule change")
	})

	t.Run("the check constraints refuse what the domain refuses", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE content_type_fields SET format = 'email' WHERE id = $1`, slug.ID)
		assert.ErrorContains(t, err, "content_type_fields_format_check")
		_, err = pool.Exec(ctx, `UPDATE content_type_fields SET min_value = 5, max_value = 1 WHERE id = $1`, slug.ID)
		assert.ErrorContains(t, err, "content_type_fields_range_check")
	})

	t.Run("deleting the type takes its reservations with it", func(t *testing.T) {
		before := ledgerRows(t, ctx, pool, "tenant_id = $1", tenant)
		require.NotZero(t, before)
		_, err := pool.Exec(ctx, `DELETE FROM content_types WHERE id = $1`, ct.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, ledgerRows(t, ctx, pool, "tenant_id = $1", tenant))
	})
}

func ptrF(v float64) *float64 { return &v }
