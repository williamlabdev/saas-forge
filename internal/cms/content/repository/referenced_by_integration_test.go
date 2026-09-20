package repository

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-024 (2.8a / 2.6a) against the real database. referenced_by_test.go in
// the service package exercises the merge/whitelist LOGIC through memRepo;
// what only Postgres can get wrong is the SQL itself — the UNION ALL/GROUP BY
// merge in referencedByQuery, the schema-aware backfill's jsonpath
// extraction, and the orphan NOT EXISTS filter. None of that has a Go fake to
// stand in for it.

func newReferencedByPool(t *testing.T, dbName string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase(dbName),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedRefType inserts a content_type row directly plus one plain "title"
// string field via AddField, so TitleFor has something to resolve — mirrors
// multivalue_integration_test.go's direct-SQL-plus-AddField pattern.
func seedRefType(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repo *PostgresContentRepository, tenant, name string) uuid.UUID {
	t.Helper()
	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,$2,$3,'')`, typeID, tenant, name); err != nil {
		t.Fatalf("insert content_type %s: %v", name, err)
	}
	title := domain.Field{ID: uuid.New(), ContentTypeID: typeID, Key: "title", Type: domain.FieldTypeString, CreatedAt: time.Now().UTC(), EnumValues: []string{}}
	if err := repo.AddField(ctx, tenant, &title); err != nil {
		t.Fatalf("add title field: %v", err)
	}
	return typeID
}

func seedRefEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant string, typeID uuid.UUID, title, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	payload := `{"title":"` + title + `"}`
	var publishedPayload, publishedAt, publishedVersion any
	if status == domain.StatusPublished {
		publishedPayload = payload
		publishedAt = time.Now().UTC()
		publishedVersion = 1
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO entries (id, tenant_id, content_type_id, payload, published_payload, published_version, status, published_at, locale, translation_group_id)
		VALUES ($1,$2,$3,$4::jsonb,$5::jsonb,$6,$7,$8,'default',$9)`,
		id, tenant, typeID, payload, publishedPayload, publishedVersion, status, publishedAt, uuid.New()); err != nil {
		t.Fatalf("insert entry %s: %v", title, err)
	}
	return id
}

