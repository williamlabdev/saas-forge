package repository

import (
	"context"
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
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Dynamic zones (ADR-020 Amendment 1) against a real Postgres. What is new
// here versus the component tests next door is that a zone's items are
// HETEROGENEOUS and tagged with __component, so every set-based rewrite has to
// hit exactly the items of one component and leave its neighbours — which may
// share a sub-field key — untouched. Every assertion reads the stored document
// or the link tables back rather than trusting the repository's own reads.

// mkZone creates a type whose fields are `title` plus one dynamic zone that
// accepts the named components, in the order given (the order an editor's
// block menu offers).
func mkZone(t *testing.T, ctx context.Context, repo *PostgresContentRepository,
	tenant, name, key string, components ...string) *domain.ContentType {
	t.Helper()
	ct := &domain.ContentType{
		ID: uuid.New(), TenantID: tenant, Name: name, Label: name,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	zone := mkField(ct.ID, key, domain.FieldTypeDynamicZone, 1)
	zone.ZoneComponents = components
	ct.Fields = []domain.Field{mkField(ct.ID, "title", domain.FieldTypeString, 0), zone}
	require.NoError(t, repo.CreateContentType(ctx, ct), "create zone type %s", name)
	return ct
}

// allowedList reads a zone field's allowed-list straight out of the column,
// because the point of most of these assertions is that the STORED list moved.
func allowedList(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ctID uuid.UUID, key string) []string {
	t.Helper()
	var out []string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT zone_components FROM content_type_fields WHERE content_type_id = $1 AND key = $2`,
		ctID, key).Scan(&out))
	return out
}

// TestDynamicZone_ComponentDeleteIsRefusedByAnAllowedList is cross-cutting
// item 1. A zone names its components in a TEXT[], so unlike an embedding
// field there is NO foreign key to refuse the delete — which is exactly why
// the refusal is asserted at this layer and not only in the service.
func TestDynamicZone_ComponentDeleteIsRefusedByAnAllowedList(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "zone_delete_guard")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "tenant-a"
	hero := mkComponent(t, ctx, repo, tenant, "hero", [2]string{"headline", "string"})
	cta := mkComponent(t, ctx, repo, tenant, "cta", [2]string{"label", "string"})
	page := mkZone(t, ctx, repo, tenant, "page", "zone", "hero", "cta")

	// A zone referrer is a referrer in every sense: used_by lists it, and the
	// sub-field rewrites fan out to it.
	refs, err := repo.ListComponentReferrers(ctx, tenant, hero.ID)
	require.NoError(t, err)
	assert.Equal(t, []ComponentRef{{TypeID: page.ID, TypeName: "page", FieldKey: "zone", Zone: true}}, refs)

	err = repo.DeleteComponent(ctx, tenant, hero.ID)
	require.Error(t, err, "a component named by a live allowed-list cannot be deleted")
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, "CONTENT_COMPONENT_IN_USE", ae.Code)
	assert.Equal(t, []string{"page"}, ae.Details["used_by"], "the refusal names the type an editor has to change")
	got, err := repo.GetComponentByName(ctx, tenant, "hero")
	require.NoError(t, err)
	require.Len(t, got.Fields, 1, "the refused delete rolled its sub-field delete back")

	// Drop the name from the list and the same delete goes through — the guard
	// is the LIST, not the existence of a zone field somewhere.
	_, err = pool.Exec(ctx,
		`UPDATE content_type_fields SET zone_components = ARRAY['cta'] WHERE content_type_id = $1 AND key = 'zone'`, page.ID)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteComponent(ctx, tenant, hero.ID))
	_, err = repo.GetComponentByName(ctx, tenant, "hero")
	require.Error(t, err)

	// And the other component in the same list is still protected.
	require.Error(t, repo.DeleteComponent(ctx, tenant, cta.ID))
}

// TestDynamicZone_RenameComponentRetagsItemsAndAllowedLists is cross-cutting
// item 2: a component's name is STORED DATA once zones exist — in every item's
// __component and in every allowed-list — so the rename is a data migration
// that must land in one transaction with the definition change.
func TestDynamicZone_RenameComponentRetagsItemsAndAllowedLists(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "zone_rename_component")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "tenant-a"
	hero := mkComponent(t, ctx, repo, tenant, "hero", [2]string{"headline", "string"})
	mkComponent(t, ctx, repo, tenant, "cta", [2]string{"label", "string"})
	page := mkZone(t, ctx, repo, tenant, "page", "zone", "hero", "cta")
	post := mkZone(t, ctx, repo, tenant, "post", "sections", "hero")
	// An embedding `component` field references the SAME component by id. It
	// must come through a rename untouched: the id did not move, and rewriting
	// its items would be inventing a __component key the type has no zone for.
	embed := mkReferrer(t, ctx, repo, tenant, "note", "meta", hero, false)

	pageIDs := seedEntries(t, ctx, pool, tenant, page.ID,
		entrySeed{
			payload:          `{"title":"p","zone":[{"__component":"hero","headline":"draft"},{"__component":"cta","label":"go"}]}`,
			version:          3,
			publishedPayload: `{"title":"p","zone":[{"__component":"hero","headline":"live"}]}`,
			publishedVersion: 2,
		},
		// No zone value at all: untouched, versions stay.
		entrySeed{payload: `{"title":"bare"}`, version: 1},
	)
	postIDs := seedEntries(t, ctx, pool, tenant, post.ID,
		entrySeed{payload: `{"title":"q","sections":[{"__component":"hero","headline":"one"},"junk"]}`, version: 1})
	embedIDs := seedEntries(t, ctx, pool, tenant, embed.ID,
		entrySeed{payload: `{"title":"n","meta":{"headline":"m"}}`, version: 1})
	bareBefore := readEntry(t, ctx, pool, pageIDs[1])
	embedBefore := readEntry(t, ctx, pool, embedIDs[0])
	revsBefore := revisionCount(t, ctx, pool, pageIDs[0])

	refs, err := repo.ListComponentReferrers(ctx, tenant, hero.ID)
	require.NoError(t, err)
	require.Equal(t, []ComponentRef{
		{TypeID: embed.ID, TypeName: "note", FieldKey: "meta"},
		{TypeID: page.ID, TypeName: "page", FieldKey: "zone", Zone: true},
		{TypeID: post.ID, TypeName: "post", FieldKey: "sections", Zone: true},
	}, refs, "both kinds of referrer, in type/key order")

	now := baseTime.Add(time.Hour)
	require.NoError(t, repo.RenameComponent(ctx, tenant, hero, refs, "banner", schemaAdmin, now))

	row := readEntry(t, ctx, pool, pageIDs[0])
	work := docOf(t, &row.payload)
	assert.Equal(t, []any{
		map[string]any{"__component": "banner", "headline": "draft"},
		map[string]any{"__component": "cta", "label": "go"},
	}, work["zone"], "only the items that named hero are retagged, and order is preserved")
	live := docOf(t, row.publishedPayload)
	assert.Equal(t, []any{map[string]any{"__component": "banner", "headline": "live"}}, live["zone"])
	assert.Equal(t, 4, row.version, "working copy changed: version moves")
	require.NotNil(t, row.publishedVersion)
	assert.Equal(t, 3, *row.publishedVersion, "published copy changed: its version moves too")
	assert.Equal(t, revsBefore+1, revisionCount(t, ctx, pool, pageIDs[0]), "the retag is a revision with provenance")

	assert.Equal(t, bareBefore, readEntry(t, ctx, pool, pageIDs[1]), "an entry without a zone value is not touched")
	assert.Equal(t, embedBefore, readEntry(t, ctx, pool, embedIDs[0]), "an embedding field stores the id: nothing to rewrite")

	row = readEntry(t, ctx, pool, postIDs[0])
	work = docOf(t, &row.payload)
	assert.Equal(t, []any{map[string]any{"__component": "banner", "headline": "one"}, "junk"}, work["sections"],
		"a non-object element is left as it is")

	// Every allowed-list follows, in place: a rename must not reshuffle the
	// order an editor's block menu offers.
	assert.Equal(t, []string{"banner", "cta"}, allowedList(t, ctx, pool, page.ID, "zone"))
	assert.Equal(t, []string{"banner"}, allowedList(t, ctx, pool, post.ID, "sections"))

	got, err := repo.GetComponentByName(ctx, tenant, "banner")
	require.NoError(t, err)
	assert.Equal(t, hero.ID, got.ID)
	_, err = repo.GetComponentByName(ctx, tenant, "hero")
	require.Error(t, err, "the old name is free again")
	for _, id := range []uuid.UUID{page.ID, post.ID, embed.ID} {
		assert.True(t, typeUpdatedAt(t, ctx, pool, id).After(baseTime), "referrers are touched: their DTOs changed shape")
	}

	// The loaded type carries the renamed component's sub-fields inline, which
	// is what makes validation independent of a second lookup.
	ct, err := repo.GetContentTypeByName(ctx, tenant, "post")
	require.NoError(t, err)
	require.Len(t, ct.Fields, 2)
	assert.Equal(t, []string{"banner"}, ct.Fields[1].ZoneComponents)
	require.Len(t, ct.Fields[1].ZoneFields["banner"], 1)
	assert.Equal(t, "headline", ct.Fields[1].ZoneFields["banner"][0].Key)

	// Taken name: refused, and nothing moved.
	err = repo.RenameComponent(ctx, tenant, got, refs, "cta", schemaAdmin, now.Add(time.Hour))
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, "CONTENT_COMPONENT_EXISTS", ae.Code)
	assert.Equal(t, []string{"banner", "cta"}, allowedList(t, ctx, pool, page.ID, "zone"),
		"the refused rename rolled its allowed-list rewrite back")
}

// TestDynamicZone_SubFieldRewritesReachZoneItemsInBothCopies is cross-cutting
// item 3. Two components deliberately SHARE a sub-field key: the rewrite is
// filtered by __component, so touching one must not touch the other, and the
// two referring types must both be rewritten in both payload copies.
func TestDynamicZone_SubFieldRewritesReachZoneItemsInBothCopies(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "zone_subfield")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "tenant-a"
	hero := mkComponent(t, ctx, repo, tenant, "hero", [2]string{"headline", "string"}, [2]string{"body", "text"})
	mkComponent(t, ctx, repo, tenant, "cta", [2]string{"label", "string"}, [2]string{"body", "text"})
	page := mkZone(t, ctx, repo, tenant, "page", "zone", "hero", "cta")
	post := mkZone(t, ctx, repo, tenant, "post", "sections", "hero")

	pagePayload := `{"title":"p","zone":[{"__component":"hero","headline":"h","body":"hb"},{"__component":"cta","label":"l","body":"cb"}]}`
	pageID := seedEntries(t, ctx, pool, tenant, page.ID,
		entrySeed{payload: pagePayload, version: 1, publishedPayload: pagePayload, publishedVersion: 1})[0]
	postPayload := `{"title":"q","sections":[{"__component":"hero","body":"pb"}]}`
	postID := seedEntries(t, ctx, pool, tenant, post.ID,
		entrySeed{payload: postPayload, version: 5, publishedPayload: postPayload, publishedVersion: 4})[0]

	refs, err := repo.ListComponentReferrers(ctx, tenant, hero.ID)
	require.NoError(t, err)
	require.Len(t, refs, 2)

	// Counting comes first: it is the number the service's 409 reports, and it
	// must be as discriminating as the rewrite.
	for _, ref := range refs {
		n, err := repo.CountEntriesWithComponentSubKey(ctx, tenant, ref, "hero", "body")
		require.NoError(t, err)
		assert.Equal(t, 1, n, "%s.%s holds a hero item with `body`", ref.TypeName, ref.FieldKey)
	}
	n, err := repo.CountEntriesWithComponentSubKey(ctx, tenant, refs[1], "hero", "label")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "`label` is cta's key: a hero-filtered count must not see it")

	now := baseTime.Add(time.Hour)
	require.NoError(t, repo.RenameComponentField(ctx, tenant, hero, refs, "body", "text", schemaAdmin, now))

	row := readEntry(t, ctx, pool, pageID)
	for _, doc := range []map[string]any{docOf(t, &row.payload), docOf(t, row.publishedPayload)} {
		assert.Equal(t, []any{
			map[string]any{"__component": "hero", "headline": "h", "text": "hb"},
			map[string]any{"__component": "cta", "label": "l", "body": "cb"},
		}, doc["zone"], "the cta item keeps the key it shares with hero")
	}
	assert.Equal(t, 2, row.version)
	require.NotNil(t, row.publishedVersion)
	assert.Equal(t, 2, *row.publishedVersion)

	row = readEntry(t, ctx, pool, postID)
	assert.Equal(t, []any{map[string]any{"__component": "hero", "text": "pb"}}, docOf(t, &row.payload)["sections"])
	assert.Equal(t, []any{map[string]any{"__component": "hero", "text": "pb"}}, docOf(t, row.publishedPayload)["sections"])
	assert.Equal(t, 6, row.version, "the second referring type is rewritten too")

	// Delete takes the same path with new_key NULL.
	c, err := repo.GetComponentByName(ctx, tenant, "hero")
	require.NoError(t, err)
	require.NoError(t, repo.DeleteComponentField(ctx, tenant, c, refs, "text", schemaAdmin, now.Add(time.Hour)))
	row = readEntry(t, ctx, pool, pageID)
	for _, doc := range []map[string]any{docOf(t, &row.payload), docOf(t, row.publishedPayload)} {
		assert.Equal(t, []any{
			map[string]any{"__component": "hero", "headline": "h"},
			map[string]any{"__component": "cta", "label": "l", "body": "cb"},
		}, doc["zone"])
	}
	row = readEntry(t, ctx, pool, postID)
	assert.Equal(t, []any{map[string]any{"__component": "hero"}}, docOf(t, &row.payload)["sections"])

	got, err := repo.GetComponentByName(ctx, tenant, "cta")
	require.NoError(t, err)
	require.Len(t, got.Fields, 2, "cta's own definition is untouched by hero's sub-field verbs")
}

// TestDynamicZone_MediaLinksArePerComponent is cross-cutting item 4, and the
// reason it is spelled out at this length: entry_media is rebuilt from the
// places a value CAN live, so a zone that the rebuild does not know how to
// walk silently leaves links behind — and AssetIsPublished keeps answering
// true for bytes nothing references any more (ADR-005). Two components here
// share the sub-field key `image` on purpose: without the __component filter
// in mediaPaths, deleting hero.image would take cta's links with it.
func TestDynamicZone_MediaLinksArePerComponent(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "zonemedia")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "t1"
	now := baseTime.Add(time.Hour)

	hero := mkComponent(t, ctx, repo, tenant, "hero",
		[2]string{"image", domain.FieldTypeFile},
		[2]string{"body", domain.FieldTypeRichText},
		[2]string{"caption", domain.FieldTypeString},
	)
	mkComponent(t, ctx, repo, tenant, "cta",
		[2]string{"image", domain.FieldTypeFile},
		[2]string{"label", domain.FieldTypeString},
	)
	// page: title + cover (top-level file) + zone [hero, cta]
	page := &domain.ContentType{ID: uuid.New(), TenantID: tenant, Name: "page", Label: "page", CreatedAt: baseTime, UpdatedAt: baseTime}
	zone := mkField(page.ID, "zone", domain.FieldTypeDynamicZone, 2)
	zone.ZoneComponents = []string{"hero", "cta"}
	page.Fields = []domain.Field{
		mkField(page.ID, "title", domain.FieldTypeString, 0),
		mkField(page.ID, "cover", domain.FieldTypeFile, 1),
		zone,
	}
	require.NoError(t, repo.CreateContentType(ctx, page))

	// A: hero item's file. B: cta item's file (SAME sub-field key).
	// C: hero item's richtext image block. Y: the top-level cover.
	A, B, C, Y := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{A, B, C, Y} {
		_, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, uploaded_at)
			VALUES ($1,$2,$3, NOW())`, id, tenant, tenant+"/"+id.String())
		require.NoError(t, err, "seed asset")
	}
	payload := fmt.Sprintf(`{"title":"p","cover":"%s","zone":[`+
		`{"__component":"hero","image":"%s","body":[{"type":"paragraph","text":"t"},{"type":"image","media_id":"%s"}]},`+
		`{"__component":"cta","image":"%s","label":"go"}]}`, Y, A, C, B)
	pageID := seedEntries(t, ctx, pool, tenant, page.ID,
		entrySeed{payload: payload, version: 1, publishedPayload: payload, publishedVersion: 1})[0]
	for _, table := range []string{"entry_media", "entry_media_published"} {
		for _, a := range []uuid.UUID{A, B, C, Y} {
			//nolint:gosec // table is a test-local literal
			_, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (entry_id, asset_id, tenant_id) VALUES ($1,$2,$3)`, table), pageID, a, tenant)
			require.NoError(t, err, "seed link")
		}
	}
	linked := func(asset uuid.UUID) (n int) {
		t.Helper()
		for _, table := range []string{"entry_media", "entry_media_published"} {
			var k int
			//nolint:gosec // table is a test-local literal
			require.NoError(t, pool.QueryRow(ctx,
				fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE entry_id = $1 AND asset_id = $2`, table), pageID, asset).Scan(&k))
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
	for _, a := range []uuid.UUID{A, B, C, Y} {
		require.True(t, published(a), "guard: %s must start published", a)
	}
	refs, err := repo.ListComponentReferrers(ctx, tenant, hero.ID)
	require.NoError(t, err)
	require.Equal(t, []ComponentRef{{TypeID: page.ID, TypeName: "page", FieldKey: "zone", Zone: true}}, refs)

	t.Run("an unrelated field delete leaves every zone link standing", func(t *testing.T) {
		full, err := repo.GetContentTypeByName(ctx, tenant, "page") // ZoneFields attached
		require.NoError(t, err)
		cover, ok := full.FieldByKey("cover")
		require.True(t, ok)
		require.NoError(t, repo.DeleteField(ctx, tenant, full, cover, schemaAdmin, now))
		assert.Equal(t, 0, linked(Y), "the deleted file field's asset is unlinked")
		assert.Equal(t, 2, linked(A), "hero.image inside the zone survives the rebuild")
		assert.Equal(t, 2, linked(B), "cta.image inside the zone survives the rebuild")
		assert.Equal(t, 2, linked(C), "the richtext image block inside the zone survives")
		assert.False(t, published(Y), "the gate closes with the link")
		for _, a := range []uuid.UUID{A, B, C} {
			assert.True(t, published(a))
		}
	})

	t.Run("deleting one component's file sub-field drops only that component's links", func(t *testing.T) {
		require.NoError(t, repo.DeleteComponentField(ctx, tenant, hero, refs, "image", schemaAdmin, now))
		assert.Equal(t, 0, linked(A), "hero.image is gone from the definition and from both link tables")
		assert.Equal(t, 2, linked(B), "cta.image shares the key and must not be collateral")
		assert.Equal(t, 2, linked(C))
		assert.False(t, published(A))
		assert.True(t, published(B))
		assert.True(t, published(C))
	})

	t.Run("deleting the richtext sub-field unlinks its image blocks too", func(t *testing.T) {
		c, err := repo.GetComponentByName(ctx, tenant, "hero")
		require.NoError(t, err)
		require.NoError(t, repo.DeleteComponentField(ctx, tenant, c, refs, "body", schemaAdmin, now.Add(time.Hour)))
		assert.Equal(t, 0, linked(C))
		assert.False(t, published(C))
		assert.Equal(t, 2, linked(B), "cta is still whole")
	})
}

