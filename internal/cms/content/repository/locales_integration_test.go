package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ListLocales (T2 2.3) against the real thing, for the two claims memRepo's
// fake in content_service_test.go cannot answer on its own:
//
//   - the GROUP BY actually groups and counts correctly across content types
//     in one tenant, ordered by locale — memRepo's fake re-implements this in
//     Go, which proves nothing about whether the SQL does the same thing.
//   - dataVisibleExpr, shared with SearchEntries and ListPendingReview, is
//     wired into THIS query's WHERE clause and actually hides a restricted
//     type's rows (and a confined author's colleagues' rows) from the count —
//     see crosstype_data_permission_integration_test.go's header for why this
//     needs the real predicate rather than memRepo's own viewerMaySee, which
//     would pass even if the SQL's EXISTS clause were dropped entirely.
func TestListLocalesGroupsCountsAndAppliesDataPermission(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("listlocales"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("postgres container: %v", err)
	}
	defer func() { _ = container.Terminate(ctx) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, loadContentRLSMigrations(t))
	require.NoError(t, err, "migrate")

	repo := NewPostgresContentRepository(pool, nil)

	// Two content types in t1: "post" (open) and "secret" (read_roles
	// ["owner"], hidden from an editor). A third type "post" in t2 proves
	// tenant isolation the same way search_integration_test.go's otherType
	// does. "mine" (own_only_roles ["editor"]) is the confinement half of
	// dataVisibleExpr — read_roles hides a TYPE outright, own_only_roles hides
	// ROWS within a type a confined viewer can otherwise see, and the count
	// must reflect that distinction rather than treating both as "0 or all".
	postType, secretType, mineType := uuid.New(), uuid.New(), uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label, read_roles) VALUES ($1,'t1','post','',$2)`,
		postType, orEmptyRoles(nil))
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label, read_roles) VALUES ($1,'t1','secret','',$2)`,
		secretType, orEmptyRoles([]string{"owner"}))
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label, own_only_roles) VALUES ($1,'t1','mine','',$2)`,
		mineType, orEmptyRoles([]string{"editor"}))
	require.NoError(t, err)
	otherTenantType := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t2','post','')`, otherTenantType)
	require.NoError(t, err)

	editorID, ownerID := uuid.New(), uuid.New()

	insert := func(typeID uuid.UUID, tenant, locale string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO entries (id, tenant_id, content_type_id, payload, locale) VALUES ($1,$2,$3,'{}'::jsonb,$4)`,
			id, tenant, typeID, locale)
		require.NoError(t, err)
		return id
	}
	insertAs := func(typeID uuid.UUID, tenant, locale string, createdBy uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO entries (id, tenant_id, content_type_id, payload, locale, created_by) VALUES ($1,$2,$3,'{}'::jsonb,$4,$5)`,
			id, tenant, typeID, locale, createdBy)
		require.NoError(t, err)
		return id
	}

	// t1/post: two "default", one "fr" — GROUP BY must collapse the two
	// defaults into one row with count 2, not two rows or a count of 1.
	insert(postType, "t1", "default")
	insert(postType, "t1", "default")
	insert(postType, "t1", "fr")
	// t1/secret: a "de" entry that only an owner may count.
	insert(secretType, "t1", "de")
	// t1/mine: TWO "ja" rows, one per author — an unconfined viewer (owner)
	// must count both; a confined editor must count only their own. A third
	// "ko" row belongs to ownerID alone, so a confined editor's count for it
	// must be zero — the whole locale disappears from their list, not merely
	// shrinks.
	insertAs(mineType, "t1", "ja", editorID)
	insertAs(mineType, "t1", "ja", ownerID)
	insertAs(mineType, "t1", "ko", ownerID)
	// t2/post: same locale as t1, must never bleed into t1's counts.
	insert(otherTenantType, "t2", "default")

	t.Run("groups and counts across content types, ordered by locale, owner sees every type", func(t *testing.T) {
		rows, err := repo.ListLocales(ctx, ListLocalesFilter{TenantID: "t1", ViewerRole: "owner"})
		require.NoError(t, err)
		require.Len(t, rows, 5, "de, default, fr, ja, ko")
		assert.Equal(t, "de", rows[0].Locale)
		assert.Equal(t, 1, rows[0].Entries)
		assert.Equal(t, "default", rows[1].Locale)
		assert.Equal(t, 2, rows[1].Entries, "two entries share the default locale and must collapse into one row")
		assert.Equal(t, "fr", rows[2].Locale)
		assert.Equal(t, 1, rows[2].Entries)
		assert.Equal(t, "ja", rows[3].Locale)
		assert.Equal(t, 2, rows[3].Entries, "own_only_roles does not confine 'owner' — both authors' 'ja' rows count")
		assert.Equal(t, "ko", rows[4].Locale)
		assert.Equal(t, 1, rows[4].Entries)
	})

	t.Run("read_roles hides a restricted type's locale and count from an editor", func(t *testing.T) {
		rows, err := repo.ListLocales(ctx, ListLocalesFilter{TenantID: "t1", ViewerRole: "editor"})
		require.NoError(t, err)
		require.Len(t, rows, 2, "default and fr only — secret's 'de' row is invisible to an editor, and 'mine' has no rows at all for a zero-value ViewerUserID")
		for _, r := range rows {
			assert.NotEqual(t, "de", r.Locale, "read_roles ['owner'] must remove secret's locale from an editor's count entirely, not just its count")
		}
	})

	// own_only_roles is the OTHER half of dataVisibleExpr: it hides ROWS a
	// confined viewer did not author, not the whole type the way read_roles
	// does — so the count itself must shrink to what that viewer owns, and a
	// locale nobody-they-are hasn't touched must vanish from the list entirely
	// rather than show up with entries: 0.
	t.Run("own_only_roles confines the count to the viewer's own entries", func(t *testing.T) {
		rows, err := repo.ListLocales(ctx, ListLocalesFilter{TenantID: "t1", ViewerRole: "editor", ViewerUserID: editorID})
		require.NoError(t, err)
		require.Len(t, rows, 3, "default, fr, and ja (editor's own row only) — 'de' (read_roles) and 'ko' (colleague-only) are both absent")

		byLocale := map[string]int{}
		for _, r := range rows {
			byLocale[r.Locale] = r.Entries
		}
		assert.Equal(t, 2, byLocale["default"], "post has no own_only_roles at all, so an editor is not confined on it — both 'default' rows still count")
		assert.Equal(t, 1, byLocale["fr"])
		require.Contains(t, byLocale, "ja")
		assert.Equal(t, 1, byLocale["ja"], "two 'ja' rows exist under mine, but only the editor's own counts — the colleague's row must not inflate it to 2")
		assert.NotContains(t, byLocale, "ko", "'ko' belongs entirely to the other author; a confined editor's count for it is not 0, the locale itself is absent")
		assert.NotContains(t, byLocale, "de", "'de' stays hidden by read_roles regardless of confinement")
	})

	t.Run("content type filter narrows the count to one type", func(t *testing.T) {
		id := postType
		rows, err := repo.ListLocales(ctx, ListLocalesFilter{TenantID: "t1", ViewerRole: "owner", ContentTypeID: &id})
		require.NoError(t, err)
		require.Len(t, rows, 2, "post has default and fr, not secret's de")
		assert.Equal(t, "default", rows[0].Locale)
		assert.Equal(t, 2, rows[0].Entries)
		assert.Equal(t, "fr", rows[1].Locale)
	})

	t.Run("tenant isolation", func(t *testing.T) {
		rows, err := repo.ListLocales(ctx, ListLocalesFilter{TenantID: "t2", ViewerRole: "owner"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "default", rows[0].Locale)
		assert.Equal(t, 1, rows[0].Entries, "t1's two 'default' entries must not bleed into t2's count")
	})
}