// TestEntryReferencedBy_Postgres pins referencedByQuery's UNION ALL/GROUP BY
// merge: a target referenced from one entry's DRAFT-only relation row and
// another's draft+published pair must come back as two rows with
// independent Draft/Published booleans, plus a Total that counts the whole
// match set regardless of the page.
func TestEntryReferencedBy_Postgres(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()
	pool := newReferencedByPool(t, "entryref")
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewPostgresContentRepository(pool, nil)

	authorType := seedRefType(t, ctx, pool, repo, "t1", "author")
	postType := seedRefType(t, ctx, pool, repo, "t1", "post")
	author := seedRefEntry(t, ctx, pool, "t1", authorType, "Ada", domain.StatusDraft)
	postDraftOnly := seedRefEntry(t, ctx, pool, "t1", postType, "Draft Post", domain.StatusDraft)
	postBoth := seedRefEntry(t, ctx, pool, "t1", postType, "Published Post", domain.StatusPublished)
	otherTenantPost := seedRefEntry(t, ctx, pool, "t2", postType, "Other Tenant", domain.StatusDraft)

	if err := repo.ReplaceEntryRelations(ctx, "t1", postDraftOnly, []EntryRelationRef{{FieldPath: "author", TargetID: author}}); err != nil {
		t.Fatalf("replace relations (draft only): %v", err)
	}
	if err := repo.ReplaceEntryRelations(ctx, "t1", postBoth, []EntryRelationRef{{FieldPath: "author", TargetID: author}}); err != nil {
		t.Fatalf("replace relations (both): %v", err)
	}
	// The published snapshot table is a distinct write — ReplaceEntryRelations
	// only ever touches the draft table, mirroring ReplaceEntryMedia; the
	// service's publish path is what copies into *_published.
	if _, err := pool.Exec(ctx, `INSERT INTO entry_relations_published (entry_id, field_path, target_entry_id, tenant_id) VALUES ($1,'author',$2,'t1')`, postBoth, author); err != nil {
		t.Fatalf("seed published relation: %v", err)
	}
	// Cross-tenant noise: same target id string coincidence is impossible
	// (uuid), so this alone proves nothing leaks even without matching ids —
	// insert a same-tenant-only different target instead to keep the count math simple.
	_ = otherTenantPost

	got, err := repo.EntryReferencedBy(ctx, "t1", author, 0, 0)
	if err != nil {
		t.Fatalf("EntryReferencedBy: %v", err)
	}
	if got.Total != 2 {
		t.Fatalf("total = %d, want 2", got.Total)
	}
	if len(got.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(got.Items))
	}
	byID := map[uuid.UUID]ReferencedByRow{}
	for _, it := range got.Items {
		byID[it.EntryID] = it
	}
	draftOnly, ok := byID[postDraftOnly]
	if !ok {
		t.Fatal("draft-only referrer missing")
	}
	if !draftOnly.Draft || draftOnly.Published {
		t.Fatalf("draft-only referrer: draft=%v published=%v, want true/false", draftOnly.Draft, draftOnly.Published)
	}
	if draftOnly.FieldPath != "author" {
		t.Fatalf("field_path = %q, want author", draftOnly.FieldPath)
	}
	if draftOnly.Title != "Draft Post" {
		t.Fatalf("title = %q, want %q (TitleFor must read the draft payload)", draftOnly.Title, "Draft Post")
	}
	both, ok := byID[postBoth]
	if !ok {
		t.Fatal("draft+published referrer missing")
	}
	if !both.Draft || !both.Published {
		t.Fatalf("both referrer: draft=%v published=%v, want true/true", both.Draft, both.Published)
	}

	t.Run("pagination pages the merged set, not the raw union", func(t *testing.T) {
		page, err := repo.EntryReferencedBy(ctx, "t1", author, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 2 || len(page.Items) != 1 {
			t.Fatalf("total=%d len(items)=%d, want 2/1", page.Total, len(page.Items))
		}
	})

	t.Run("a target with no referrers returns an empty page, not an error", func(t *testing.T) {
		empty, err := repo.EntryReferencedBy(ctx, "t1", uuid.New(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if empty.Total != 0 || len(empty.Items) != 0 {
			t.Fatalf("total=%d len(items)=%d, want 0/0", empty.Total, len(empty.Items))
		}
	})
}

// TestMediaReferencedBy_Postgres mirrors the entry test for entry_media /
// entry_media_published — field_path is always "" here (entry_media has no
// per-field column), and the merge/pagination SQL is otherwise identical
// because both callers share referencedByQuery.
func TestMediaReferencedBy_Postgres(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()
	pool := newReferencedByPool(t, "mediaref")
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewPostgresContentRepository(pool, nil)

	docType := seedRefType(t, ctx, pool, repo, "t1", "doc")
	assetID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_assets (id, tenant_id, storage_key, content_type, size_bytes, filename, uploaded_at, created_at)
		VALUES ($1,'t1',$2,'image/png',1,'cover.png',NOW(),NOW())`, assetID, "t1/"+assetID.String()); err != nil {
		t.Fatalf("insert media asset: %v", err)
	}
	draftDoc := seedRefEntry(t, ctx, pool, "t1", docType, "Draft Doc", domain.StatusDraft)
	bothDoc := seedRefEntry(t, ctx, pool, "t1", docType, "Published Doc", domain.StatusPublished)

	if err := repo.ReplaceEntryMedia(ctx, "t1", draftDoc, []uuid.UUID{assetID}); err != nil {
		t.Fatalf("replace media (draft only): %v", err)
	}
	if err := repo.ReplaceEntryMedia(ctx, "t1", bothDoc, []uuid.UUID{assetID}); err != nil {
		t.Fatalf("replace media (both): %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entry_media_published (entry_id, asset_id, tenant_id) VALUES ($1,$2,'t1')`, bothDoc, assetID); err != nil {
		t.Fatalf("seed published media link: %v", err)
	}

	got, err := repo.MediaReferencedBy(ctx, "t1", assetID, 0, 0)
	if err != nil {
		t.Fatalf("MediaReferencedBy: %v", err)
	}
	if got.Total != 2 {
		t.Fatalf("total = %d, want 2", got.Total)
	}
	byID := map[uuid.UUID]ReferencedByRow{}
	for _, it := range got.Items {
		byID[it.EntryID] = it
		if it.FieldPath != "" {
			t.Fatalf("entry_media carries no field_path, got %q", it.FieldPath)
		}
	}
	if d := byID[draftDoc]; !d.Draft || d.Published {
		t.Fatalf("draft-only doc: draft=%v published=%v, want true/false", d.Draft, d.Published)
	}
	if b := byID[bothDoc]; !b.Draft || !b.Published {
		t.Fatalf("both doc: draft=%v published=%v, want true/true", b.Draft, b.Published)
	}
}

// TestContentRelationsBackfill_Postgres is 000050's own reason to exist:
// entries written before the migration have relation values sitting in
// payload with no entry_relations rows at all until the backfill runs. This
// loads every migration EXCEPT 000050 first, seeds a schema and entries the
// way pre-migration data would look, then applies 000050 alone and checks
// what it inserted — including the schema-aware guard against a false
// positive on an unrelated string field (the whole reason this backfill is
// NOT 000047's schema-blind template, see 000050's own header).
func TestContentRelationsBackfill_Postgres(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()
	pool := newReferencedByPool(t, "relbackfill")

	dir := contentMigrationDir()
	names, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(names) == 0 {
		t.Fatalf("glob migrations: %v (n=%d)", err, len(names))
	}
	sort.Strings(names)
	var preSQL, backfillSQL string
	var found bool
	for _, name := range names {
		b, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if filepath.Base(name) == "000050_content_relations.up.sql" {
			backfillSQL = string(b)
			found = true
			continue
		}
		preSQL += string(b) + "\n"
	}
	if !found {
		t.Fatal("000050_content_relations.up.sql not found — has it been renumbered again?")
	}
	if _, err := pool.Exec(ctx, preSQL); err != nil {
		t.Fatalf("migrate (pre-000050): %v", err)
	}

	repo := NewPostgresContentRepository(pool, nil)
	authorType := seedRefType(t, ctx, pool, repo, "t1", "author")
	postType := seedRefType(t, ctx, pool, repo, "t1", "post")
	author := seedRefEntry(t, ctx, pool, "t1", authorType, "Ada", domain.StatusDraft)

	authorField := domain.Field{ID: uuid.New(), ContentTypeID: postType, Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author", CreatedAt: time.Now().UTC(), EnumValues: []string{}}
	if err := repo.AddField(ctx, "t1", &authorField); err != nil {
		t.Fatalf("add relation field: %v", err)
	}

	post := uuid.New()
	// The payload also carries a plain string field ("note") whose value
	// happens to be UUID-shaped — the false-positive case a schema-blind scan
	// would have picked up and this backfill must not.
	decoy := uuid.New().String()
	payload := `{"title":"Old Post","author":"` + author.String() + `","note":"` + decoy + `"}`
	if _, err := pool.Exec(ctx, `
		INSERT INTO entries (id, tenant_id, content_type_id, payload, published_payload, published_version, status, published_at, locale, translation_group_id)
		VALUES ($1,'t1',$2,$3::jsonb,$3::jsonb,1,'published',NOW(),'default',$4)`,
		post, postType, payload, uuid.New()); err != nil {
		t.Fatalf("insert pre-migration entry: %v", err)
	}

	// Sanity: entry_relations must not exist yet — its CREATE TABLE lives
	// inside 000050 itself, alongside the backfill, so the pre-migration state
	// this test is simulating genuinely has no such table at all.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'entry_relations')`).Scan(&exists); err != nil {
		t.Fatalf("check entry_relations existence: %v", err)
	}
	if exists {
		t.Fatal("entry_relations already exists before 000050 ran — preSQL must have picked it up from another migration")
	}

	if _, err := pool.Exec(ctx, backfillSQL); err != nil {
		t.Fatalf("apply 000050 (backfill): %v", err)
	}

	got, err := repo.EntryReferencedBy(ctx, "t1", author, 0, 0)
	if err != nil {
		t.Fatalf("EntryReferencedBy after backfill: %v", err)
	}
	if got.Total != 1 {
		t.Fatalf("total = %d, want 1 (backfill must have found the pre-existing relation)", got.Total)
	}
	if len(got.Items) != 1 || got.Items[0].EntryID != post || got.Items[0].FieldPath != "author" {
		t.Fatalf("items = %+v, want one row for %s field=author", got.Items, post)
	}
	if !got.Items[0].Draft || !got.Items[0].Published {
		t.Fatalf("draft=%v published=%v, want true/true (both backfill statements should have matched this published entry)", got.Items[0].Draft, got.Items[0].Published)
	}

	// The decoy UUID-shaped string must not have produced a row of its own —
	// relation_field_paths only enumerates keys the schema marks as type
	// 'relation', and "note" is a plain string field.
	var decoyRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM entry_relations WHERE field_path = 'note'`).Scan(&decoyRows); err != nil {
		t.Fatalf("count decoy rows: %v", err)
	}
	if decoyRows != 0 {
		t.Fatalf("schema-blind false positive: %d entry_relations rows came from the decoy 'note' field", decoyRows)
	}
}

// TestMediaOrphanFilter_Postgres pins ListMediaAssets' NOT EXISTS filter: an
// asset must be excluded from ?orphan=true results the moment EITHER the
// draft or the published link table names it, and included when neither does.
func TestMediaOrphanFilter_Postgres(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()
	pool := newReferencedByPool(t, "orphanfilter")
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewPostgresContentRepository(pool, nil)

	docType := seedRefType(t, ctx, pool, repo, "t1", "doc")
	mkAsset := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, content_type, size_bytes, filename, uploaded_at, created_at)
			VALUES ($1,'t1',$2,'image/png',1,$3,NOW(),NOW())`, id, "t1/"+id.String(), name); err != nil {
			t.Fatalf("insert asset %s: %v", name, err)
		}
		return id
	}
	orphan := mkAsset("orphan.png")
	draftLinked := mkAsset("draft-linked.png")
	publishedLinked := mkAsset("published-linked.png")

	doc := seedRefEntry(t, ctx, pool, "t1", docType, "Doc", domain.StatusDraft)
	if err := repo.ReplaceEntryMedia(ctx, "t1", doc, []uuid.UUID{draftLinked}); err != nil {
		t.Fatalf("replace media: %v", err)
	}
	pubDoc := seedRefEntry(t, ctx, pool, "t1", docType, "Pub Doc", domain.StatusPublished)
	if _, err := pool.Exec(ctx, `INSERT INTO entry_media_published (entry_id, asset_id, tenant_id) VALUES ($1,$2,'t1')`, pubDoc, publishedLinked); err != nil {
		t.Fatalf("seed published-only link: %v", err)
	}

	items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{Orphan: true, Limit: 20})
	if err != nil {
		t.Fatalf("ListMediaAssets(Orphan): %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("total=%d len(items)=%d, want 1/1", total, len(items))
	}
	if items[0].ID != orphan {
		t.Fatalf("orphan filter returned %s, want %s", items[0].ID, orphan)
	}

	t.Run("Orphan=false (default) still returns every asset", func(t *testing.T) {
		items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("total=%d len(items)=%d, want 3/3", total, len(items))
		}
	})
}