// TestDynamicZone_Migration045DownUpDownUp is cross-cutting item 8. Twice,
// because a down that is not an exact reverse usually survives one round and
// fails the second — the constraint it left behind is already there.
func TestDynamicZone_Migration045DownUpDownUp(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "zone_migration")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "tenant-a"
	mkComponent(t, ctx, repo, tenant, "hero", [2]string{"headline", "string"})
	page := mkZone(t, ctx, repo, tenant, "page", "zone", "hero")
	seedEntries(t, ctx, pool, tenant, page.ID,
		entrySeed{payload: `{"title":"p","zone":[{"__component":"hero","headline":"h"}]}`, version: 1})

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(contentMigrationDir(), name))
		require.NoError(t, err)
		return string(b)
	}
	down, up := read("000045_content_dynamic_zone.down.sql"), read("000045_content_dynamic_zone.up.sql")

	exists := func(q string, args ...any) bool {
		var ok bool
		require.NoError(t, pool.QueryRow(ctx, q, args...).Scan(&ok))
		return ok
	}
	columnExists := func() bool {
		return exists(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name = 'content_type_fields' AND column_name = 'zone_components')`)
	}
	fnExists := func(name string) bool {
		return exists(`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = $1)`, name)
	}
	constraintExists := func(name string) bool {
		return exists(`SELECT EXISTS (SELECT 1 FROM pg_constraint
			WHERE conrelid = 'content_type_fields'::regclass AND conname = $1)`, name)
	}
	checkName := func() string {
		var name string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT conname FROM pg_constraint
			WHERE conrelid = 'content_type_fields'::regclass AND conname LIKE 'content_type_fields_field_type_check%'`).Scan(&name))
		return name
	}
	zoneFields := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM content_type_fields WHERE field_type = 'dynamiczone'`).Scan(&n))
		return n
	}

	require.Equal(t, 1, zoneFields(), "guard: the fixture wrote a zone field")

	for round := 0; round < 2; round++ {
		_, err := pool.Exec(ctx, down)
		require.NoError(t, err, "down, round %d", round)
		assert.False(t, columnExists())
		assert.Equal(t, "content_type_fields_field_type_check_v3", checkName())
		assert.False(t, constraintExists("content_type_fields_zone_components_check"))
		assert.False(t, constraintExists("content_type_fields_zone_nesting_check"))
		for _, fn := range []string{"content_zone_rewrite_component", "content_zone_has_component",
			"content_zone_rewrite_subkey", "content_zone_has_subkey"} {
			assert.False(t, fnExists(fn), "%s must go with the column it exists to walk", fn)
		}
		assert.True(t, fnExists("content_component_rewrite_item"), "000044's helpers are not this migration's to drop")
		assert.Equal(t, 0, zoneFields(), "the field definitions go: the v3 CHECK could not hold them")
		// The scalar field beside the zone stays, and the item array stays in
		// the payload as a key no field defines (documented loss).
		assert.True(t, exists(`SELECT EXISTS (SELECT 1 FROM content_type_fields WHERE content_type_id = $1 AND key = 'title')`, page.ID))
		assert.True(t, exists(`SELECT EXISTS (SELECT 1 FROM entries WHERE content_type_id = $1 AND payload ? 'zone')`, page.ID),
			"the inline value is data the migration does not touch")
		_, err = pool.Exec(ctx,
			`INSERT INTO content_type_fields (id, content_type_id, key, field_type, label) VALUES ($1, $2, 'x', 'dynamiczone', '')`,
			uuid.New(), page.ID)
		require.Error(t, err, "v3 CHECK is back: dynamiczone is not a type")

		_, err = pool.Exec(ctx, up)
		require.NoError(t, err, "up, round %d", round)
		assert.True(t, columnExists())
		assert.Equal(t, "content_type_fields_field_type_check_v4", checkName())
		assert.True(t, constraintExists("content_type_fields_zone_components_check"))
		assert.True(t, constraintExists("content_type_fields_zone_nesting_check"))
		for _, fn := range []string{"content_zone_rewrite_component", "content_zone_has_component",
			"content_zone_rewrite_subkey", "content_zone_has_subkey"} {
			assert.True(t, fnExists(fn))
		}
		// And the feature works again on the re-applied schema.
		again := mkZone(t, ctx, repo, tenant, fmt.Sprintf("again%d", round), "zone", "hero")
		ct, err := repo.GetContentTypeByName(ctx, tenant, again.Name)
		require.NoError(t, err)
		require.Len(t, ct.Fields, 2)
		assert.Equal(t, []string{"hero"}, ct.Fields[1].ZoneComponents)
		require.Len(t, ct.Fields[1].ZoneFields["hero"], 1)
	}
}
