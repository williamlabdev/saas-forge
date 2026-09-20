package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// Reusable components (ADR-020) against a real Postgres: the set-based
// rewrite over every referrer in both payload copies, the FK that keeps a
// referenced component alive, RLS on the new table, and migration 000044's
// down/up symmetry. Values are inline JSON, so every assertion here reads the
// stored document back rather than trusting the repository's own reads.

func mkComponent(t *testing.T, ctx context.Context, repo *PostgresContentRepository, tenant, name string, fields ...[2]string) *domain.Component {
	t.Helper()
	c := &domain.Component{
		ID: uuid.New(), TenantID: tenant, Name: name, Label: name,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	for i, f := range fields {
		sf := mkField(uuid.Nil, f[0], f[1], i)
		sf.ContentTypeID = uuid.Nil
		if f[1] == domain.FieldTypeEnum {
			sf.EnumValues = []string{"article", "product"}
		}
		c.Fields = append(c.Fields, sf)
	}
	require.NoError(t, repo.CreateComponent(ctx, c), "create component %s", name)
	return c
}

// mkReferrer creates a type with one scalar field and one field embedding
// the component — as an object (multiple=false) or a list.
func mkReferrer(t *testing.T, ctx context.Context, repo *PostgresContentRepository, tenant, name, key string, comp *domain.Component, multiple bool) *domain.ContentType {
	t.Helper()
	ct := &domain.ContentType{
		ID: uuid.New(), TenantID: tenant, Name: name, Label: name,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	ref := mkField(ct.ID, key, domain.FieldTypeComponent, 1)
	id := comp.ID
	ref.ComponentID = &id
	ref.ComponentName = comp.Name
	ref.Multiple = multiple
	ct.Fields = []domain.Field{mkField(ct.ID, "title", domain.FieldTypeString, 0), ref}
	require.NoError(t, repo.CreateContentType(ctx, ct), "create type %s", name)
	return ct
}

func docOf(t *testing.T, raw *string) map[string]any {
	t.Helper()
	require.NotNil(t, raw)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(*raw), &doc))
	return doc
}

func revisionCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM entry_revisions WHERE entry_id = $1`, id).Scan(&n))
	return n
}

func TestComponents_RenameRewritesEveryReferrerInBothCopies(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "components_rename")
	repo := NewPostgresContentRepository(pool, nil)
	tenant := "tenant-a"
	seo := mkComponent(t, ctx, repo, tenant, "seo", [2]string{"title", "string"}, [2]string{"kind", "enum"})
	page := mkReferrer(t, ctx, repo, tenant, "page", "meta", seo, false)
	post := mkReferrer(t, ctx, repo, tenant, "post", "blocks", seo, true)

	pageIDs := seedEntries(t, ctx, pool, tenant, page.ID,
		// Both copies hold the sub-key: both are rewritten, both versions move.
		entrySeed{payload: `{"title":"p","meta":{"title":"draft","kind":"article"}}`, version: 3,
			publishedPayload: `{"title":"p","meta":{"title":"live","kind":"article"}}`, publishedVersion: 2},
		// No component value at all: untouched, version and updated_at stay.
		entrySeed{payload: `{"title":"bare"}`, version: 1},
	)
	postIDs := seedEntries(t, ctx, pool, tenant, post.ID,
		entrySeed{payload: `{"title":"q","blocks":[{"title":"one","kind":"product"},{"kind":"article"},"junk"]}`, version: 1,
			publishedPayload: `{"title":"q","blocks":[{"title":"one"}]}`, publishedVersion: 1},
	)
	bareBefore := readEntry(t, ctx, pool, pageIDs[1])
	revsBefore := revisionCount(t, ctx, pool, pageIDs[0])

	refs, err := repo.ListComponentReferrers(ctx, tenant, seo.ID)
	require.NoError(t, err)
	require.Equal(t, []ComponentRef{
		{TypeID: page.ID, TypeName: "page", FieldKey: "meta", Multiple: false},
		{TypeID: post.ID, TypeName: "post", FieldKey: "blocks", Multiple: true},
	}, refs, "referrers are every field of type component that points here, in type/key order")

	now := baseTime.Add(time.Hour)
	require.NoError(t, repo.RenameComponentField(ctx, tenant, seo, refs, "title", "headline", schemaAdmin, now))

	// page: the object under meta is rewritten in both copies; the TYPE's own
	// `title` key is not, because the rewrite descends into the field value.
	row := readEntry(t, ctx, pool, pageIDs[0])
	work := docOf(t, &row.payload)
	assert.Equal(t, "p", work["title"])
	assert.Equal(t, map[string]any{"headline": "draft", "kind": "article"}, work["meta"])
	live := docOf(t, row.publishedPayload)
	assert.Equal(t, map[string]any{"headline": "live", "kind": "article"}, live["meta"])
	assert.Equal(t, 4, row.version, "working copy changed: version moves")
	require.NotNil(t, row.publishedVersion)
	assert.Equal(t, 3, *row.publishedVersion, "published copy changed: its version moves too")
	assert.Equal(t, revsBefore+1, revisionCount(t, ctx, pool, pageIDs[0]), "the rewrite is a revision with provenance")

	// The entry without the value is not touched at all.
	bare := readEntry(t, ctx, pool, pageIDs[1])
	assert.Equal(t, bareBefore, bare)

	// post: every object in the list is rewritten, non-object elements and
	// items without the key are left as they were.
	row = readEntry(t, ctx, pool, postIDs[0])
	work = docOf(t, &row.payload)
	assert.Equal(t, []any{
		map[string]any{"headline": "one", "kind": "product"},
		map[string]any{"kind": "article"},
		"junk",
	}, work["blocks"])
	live = docOf(t, row.publishedPayload)
	assert.Equal(t, []any{map[string]any{"headline": "one"}}, live["blocks"])
	assert.Equal(t, 2, row.version)

	// The definition follows, and both referring types are touched so their
	// DTOs (which render the sub-fields) invalidate.
	got, err := repo.GetComponentByName(ctx, tenant, "seo")
	require.NoError(t, err)
	require.Len(t, got.Fields, 2)
	assert.Equal(t, "headline", got.Fields[0].Key)
	assert.Equal(t, "kind", got.Fields[1].Key)
	assert.True(t, typeUpdatedAt(t, ctx, pool, page.ID).After(baseTime))
	assert.True(t, typeUpdatedAt(t, ctx, pool, post.ID).After(baseTime))

	// And the referring type loads the renamed sub-fields inline.
	ct, err := repo.GetContentTypeByName(ctx, tenant, "post")
	require.NoError(t, err)
	require.Len(t, ct.Fields, 2)
	assert.Equal(t, "seo", ct.Fields[1].ComponentName)
	require.Len(t, ct.Fields[1].ComponentFields, 2)
	assert.Equal(t, "headline", ct.Fields[1].ComponentFields[0].Key)
}

func TestComponents_DeleteSubFieldPrunesEveryReferrer(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "components_delete_field")
	repo := NewPostgresContentRepository(pool, nil)
	tenant := "tenant-a"
	seo := mkComponent(t, ctx, repo, tenant, "seo", [2]string{"title", "string"}, [2]string{"kind", "enum"})
	page := mkReferrer(t, ctx, repo, tenant, "page", "meta", seo, false)
	post := mkReferrer(t, ctx, repo, tenant, "post", "blocks", seo, true)
	pageIDs := seedEntries(t, ctx, pool, tenant, page.ID,
		entrySeed{payload: `{"title":"p","meta":{"title":"t","kind":"article"}}`, version: 1,
			publishedPayload: `{"title":"p","meta":{"title":"t","kind":"product"}}`, publishedVersion: 1},
		entrySeed{payload: `{"title":"p2","meta":{"title":"no kind"}}`, version: 1},
	)
	postIDs := seedEntries(t, ctx, pool, tenant, post.ID,
		entrySeed{payload: `{"blocks":[{"title":"a","kind":"article"},{"title":"b"}]}`, version: 1},
	)

	// The count the service's HAS_DATA guard uses: entries, not items, and an
	// entry with the key in EITHER copy counts once.
	n, err := repo.CountEntriesWithComponentSubKey(ctx, tenant, ComponentRef{TypeID: page.ID, FieldKey: "meta"}, "seo", "kind")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = repo.CountEntriesWithComponentSubKey(ctx, tenant, ComponentRef{TypeID: post.ID, FieldKey: "blocks", Multiple: true}, "seo", "kind")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = repo.CountEntriesWithComponentSubKey(ctx, tenant, ComponentRef{TypeID: post.ID, FieldKey: "blocks", Multiple: true}, "seo", "title")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	refs, err := repo.ListComponentReferrers(ctx, tenant, seo.ID)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteComponentField(ctx, tenant, seo, refs, "kind", schemaAdmin, baseTime.Add(time.Hour)))

	row := readEntry(t, ctx, pool, pageIDs[0])
	assert.Equal(t, map[string]any{"title": "t"}, docOf(t, &row.payload)["meta"])
	assert.Equal(t, map[string]any{"title": "t"}, docOf(t, row.publishedPayload)["meta"])
	assert.Equal(t, 2, row.version)
	row = readEntry(t, ctx, pool, pageIDs[1])
	assert.Equal(t, 1, row.version, "an entry whose item never held the key is not rewritten")
	row = readEntry(t, ctx, pool, postIDs[0])
	assert.Equal(t, []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}}, docOf(t, &row.payload)["blocks"])

	for _, ct := range []uuid.UUID{page.ID, post.ID} {
		n, err := repo.CountEntriesWithComponentSubKey(ctx, tenant, ComponentRef{TypeID: ct, FieldKey: refs[0].FieldKey}, "seo", "kind")
		require.NoError(t, err)
		_ = n
	}
	got, err := repo.GetComponentByName(ctx, tenant, "seo")
	require.NoError(t, err)
	require.Len(t, got.Fields, 1)
	assert.Equal(t, "title", got.Fields[0].Key)
	// A second delete of the same key is not found — the row is gone.
	assert.Error(t, repo.DeleteComponentField(ctx, tenant, seo, refs, "kind", schemaAdmin, baseTime.Add(2*time.Hour)))
}

func TestComponents_DeleteIsRestrictedWhileReferenced(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "components_delete")
	repo := NewPostgresContentRepository(pool, nil)
	tenant := "tenant-a"
	seo := mkComponent(t, ctx, repo, tenant, "seo", [2]string{"title", "string"})
	page := mkReferrer(t, ctx, repo, tenant, "page", "meta", seo, false)

	// The service refuses first (CONTENT_COMPONENT_IN_USE); the schema refuses
	// SECOND, so a path around the service cannot orphan a referencing field.
	err := repo.DeleteComponent(ctx, tenant, seo.ID)
	require.Error(t, err, "FK RESTRICT keeps a referenced component")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM content_type_fields WHERE component_id = $1`, seo.ID).Scan(&n))
	assert.Equal(t, 1, n, "the refused delete rolled its sub-field delete back")

	// A referencing field cannot be pointed at a component that does not
	// exist, and a component field cannot exist without a reference.
	_, err = pool.Exec(ctx, `UPDATE content_type_fields SET ref_component_id = NULL WHERE content_type_id = $1 AND key = 'meta'`, page.ID)
	require.Error(t, err, "content_type_fields_component_ref_check")
	_, err = pool.Exec(ctx, `UPDATE content_type_fields SET ref_component_id = $1 WHERE component_id = $1`, seo.ID)
	require.Error(t, err, "a sub-field cannot be a component: nesting check")

	// Drop the referrer's field, and the component goes with its sub-fields.
	_, err = pool.Exec(ctx, `DELETE FROM content_type_fields WHERE content_type_id = $1 AND key = 'meta'`, page.ID)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteComponent(ctx, tenant, seo.ID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM content_type_fields WHERE component_id = $1`, seo.ID).Scan(&n))
	assert.Equal(t, 0, n)
	_, err = repo.GetComponentByName(ctx, tenant, "seo")
	require.Error(t, err)

	// Unique per tenant, not globally: another tenant may own a `seo` too.
	mkComponent(t, ctx, repo, tenant, "seo", [2]string{"title", "string"})
	mkComponent(t, ctx, repo, "tenant-b", "seo", [2]string{"title", "string"})
	dup := &domain.Component{ID: uuid.New(), TenantID: tenant, Name: "seo", CreatedAt: baseTime, UpdatedAt: baseTime}
	err = repo.CreateComponent(ctx, dup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestComponents_RLSIsolatesTheTable(t *testing.T) {
	ctx, pool, container := startContentDB(t, "components_rls")
	repo := NewPostgresContentRepository(pool, nil)
	mkComponent(t, ctx, repo, "tenant-a", "seo", [2]string{"title", "string"})
	mkComponent(t, ctx, repo, "tenant-b", "seo", [2]string{"title", "string"})

	_, err := pool.Exec(ctx, `
		CREATE ROLE rlsapp LOGIN PASSWORD 'rlspw' NOSUPERUSER;
		GRANT USAGE ON SCHEMA public TO rlsapp;
		GRANT SELECT, INSERT, UPDATE, DELETE ON content_types, entries, content_type_fields, content_components TO rlsapp;
	`)
	require.NoError(t, err)
	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	app, err := pgxpool.New(ctx, "postgres://rlsapp:rlspw@"+host+":"+port.Port()+"/components_rls?sslmode=disable")
	require.NoError(t, err)
	defer app.Close()

	names := func(tenant string) []string {
		tx, err := app.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if tenant != "" {
			_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
			require.NoError(t, err)
		}
		rows, err := tx.Query(ctx, `SELECT tenant_id FROM content_components ORDER BY tenant_id`)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			out = append(out, s)
		}
		return out
	}
	assert.Equal(t, []string{"tenant-a"}, names("tenant-a"))
	assert.Equal(t, []string{"tenant-b"}, names("tenant-b"))
	assert.Empty(t, names(""), "no tenant set: no rows, not all rows")

	// WITH CHECK: a row for another tenant cannot be written from this one.
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-a', true)`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO content_components (id, tenant_id, name, label) VALUES ($1, 'tenant-b', 'og', '')`, uuid.New())
	require.Error(t, err)
}

func TestComponents_Migration044DownUpDownUp(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "components_migration")
	repo := NewPostgresContentRepository(pool, nil)
	tenant := "tenant-a"
	seo := mkComponent(t, ctx, repo, tenant, "seo", [2]string{"title", "string"})
	page := mkReferrer(t, ctx, repo, tenant, "page", "meta", seo, false)
	seedEntries(t, ctx, pool, tenant, page.ID, entrySeed{payload: `{"title":"p","meta":{"title":"t"}}`, version: 1})

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(contentMigrationDir(), name))
		require.NoError(t, err)
		return string(b)
	}
	down, up := read("000044_content_components.down.sql"), read("000044_content_components.up.sql")

	exists := func(q string, args ...any) bool {
		var ok bool
		require.NoError(t, pool.QueryRow(ctx, q, args...).Scan(&ok))
		return ok
	}
	tableExists := func() bool { return exists(`SELECT to_regclass('content_components') IS NOT NULL`) }
	columnExists := func(col string) bool {
		return exists(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'content_type_fields' AND column_name = $1)`, col)
	}
	checkName := func() string {
		var name string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT conname FROM pg_constraint
			WHERE conrelid = 'content_type_fields'::regclass AND conname LIKE 'content_type_fields_field_type_check%'`).Scan(&name))
		return name
	}

	for round := 0; round < 2; round++ {
		// Down drops cleanly WITH data present: the referencing field and the
		// sub-field rows go (documented loss); the entry keeps its inline
		// value as an undefined key; the scalar field survives.
		_, err := pool.Exec(ctx, down)
		require.NoError(t, err, "down, round %d", round)
		assert.False(t, tableExists())
		assert.False(t, columnExists("component_id"))
		assert.False(t, columnExists("ref_component_id"))
		assert.Equal(t, "content_type_fields_field_type_check_v2", checkName())
		// Raw SQL, not the repository: its field loader joins the table the
		// down just dropped, which is the correct shape for code that only
		// ever runs against the migrated schema.
		var keys []string
		rows, err := pool.Query(ctx, `SELECT key FROM content_type_fields WHERE content_type_id = $1 ORDER BY key`, page.ID)
		require.NoError(t, err)
		for rows.Next() {
			var k string
			require.NoError(t, rows.Scan(&k))
			keys = append(keys, k)
		}
		rows.Close()
		assert.Equal(t, []string{"title"}, keys, "the referencing field is gone, the scalar one stays")
		assert.True(t, exists(`SELECT EXISTS (SELECT 1 FROM entries WHERE content_type_id = $1 AND payload ? 'meta')`, page.ID),
			"the inline value is data the migration does not touch")
		_, err = pool.Exec(ctx, `INSERT INTO content_type_fields (id, content_type_id, key, field_type, label) VALUES ($1, $2, 'x', 'component', '')`, uuid.New(), page.ID)
		require.Error(t, err, "v2 CHECK is back: component is not a type")

		_, err = pool.Exec(ctx, up)
		require.NoError(t, err, "up, round %d", round)
		assert.True(t, tableExists())
		assert.True(t, columnExists("component_id"))
		assert.True(t, columnExists("ref_component_id"))
		assert.Equal(t, "content_type_fields_field_type_check_v3", checkName())
		// And the feature works again on the re-applied schema.
		c := mkComponent(t, ctx, repo, tenant, "og", [2]string{"image", "file"})
		_, err = repo.GetComponentByName(ctx, tenant, "og")
		require.NoError(t, err)
		require.NoError(t, repo.DeleteComponent(ctx, tenant, c.ID))
	}
}

// --- media links --------------------------------------------------------------

// A file (or richtext) sub-field links media through entry_media exactly as a
// top-level field does, so deleting it must re-sync the link tables the same
// way: otherwise the removed asset stays publicly resolvable through the
// "asset is published" gate until the entry happens to be written again.
// The rebuild is also the ONLY place that decides which links survive, so it
// has to know about the nested places too — a top-level file delete on a type
// that embeds a component must not prune the component's links.
func TestComponents_DeleteMediaSubFieldRelinksEntryMedia(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "compmedia")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "t1"
	now := baseTime.Add(time.Hour)

	hero := mkComponent(t, ctx, repo, tenant, "hero",
		[2]string{"image", domain.FieldTypeFile},
		[2]string{"body", domain.FieldTypeRichText},
		[2]string{"caption", domain.FieldTypeString},
	)
	// page: title + cover (top-level file) + hero (single object)
	page := &domain.ContentType{ID: uuid.New(), TenantID: tenant, Name: "page", Label: "page", CreatedAt: baseTime, UpdatedAt: baseTime}
	{
		ref := mkField(page.ID, "hero", domain.FieldTypeComponent, 2)
		id := hero.ID
		ref.ComponentID, ref.ComponentName = &id, hero.Name
		page.Fields = []domain.Field{
			mkField(page.ID, "title", domain.FieldTypeString, 0),
			mkField(page.ID, "cover", domain.FieldTypeFile, 1),
			ref,
		}
		require.NoError(t, repo.CreateContentType(ctx, page))
	}
	post := mkReferrer(t, ctx, repo, tenant, "post", "heroes", hero, true)

	// X: page.hero.image; Y: page.cover; W: post.heroes[0].image; Z: post.heroes[1].body image block.
	X, Y, W, Z := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{X, Y, W, Z} {
		_, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, uploaded_at)
			VALUES ($1,$2,$3, NOW())`, id, tenant, tenant+"/"+id.String())
		require.NoError(t, err, "seed asset")
	}
	pagePayload := fmt.Sprintf(`{"title":"p","cover":"%s","hero":{"image":"%s","caption":"c"}}`, Y, X)
	postPayload := fmt.Sprintf(`{"title":"q","heroes":[{"image":"%s"},{"body":[{"type":"paragraph","text":"t"},{"type":"image","media_id":"%s"}]}]}`, W, Z)
	pageID := seedEntries(t, ctx, pool, tenant, page.ID,
		entrySeed{payload: pagePayload, version: 1, publishedPayload: pagePayload, publishedVersion: 1})[0]
	postID := seedEntries(t, ctx, pool, tenant, post.ID,
		entrySeed{payload: postPayload, version: 1, publishedPayload: postPayload, publishedVersion: 1})[0]
	link := func(entry uuid.UUID, assets ...uuid.UUID) {
		for _, table := range []string{"entry_media", "entry_media_published"} {
			for _, a := range assets {
				//nolint:gosec // table is a test-local literal
				_, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (entry_id, asset_id, tenant_id) VALUES ($1,$2,$3)`, table), entry, a, tenant)
				require.NoError(t, err, "seed link")
			}
		}
	}
	link(pageID, X, Y)
	link(postID, W, Z)

	linked := func(entry, asset uuid.UUID) (n int) {
		t.Helper()
		for _, table := range []string{"entry_media", "entry_media_published"} {
			var k int
			//nolint:gosec // table is a test-local literal
			require.NoError(t, pool.QueryRow(ctx,
				fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE entry_id = $1 AND asset_id = $2`, table), entry, asset).Scan(&k))
			n += k
		}
		return n
	}
	published := func(asset uuid.UUID) bool {
		t.Helper()
		ok, err := repo.AssetIsPublished(ctx, tenant, asset)
		require.NoError(t, err)
		return ok
	}
	for _, a := range []uuid.UUID{X, Y, W, Z} {
		require.True(t, published(a), "guard: %s must start published", a)
	}
	refs, err := repo.ListComponentReferrers(ctx, tenant, hero.ID)
	require.NoError(t, err)
	require.Len(t, refs, 2)

	t.Run("a top-level file delete keeps the links that live inside the component", func(t *testing.T) {
		full, err := repo.GetContentTypeByName(ctx, tenant, "page") // ComponentFields inlined
		require.NoError(t, err)
		cover, _ := full.FieldByKey("cover")
		require.NoError(t, repo.DeleteField(ctx, tenant, full, cover, schemaAdmin, now))
		assert.Equal(t, 0, linked(pageID, Y), "the deleted file field's asset is unlinked")
		assert.Equal(t, 2, linked(pageID, X), "the component's file sub-field survives the rebuild")
		assert.False(t, published(Y))
		assert.True(t, published(X))
	})

	t.Run("deleting the file sub-field unlinks its asset in every referrer", func(t *testing.T) {
		require.NoError(t, repo.DeleteComponentField(ctx, tenant, hero, refs, "image", schemaAdmin, now))
		assert.Equal(t, 0, linked(pageID, X), "page.hero.image")
		assert.Equal(t, 0, linked(postID, W), "post.heroes[0].image")
		assert.Equal(t, 2, linked(postID, Z), "the richtext sub-field's image block survives")
		assert.False(t, published(X), "the gate closes with the link")
		assert.False(t, published(W))
		assert.True(t, published(Z))
	})

	t.Run("deleting the richtext sub-field unlinks its image blocks too", func(t *testing.T) {
		c, err := repo.GetComponentByName(ctx, tenant, "hero")
		require.NoError(t, err)
		require.NoError(t, repo.DeleteComponentField(ctx, tenant, c, refs, "body", schemaAdmin, now))
		assert.Equal(t, 0, linked(postID, Z))
		assert.False(t, published(Z))
	})
}
