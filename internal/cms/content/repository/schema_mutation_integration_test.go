package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// Schema mutation against real Postgres. EVERY data assertion lives here rather
// than in the service tests, because every one of them is a property of SQL that
// a Go fake can only restate:
//
//   - the jsonb operators (`-`, `||`, jsonb_build_object) and the CASE guards
//     wrapped around them;
//   - ON DELETE CASCADE, which has no Go analogue at all;
//   - the tenant join on the relation_entity cascade — content_type_fields has no
//     tenant_id and no RLS policy (deliberately, documented in 000014), so the
//     join is the ONLY thing standing between a rename and another tenant's data.
//
// The service tests cover the refusals, which fire before any of this runs.

// --- harness -----------------------------------------------------------------

func startContentDB(t *testing.T, dbName string) (context.Context, *pgxpool.Pool, *postgres.PostgresContainer) {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
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
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool, container
}

// baseTime is a fixed past instant so "did updated_at move" is a comparison
// against a known value rather than against a wall clock that a slow container
// start could make ambiguous.
var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// mkField builds a field for direct construction. EnumValues is []string{} and
// not nil on purpose: enum_values is NOT NULL DEFAULT '{}', and a nil slice
// encodes as SQL NULL, so a nil here fails the insert rather than defaulting.
func mkField(ctID uuid.UUID, key, typ string, order int) domain.Field {
	return domain.Field{
		ID:            uuid.New(),
		ContentTypeID: ctID,
		Key:           key,
		Type:          typ,
		EnumValues:    []string{},
		// loadFields orders by created_at, so the offsets make field order
		// deterministic instead of dependent on insert timing.
		CreatedAt: baseTime.Add(time.Duration(order) * time.Second),
	}
}

// mkType creates a content type with the given (key, type) fields.
func mkType(t *testing.T, ctx context.Context, repo *PostgresContentRepository, tenant, name string, fields ...[2]string) *domain.ContentType {
	t.Helper()
	ct := &domain.ContentType{
		ID: uuid.New(), TenantID: tenant, Name: name, Label: name,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	for i, f := range fields {
		ct.Fields = append(ct.Fields, mkField(ct.ID, f[0], f[1], i))
	}
	if err := repo.CreateContentType(ctx, ct); err != nil {
		t.Fatalf("create type %s/%s: %v", tenant, name, err)
	}
	return ct
}

// entrySeed is one row written straight through SQL. Direct inserts, not the
// repository: these tests need to pin version, published_version and the two
// payload copies independently, and CreateEntry/SetEntryPublishState deliberately
// couple them.
type entrySeed struct {
	payload          string
	version          int
	publishedPayload string // "" = draft
	publishedVersion int
}

func seedEntries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant string, ctID uuid.UUID, seeds ...entrySeed) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, len(seeds))
	for i, s := range seeds {
		id := uuid.New()
		var status string
		var publishedAt, snapshot, snapshotVersion any
		if s.publishedPayload == "" {
			status = domain.StatusDraft
		} else {
			status = domain.StatusPublished
			publishedAt, snapshot, snapshotVersion = baseTime, s.publishedPayload, s.publishedVersion
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, version,
			                     status, published_at, published_payload, published_version,
			                     created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`,
			id, tenant, ctID, s.payload, s.version, status, publishedAt, snapshot, snapshotVersion, baseTime,
		); err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// entryRow is the post-mutation state of one row, read raw.
type entryRow struct {
	payload          string
	version          int
	publishedPayload *string
	publishedVersion *int
	updatedAt        time.Time
}

func readEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) entryRow {
	t.Helper()
	var r entryRow
	if err := pool.QueryRow(ctx, `
		SELECT payload::text, version, published_payload::text, published_version, updated_at
		FROM entries WHERE id = $1`, id).
		Scan(&r.payload, &r.version, &r.publishedPayload, &r.publishedVersion, &r.updatedAt); err != nil {
		t.Fatalf("read entry %s: %v", id, err)
	}
	return r
}

func typeUpdatedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM content_types WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return ts
}

func hasKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, column, key string) bool {
	t.Helper()
	var ok bool
	//nolint:gosec // column is a test-local literal, never caller input
	if err := pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COALESCE(jsonb_exists(%s, $2), FALSE) FROM entries WHERE id = $1`, column),
		id, key).Scan(&ok); err != nil {
		t.Fatalf("jsonb_exists(%s, %s): %v", column, key, err)
	}
	return ok
}

// --- the data migrations ------------------------------------------------------

func TestSchemaMutation_EntryDataMigration(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "schemamut")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "t1"
	now := baseTime.Add(time.Hour)

	t.Run("field delete strips BOTH copies and bumps only the copies that held the key", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "post", [2]string{"title", domain.FieldTypeString}, [2]string{"body", domain.FieldTypeText})
		ids := seedEntries(t, ctx, pool, tenant, ct.ID,
			// draft holding the key in its only copy
			entrySeed{payload: `{"title":"a","body":"x"}`, version: 3},
			// published: key ONLY in the snapshot — the working copy already
			// dropped it, but delivery is still serving it
			entrySeed{payload: `{"title":"b"}`, version: 5, publishedPayload: `{"title":"b","body":"y"}`, publishedVersion: 5},
			// published: key in both copies
			entrySeed{payload: `{"title":"c","body":"z"}`, version: 7, publishedPayload: `{"title":"c","body":"z"}`, publishedVersion: 7},
			// published: never held the key at all
			entrySeed{payload: `{"title":"d"}`, version: 9, publishedPayload: `{"title":"d"}`, publishedVersion: 9},
			// published, and the key is the ONLY thing in the snapshot: the strip
			// must leave `{}`, not NULL, or entries_published_snapshot_check fires
			entrySeed{payload: `{"body":"lone"}`, version: 11, publishedPayload: `{"body":"lone"}`, publishedVersion: 11},
		)
		body, _ := ct.FieldByKey("body")
		if err := repo.DeleteField(ctx, tenant, ct, body, schemaAdmin, now); err != nil {
			t.Fatalf("delete field: %v", err)
		}

		// 1. draft: stripped, version bumped, and the snapshot columns stay
		// (NULL, NULL) — `NULL - 'body'` is NULL, which is what keeps the CHECK
		// satisfied for a row that was never published.
		got := readEntry(t, ctx, pool, ids[0])
		if got.payload != `{"title": "a"}` {
			t.Fatalf("draft payload = %s, want the key stripped", got.payload)
		}
		if got.version != 4 {
			t.Fatalf("draft version = %d, want 4", got.version)
		}
		if got.publishedPayload != nil || got.publishedVersion != nil {
			t.Fatalf("a draft must stay (NULL, NULL), got (%v, %v)", got.publishedPayload, got.publishedVersion)
		}

		// 2. snapshot-only: published_version moves, `version` must NOT — an admin
		// with an open editor would otherwise get a spurious 409 for a change that
		// did not touch the copy they are editing.
		got = readEntry(t, ctx, pool, ids[1])
		if got.version != 5 {
			t.Fatalf("version = %d, want 5 — the working copy never held the key", got.version)
		}
		if got.publishedVersion == nil || *got.publishedVersion != 6 {
			t.Fatalf("published_version = %v, want 6", got.publishedVersion)
		}
		if hasKey(t, ctx, pool, ids[1], "published_payload", "body") {
			t.Fatal("the snapshot still serves the deleted field")
		}

		// 3. both copies: both counters move.
		got = readEntry(t, ctx, pool, ids[2])
		if got.version != 8 || got.publishedVersion == nil || *got.publishedVersion != 8 {
			t.Fatalf("versions = (%d, %v), want (8, 8)", got.version, got.publishedVersion)
		}

		// 4. untouched row: BOTH numbers stand still, and so does updated_at.
		// published_version is what a delivery consumer watches for change
		// (ADR-006 Am.1a); moving it while the snapshot stands still is exactly
		// the lie that amendment closed.
		got = readEntry(t, ctx, pool, ids[3])
		if got.version != 9 || got.publishedVersion == nil || *got.publishedVersion != 9 {
			t.Fatalf("a row that never held the key changed versions: (%d, %v)", got.version, got.publishedVersion)
		}
		if !got.updatedAt.Equal(baseTime) {
			t.Fatalf("updated_at moved on a row the delete did not touch: %v", got.updatedAt)
		}

		// 5. emptied snapshot: `{}`, not NULL. A NULL here would violate
		// entries_published_snapshot_check and the whole statement would have
		// rolled back — so reaching this assertion at all is half the proof.
		got = readEntry(t, ctx, pool, ids[4])
		if got.publishedPayload == nil || *got.publishedPayload != `{}` {
			t.Fatalf("emptied snapshot = %v, want {}", got.publishedPayload)
		}
		var stillPublished string
		if err := pool.QueryRow(ctx, `SELECT status FROM entries WHERE id = $1`, ids[4]).Scan(&stillPublished); err != nil {
			t.Fatal(err)
		}
		if stillPublished != domain.StatusPublished {
			t.Fatalf("status = %q — the row must remain published with an empty snapshot", stillPublished)
		}

		// The definition is gone too, in the same transaction.
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM content_type_fields WHERE content_type_id = $1 AND key = 'body'`, ct.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("the field definition survived its own delete")
		}
	})

	t.Run("rename moves the key in both copies and materialises nothing", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "note", [2]string{"title", domain.FieldTypeString}, [2]string{"body", domain.FieldTypeText})
		ids := seedEntries(t, ctx, pool, tenant, ct.ID,
			entrySeed{payload: `{"title":"a","body":"x"}`, version: 1},
			entrySeed{payload: `{"title":"b"}`, version: 2, publishedPayload: `{"title":"b","body":"y"}`, publishedVersion: 2},
			entrySeed{payload: `{"title":"c"}`, version: 3, publishedPayload: `{"title":"c"}`, publishedVersion: 3},
		)
		if err := repo.RenameField(ctx, tenant, ct, "body", "content", schemaAdmin, now); err != nil {
			t.Fatalf("rename field: %v", err)
		}

		got := readEntry(t, ctx, pool, ids[0])
		if got.payload != `{"title": "a", "content": "x"}` {
			t.Fatalf("payload = %s — the value must move with the key", got.payload)
		}
		if got.version != 2 {
			t.Fatalf("version = %d, want 2", got.version)
		}

		// The snapshot-only holder: the rename lands in published_payload and the
		// WORKING copy must not acquire the new key at all. Without the CASE
		// guard, jsonb_build_object($new, payload -> $old) yields {"content": null}
		// here — a silent document change on a copy the rename should not touch.
		got = readEntry(t, ctx, pool, ids[1])
		if hasKey(t, ctx, pool, ids[1], "payload", "content") {
			t.Fatalf("payload = %s — a copy that lacked the key must not gain {\"content\": null}", got.payload)
		}
		if got.version != 2 {
			t.Fatalf("version = %d, want 2 — the working copy did not change", got.version)
		}
		if got.publishedPayload == nil || *got.publishedPayload != `{"title": "b", "content": "y"}` {
			t.Fatalf("snapshot = %v", got.publishedPayload)
		}
		if got.publishedVersion == nil || *got.publishedVersion != 3 {
			t.Fatalf("published_version = %v, want 3", got.publishedVersion)
		}

		// A row that held the key in NEITHER copy is not touched at all.
		got = readEntry(t, ctx, pool, ids[2])
		if hasKey(t, ctx, pool, ids[2], "payload", "content") || hasKey(t, ctx, pool, ids[2], "published_payload", "content") {
			t.Fatal("a row that never held the old key gained the new one")
		}
		if got.version != 3 || got.publishedVersion == nil || *got.publishedVersion != 3 {
			t.Fatalf("untouched row changed versions: (%d, %v)", got.version, got.publishedVersion)
		}
		if !got.updatedAt.Equal(baseTime) {
			t.Fatalf("updated_at moved on an untouched row: %v", got.updatedAt)
		}

		// And the definition moved.
		var key string
		if err := pool.QueryRow(ctx,
			`SELECT key FROM content_type_fields WHERE content_type_id = $1 AND key = 'content'`, ct.ID).Scan(&key); err != nil {
			t.Fatalf("the field definition did not move: %v", err)
		}
	})

	t.Run("CountEntriesMissingField treats an explicit null as missing", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "survey", [2]string{"title", domain.FieldTypeString}, [2]string{"answer", domain.FieldTypeText})
		seedEntries(t, ctx, pool, tenant, ct.ID,
			entrySeed{payload: `{"title":"a","answer":"yes"}`, version: 1}, // present
			entrySeed{payload: `{"title":"b"}`, version: 1},                // absent
			entrySeed{payload: `{"title":"c","answer":null}`, version: 1},  // explicit null
			entrySeed{payload: `{"title":"d","answer":""}`, version: 1},    // empty string IS a value
			entrySeed{payload: `{"title":"e","answer":false}`, version: 1}, // so is false
		)
		n, err := repo.CountEntriesMissingField(ctx, tenant, ct.ID, "answer")
		if err != nil {
			t.Fatal(err)
		}
		// 2, not 1: validatePayload's test is `!present || v == nil`, so the
		// explicit null must count. This is the exact place a bare jsonb_exists
		// disagrees with the validator, and it disagrees on precisely the rows
		// that make tightening `required` unsafe.
		if n != 2 {
			t.Fatalf("CountEntriesMissingField = %d, want 2 (absent + explicit null)", n)
		}
		// Guard: prove the bare test really would answer differently, or the
		// number above could be right for an unrelated reason.
		var bare int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM entries WHERE content_type_id = $1 AND NOT jsonb_exists(payload, 'answer')`, ct.ID).
			Scan(&bare); err != nil {
			t.Fatal(err)
		}
		if bare != 1 {
			t.Fatalf("guard: a bare key-existence test should see %d, saw %d — the fixture no longer distinguishes the two", 1, bare)
		}
	})

	t.Run("CountEntriesMissingField consults only the working copy", func(t *testing.T) {
		// Tightening `required` is about what an EDITOR must now supply. The
		// snapshot is not writable, so a value living only there does not make the
		// next PATCH succeed.
		ct := mkType(t, ctx, repo, tenant, "poll", [2]string{"title", domain.FieldTypeString}, [2]string{"answer", domain.FieldTypeText})
		seedEntries(t, ctx, pool, tenant, ct.ID,
			entrySeed{payload: `{"title":"a"}`, version: 1, publishedPayload: `{"title":"a","answer":"y"}`, publishedVersion: 1},
		)
		n, err := repo.CountEntriesMissingField(ctx, tenant, ct.ID, "answer")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("= %d, want 1 — a value only in the snapshot does not satisfy a new `required`", n)
		}
	})

	t.Run("CountEntriesWithField and the enum guard span BOTH copies", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "ticket", [2]string{"state", domain.FieldTypeEnum})
		seedEntries(t, ctx, pool, tenant, ct.ID,
			entrySeed{payload: `{}`, version: 1, publishedPayload: `{"state":"gone"}`, publishedVersion: 1},
		)
		n, err := repo.CountEntriesWithField(ctx, tenant, ct.ID, "state")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("CountEntriesWithField = %d — a key living only in the snapshot is still data", n)
		}
		state, _ := ct.FieldByKey("state")
		n, err = repo.CountEntriesWithValuesOutside(ctx, tenant, ct.ID, state, []string{"open", "closed"})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("CountEntriesWithValuesOutside = %d — the published document must still validate too", n)
		}
	})

	t.Run("the enum guard is element-wise for a multi-valued field", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "tagged", [2]string{"tags", domain.FieldTypeEnum})
		if _, err := pool.Exec(ctx,
			`UPDATE content_type_fields SET multiple = TRUE WHERE content_type_id = $1 AND key = 'tags'`, ct.ID); err != nil {
			t.Fatal(err)
		}
		reloaded, err := repo.GetContentTypeByName(ctx, tenant, "tagged")
		if err != nil {
			t.Fatal(err)
		}
		tags, _ := reloaded.FieldByKey("tags")
		if !tags.Multiple {
			t.Fatal("guard: the field must be multi-valued or the array arm is not the one under test")
		}
		seedEntries(t, ctx, pool, tenant, ct.ID,
			entrySeed{payload: `{"tags":["ai","sql"]}`, version: 1},
			// A SCALAR left over from before the field became multi-valued.
			// jsonb_array_elements_text would raise on it, which is why the array
			// arm guards the shape first — an error here is a 500, not a refusal.
			entrySeed{payload: `{"tags":"legacy"}`, version: 1},
		)
		n, err := repo.CountEntriesWithValuesOutside(ctx, tenant, reloaded.ID, tags, []string{"ai", "go"})
		if err != nil {
			t.Fatalf("the scalar row must not raise: %v", err)
		}
		if n != 1 {
			t.Fatalf("= %d, want 1 — the SECOND element is what falls outside the set", n)
		}
	})
}

// --- media links --------------------------------------------------------------

// entry_media is only ever rewritten by an entry write, so without relinkEntryMedia
// a deleted file field leaves its links behind, AssetIsPublished keeps answering
// true, and the signed-URL gate stays open on bytes nothing references any more
// (ADR-005's invariant).
func TestSchemaMutation_FileFieldDeleteRelinksMedia(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "schemamedia")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "t1"
	now := baseTime.Add(time.Hour)

	ct := mkType(t, ctx, repo, tenant, "gallery",
		[2]string{"title", domain.FieldTypeString},
		[2]string{"cover", domain.FieldTypeFile},
		[2]string{"attachment", domain.FieldTypeFile},
	)
	cover, attachment := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{cover, attachment} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, uploaded_at)
			VALUES ($1,$2,$3, NOW())`, id, tenant, tenant+"/"+id.String()); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
	}
	payload := fmt.Sprintf(`{"title":"g","cover":"%s","attachment":"%s"}`, cover, attachment)
	ids := seedEntries(t, ctx, pool, tenant, ct.ID,
		entrySeed{payload: payload, version: 1, publishedPayload: payload, publishedVersion: 1},
	)
	for _, table := range []string{"entry_media", "entry_media_published"} {
		for _, asset := range []uuid.UUID{cover, attachment} {
			//nolint:gosec // table is a test-local literal
			if _, err := pool.Exec(ctx,
				fmt.Sprintf(`INSERT INTO %s (entry_id, asset_id, tenant_id) VALUES ($1,$2,$3)`, table),
				ids[0], asset, tenant); err != nil {
				t.Fatalf("seed link: %v", err)
			}
		}
	}

	linked := func(table string, asset uuid.UUID) bool {
		t.Helper()
		var n int
		//nolint:gosec // table is a test-local literal
		if err := pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE entry_id = $1 AND asset_id = $2`, table),
			ids[0], asset).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	// Guard: both assets start public, or "it flipped to false" proves nothing.
	for _, asset := range []uuid.UUID{cover, attachment} {
		if ok, err := repo.AssetIsPublished(ctx, tenant, asset); err != nil || !ok {
			t.Fatalf("guard: asset %s must start published (ok=%v err=%v)", asset, ok, err)
		}
	}

	t.Run("deleting a NON-file field leaves every link alone", func(t *testing.T) {
		title, _ := ct.FieldByKey("title")
		if err := repo.DeleteField(ctx, tenant, ct, title, schemaAdmin, now); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"entry_media", "entry_media_published"} {
			for _, asset := range []uuid.UUID{cover, attachment} {
				if !linked(table, asset) {
					t.Fatalf("%s lost %s — a text field's delete must not touch media links", table, asset)
				}
			}
		}
	})

	t.Run("deleting a file field removes its links from BOTH tables", func(t *testing.T) {
		// The pre-delete type is passed, exactly as the service does: the survivor
		// set is computed from the fields OTHER than the one leaving.
		after := *ct
		after.Fields = nil
		for _, f := range ct.Fields {
			if f.Key != "title" { // already deleted above
				after.Fields = append(after.Fields, f)
			}
		}
		coverField, _ := after.FieldByKey("cover")
		if err := repo.DeleteField(ctx, tenant, &after, coverField, schemaAdmin, now); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"entry_media", "entry_media_published"} {
			if linked(table, cover) {
				t.Fatalf("%s still links the deleted file field's asset", table)
			}
			if !linked(table, attachment) {
				t.Fatalf("%s dropped a SURVIVING file field's asset — the rebuild is over-broad", table)
			}
		}

		// The gate the whole media delivery path rests on.
		if ok, err := repo.AssetIsPublished(ctx, tenant, cover); err != nil || ok {
			t.Fatalf("the deleted field's asset is still publicly readable (ok=%v err=%v)", ok, err)
		}
		if ok, err := repo.AssetIsPublished(ctx, tenant, attachment); err != nil || !ok {
			t.Fatalf("the surviving field's asset lost access (ok=%v err=%v)", ok, err)
		}
		// The bytes' metadata row is NOT destroyed: unlinking an asset is not
		// deleting it, and the tenant's library must survive a schema edit.
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM media_assets WHERE id = $1`, cover).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatal("the media asset row was destroyed by a field delete")
		}
	})
}

// --- the cross-tenant case ----------------------------------------------------

// The single most important test in this file.
//
// relation_entity stores the type NAME, so renaming a type has to rewrite every
// field pointing at the old one. content_type_fields has NO tenant_id and NO RLS
// policy — deliberately, and documented in migration 000014 — so the
// obvious-looking `UPDATE content_type_fields SET relation_entity = $new WHERE
// relation_entity = $old` rewrites EVERY other tenant's fields that happen to use
// the same type name. Type names are only unique per tenant, so "article" is about
// as likely a collision as exists. That is cross-tenant data corruption arriving
// through the exact gap 000014 chose to accept, and the join through content_types
// is the only thing preventing it.
func TestSchemaMutation_TypeRenameCascadeIsTenantScoped(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "schemarename")
	repo := NewPostgresContentRepository(pool, nil)
	now := baseTime.Add(time.Hour)

	// Two tenants, identical schemas: the same type name, and a relation field in
	// each pointing at it.
	setup := func(tenant string) (*domain.ContentType, *domain.ContentType) {
		article := mkType(t, ctx, repo, tenant, "article", [2]string{"title", domain.FieldTypeString})
		comment := &domain.ContentType{
			ID: uuid.New(), TenantID: tenant, Name: "comment", Label: "Comment",
			CreatedAt: baseTime, UpdatedAt: baseTime,
		}
		rel := mkField(comment.ID, "about", domain.FieldTypeRelation, 0)
		rel.RelationEntity = "article"
		text := mkField(comment.ID, "text", domain.FieldTypeString, 1)
		// A NON-relation field whose relation_entity happens to hold the same
		// string. The cascade filters on field_type, so it must be left alone.
		decoy := mkField(comment.ID, "decoy", domain.FieldTypeString, 2)
		decoy.RelationEntity = "article"
		comment.Fields = []domain.Field{rel, text, decoy}
		if err := repo.CreateContentType(ctx, comment); err != nil {
			t.Fatalf("create comment for %s: %v", tenant, err)
		}
		return article, comment
	}
	articleA, commentA := setup("tenant-a")
	articleB, commentB := setup("tenant-b")

	relationEntity := func(ctID uuid.UUID, key string) string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT relation_entity FROM content_type_fields WHERE content_type_id = $1 AND key = $2`,
			ctID, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := repo.RenameContentType(ctx, "tenant-a", articleA.ID, "article", "story", now); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Tenant A: the rename landed, and its referrer followed.
	var nameA string
	if err := pool.QueryRow(ctx, `SELECT name FROM content_types WHERE id = $1`, articleA.ID).Scan(&nameA); err != nil {
		t.Fatal(err)
	}
	if nameA != "story" {
		t.Fatalf("tenant-a's type name = %q, want story", nameA)
	}
	if got := relationEntity(commentA.ID, "about"); got != "story" {
		t.Fatalf("tenant-a's referrer = %q, want story — checkRelations resolves this per write", got)
	}

	// Tenant B: UNTOUCHED. This is the assertion the whole test exists for.
	var nameB string
	if err := pool.QueryRow(ctx, `SELECT name FROM content_types WHERE id = $1`, articleB.ID).Scan(&nameB); err != nil {
		t.Fatal(err)
	}
	if nameB != "article" {
		t.Fatalf("tenant-b's type was renamed by tenant-a's request: %q", nameB)
	}
	if got := relationEntity(commentB.ID, "about"); got != "article" {
		t.Fatalf("tenant-a's rename rewrote TENANT-B's relation_entity to %q — cross-tenant corruption "+
			"through content_type_fields, which has no tenant_id and no RLS", got)
	}

	// The field_type filter: a non-relation field carrying the same string is not
	// a referrer and must not be rewritten.
	if got := relationEntity(commentA.ID, "decoy"); got != "article" {
		t.Fatalf("a non-relation field was rewritten: %q", got)
	}

	// The referring type's DTO changed, so its timestamp moves with it — but only
	// within the tenant, same join, same reason.
	if ts := typeUpdatedAt(t, ctx, pool, commentA.ID); !ts.Equal(now) {
		t.Fatalf("tenant-a's referring type updated_at = %v, want %v", ts, now)
	}
	if ts := typeUpdatedAt(t, ctx, pool, commentB.ID); !ts.Equal(baseTime) {
		t.Fatalf("tenant-b's referring type was touched: %v", ts)
	}

	// ListRelationReferrers — the read side of the same question — is scoped too.
	refs, err := repo.ListRelationReferrers(ctx, "tenant-b", "article")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].TypeName != "comment" || refs[0].FieldKey != "about" {
		t.Fatalf("tenant-b's referrers = %+v, want exactly its own comment.about", refs)
	}
	if refs, err := repo.ListRelationReferrers(ctx, "tenant-a", "article"); err != nil {
		t.Fatal(err)
	} else if len(refs) != 0 {
		t.Fatalf("tenant-a still reports referrers for the OLD name: %+v", refs)
	}
}

// --- cascade under the role RLS actually applies to ---------------------------

// Cascade actions bypass RLS, so a type delete is one of the few places the
// database does more than the statement says. Run as the non-superuser role the
// way rls_integration_test.go does — under a superuser the cascade would run
// regardless, and the test would prove nothing about production.
func TestSchemaMutation_TypeDeleteCascadesUnderNonSuperuser(t *testing.T) {
	ctx, super, container := startContentDB(t, "schemacascade")

	superRepo := NewPostgresContentRepository(super, nil)
	victim := mkType(t, ctx, superRepo, "tenant-a", "post",
		[2]string{"title", domain.FieldTypeString}, [2]string{"cover", domain.FieldTypeFile})
	// A same-named type in ANOTHER tenant: the delete must not reach it.
	bystander := mkType(t, ctx, superRepo, "tenant-b", "post", [2]string{"title", domain.FieldTypeString})
	bystanderEntries := seedEntries(t, ctx, super, "tenant-b", bystander.ID, entrySeed{payload: `{"title":"b"}`, version: 1})

	asset := uuid.New()
	if _, err := super.Exec(ctx, `
		INSERT INTO media_assets (id, tenant_id, storage_key, uploaded_at) VALUES ($1,'tenant-a',$2, NOW())`,
		asset, "tenant-a/"+asset.String()); err != nil {
		t.Fatal(err)
	}
	entries := seedEntries(t, ctx, super, "tenant-a", victim.ID,
		entrySeed{payload: fmt.Sprintf(`{"title":"a","cover":"%s"}`, asset), version: 1,
			publishedPayload: fmt.Sprintf(`{"title":"a","cover":"%s"}`, asset), publishedVersion: 1},
	)
	for _, table := range []string{"entry_media", "entry_media_published"} {
		//nolint:gosec // table is a test-local literal
		if _, err := super.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (entry_id, asset_id, tenant_id) VALUES ($1,$2,'tenant-a')`, table),
			entries[0], asset); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := super.Exec(ctx, `
		CREATE ROLE rlsapp LOGIN PASSWORD 'rlspw' NOSUPERUSER;
		GRANT USAGE ON SCHEMA public TO rlsapp;
		GRANT SELECT, INSERT, UPDATE, DELETE ON
			content_types, entries, content_type_fields,
			media_assets, entry_media, entry_media_published TO rlsapp;
	`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	app, err := pgxpool.New(ctx, "postgres://rlsapp:rlspw@"+host+":"+port.Port()+"/schemacascade?sslmode=disable")
	if err != nil {
		t.Fatalf("connect as rlsapp: %v", err)
	}
	defer app.Close()

	appRepo := NewPostgresContentRepository(app, nil)

	// Guard: RLS really is in force for this role, or the delete below would be
	// running as an effective superuser and the cascade claim would be untested.
	if err := appRepo.DeleteContentType(ctx, "tenant-a", bystander.ID); err == nil {
		t.Fatal("guard: tenant-a must not be able to delete tenant-b's type")
	}

	if err := appRepo.DeleteContentType(ctx, "tenant-a", victim.ID); err != nil {
		t.Fatalf("delete content type as the non-superuser role: %v", err)
	}

	// Verified through the SUPERUSER pool: the question is whether the rows exist
	// at all, not whether this role can see them.
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := super.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM content_types WHERE id = $1`, victim.ID); n != 0 {
		t.Fatal("the content type survived its own delete")
	}
	if n := count(`SELECT COUNT(*) FROM content_type_fields WHERE content_type_id = $1`, victim.ID); n != 0 {
		t.Fatalf("%d field definitions survived — ON DELETE CASCADE did not reach content_type_fields", n)
	}
	// entries is RLS'd with FORCE, and the cascade still has to clear it. This is
	// the half that would silently leave orphans if referential actions honoured
	// the policy.
	if n := count(`SELECT COUNT(*) FROM entries WHERE content_type_id = $1`, victim.ID); n != 0 {
		t.Fatalf("%d entries survived the type delete", n)
	}
	for _, table := range []string{"entry_media", "entry_media_published"} {
		//nolint:gosec // table is a test-local literal
		if n := count(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE entry_id = $1`, table), entries[0]); n != 0 {
			t.Fatalf("%s rows survived — the asset stays reachable through a link to a deleted entry", table)
		}
	}
	// The asset ITSELF is not destroyed: deleting a collection is not emptying the
	// media library.
	if n := count(`SELECT COUNT(*) FROM media_assets WHERE id = $1`, asset); n != 1 {
		t.Fatal("the media asset was destroyed by a content type delete")
	}
	// And the other tenant is entirely untouched.
	if n := count(`SELECT COUNT(*) FROM content_types WHERE id = $1`, bystander.ID); n != 1 {
		t.Fatal("another tenant's same-named type was deleted")
	}
	if n := count(`SELECT COUNT(*) FROM entries WHERE id = $1`, bystanderEntries[0]); n != 1 {
		t.Fatal("another tenant's entries were cascaded away")
	}
}

// --- updated_at ----------------------------------------------------------------

// content_types.updated_at was written once at insert and never again, while the
// DTO exposed it — so a type reported a timestamp frozen at created_at even as its
// fields changed. Every mutation now moves it, and every path is asserted because
// a missed one is invisible: the response still carries a plausible timestamp.
func TestSchemaMutation_EveryPathTouchesTheTypeTimestamp(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "schematouch")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "t1"

	ct := mkType(t, ctx, repo, tenant, "post",
		[2]string{"title", domain.FieldTypeString}, [2]string{"body", domain.FieldTypeText})
	if ts := typeUpdatedAt(t, ctx, pool, ct.ID); !ts.Equal(baseTime) {
		t.Fatalf("guard: updated_at must start at %v, got %v", baseTime, ts)
	}

	steps := []struct {
		name string
		run  func(now time.Time) error
	}{
		{"AddField", func(now time.Time) error {
			f := mkField(ct.ID, "extra", domain.FieldTypeString, 9)
			f.CreatedAt = now
			return repo.AddField(ctx, tenant, &f)
		}},
		{"UpdateFieldDefinition", func(now time.Time) error {
			f, _ := ct.FieldByKey("body")
			f.Label = "Body"
			return repo.UpdateFieldDefinition(ctx, tenant, ct, f, now)
		}},
		{"RenameField", func(now time.Time) error {
			return repo.RenameField(ctx, tenant, ct, "body", "content", schemaAdmin, now)
		}},
		{"DeleteField", func(now time.Time) error {
			f, _ := ct.FieldByKey("title")
			return repo.DeleteField(ctx, tenant, ct, f, schemaAdmin, now)
		}},
		{"UpdateContentTypeDefinition", func(now time.Time) error {
			next := *ct
			next.Label = "Posts"
			return repo.UpdateContentTypeDefinition(ctx, tenant, &next, now)
		}},
		{"RenameContentType", func(now time.Time) error {
			return repo.RenameContentType(ctx, tenant, ct.ID, "post", "story", now)
		}},
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			// Distinct instants so a step that leaves the column alone is caught
			// rather than reading as "the previous step's value is still correct".
			now := baseTime.Add(time.Duration(i+1) * time.Hour)
			if err := step.run(now); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			if ts := typeUpdatedAt(t, ctx, pool, ct.ID); !ts.Equal(now) {
				t.Fatalf("%s left content_types.updated_at at %v, want %v", step.name, ts, now)
			}
		})
	}
}

// --- migration 000024 ----------------------------------------------------------

// field_type had never been constrained in the DATABASE — domain.ValidFieldType
// at definition time was the only gate, unlike status (000016) and locale
// (000018). Schema mutation multiplies the ways a field definition gets written,
// so the set is pinned where it cannot be bypassed.
func TestSchemaMutation_FieldTypeCheckConstraint(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "schemacheck")
	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatal(err)
	}
	insert := func(fieldType string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO content_type_fields (id, content_type_id, key, field_type) VALUES ($1,$2,$3,$4)`,
			uuid.New(), typeID, "k_"+fieldType, fieldType)
		return err
	}

	if err := insert("bogus"); err == nil {
		t.Fatal("expected content_type_fields_field_type_check to reject field_type='bogus'")
	}
	// Case matters, and so does whitespace: a CHECK that used a case-insensitive
	// or trimmed comparison would let 'String' through to validateScalar's
	// fail-closed default arm, which refuses every value in the field.
	for _, bad := range []string{"String", "STRING", " string", "string "} {
		if err := insert(bad); err == nil {
			t.Fatalf("field_type=%q must be refused", bad)
		}
	}
	// And every type the application recognises must still insert, or the
	// constraint is narrower than the domain and AddField starts 500ing.
	for _, good := range domain.AllowedFieldTypes() {
		if err := insert(good); err != nil {
			t.Fatalf("field_type=%q is legal in Go but refused by the DB: %v", good, err)
		}
	}
}

// schemaAdmin is the actor the bulk schema statements now take (ADR-014 §5,
// william 0806). A fixed human principal rather than a fresh uuid per call, so
// a test that wants to assert WHO the revision names has a value to compare
// against instead of one it has to read back off the row.
var schemaAdmin = domain.WriteActor{
	Kind:   domain.ActorKindHuman,
	UserID: func() *uuid.UUID { id := uuid.MustParse("00000000-0000-0000-0000-0000000000ad"); return &id }(),
}

// Bulk schema mutations and the revision history (ADR-014 §5, william 0806).
//
// The gap this closes was named in migration 000034's own header: DeleteField
// and RenameField change `payload` in bulk and used to write nothing here, which
// left the sparse read rule ("newest revision with version <= N") returning a
// payload that still held the deleted key — a WRONG answer rather than a missing
// one. Closing it needed a ruling on who authors a bulk change, because copying
// each row's previous editor would attribute a schema admin's deletion to
// whoever last edited each entry. william ruled: thread the actor through.
//
// MUTATION VERIFICATION (actually run 2026-08-06). Four mutations, each landing
// on a different assertion, each red alone:
//
//   - Drop DeleteField's `JOIN working` → "duplicate key value violates unique
//     constraint entry_revisions_pkey". The join is a correctness guard, not a
//     filter: the statement touches rows whose version does NOT move, and a
//     revision for one of those re-inserts a version that already has a row.
//   - Drop RenameField's `JOIN working` → the same violation from the other
//     statement. Covered separately because it is a separate copy of the guard;
//     with only the delete case tested, this mutation stays green.
//   - Stamp the provenance columns unconditionally → the published-only row
//     reports the admin as its last writer, and the working copy it claims to
//     describe was never written.
//   - Copy each row's existing author into the revision instead of the actor
//     (COALESCE(updated_by, $5)) → "author_user_id = ...ed, want ...ad". THIS IS
//     THE ONE THAT MATTERS: it reproduces exactly the false answer the ruling
//     exists to prevent, and it is invisible unless priorEditor and schemaAdmin
//     are different uuids. A fixture where they coincide makes the bug and the
//     fix produce identical rows.
func TestBulkSchemaMutationsRecordRevisions(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "bulkrevisions")
	repo := NewPostgresContentRepository(pool, nil)
	const tenant = "t"

	// Entries are created through the repository, not seedEntries, and that is
	// load-bearing for the last subtest: only the real write paths leave a
	// revision at the row's CURRENT version, which is what a second insert at
	// that version would collide with.
	// priorEditor is NOT schemaAdmin, and the difference is the whole point of
	// these assertions. The false answer this ruling exists to prevent is a bulk
	// revision copying each row's PREVIOUS editor; with one uuid playing both
	// parts, that bug and the correct behaviour produce identical rows and every
	// assertion below would hold against either.
	priorEditor := uuid.MustParse("00000000-0000-0000-0000-0000000000ed")
	mkEntry := func(t *testing.T, ct *domain.ContentType, payload string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		human := domain.ActorKindHuman
		e := &domain.Entry{
			ID: id, TenantID: tenant, ContentTypeID: ct.ID,
			Payload:            json.RawMessage(payload),
			Version:            1,
			Status:             domain.StatusDraft,
			Locale:             domain.DefaultLocale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &priorEditor,
			UpdatedBy:          &priorEditor,
			UpdatedByKind:      &human,
			CreatedAt:          baseTime, UpdatedAt: baseTime,
		}
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatalf("create entry: %v", err)
		}
		return id
	}
	revisions := func(t *testing.T, id uuid.UUID) []domain.EntryRevision {
		t.Helper()
		revs, err := repo.ListEntryRevisions(ctx, tenant, id)
		if err != nil {
			t.Fatalf("list revisions: %v", err)
		}
		return revs
	}
	payloadHasKey := func(t *testing.T, raw json.RawMessage, key string) bool {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("payload is not an object: %v (%s)", err, raw)
		}
		_, ok := doc[key]
		return ok
	}

	t.Run("delete_field records the stripped payload against the schema admin", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "post_del", [2]string{"title", "string"}, [2]string{"body", "string"})
		id := mkEntry(t, ct, `{"title":"keep","body":"gone"}`)

		body, ok := ct.FieldByKey("body")
		if !ok {
			t.Fatal("fixture has no body field")
		}
		at := baseTime.Add(time.Hour)
		if err := repo.DeleteField(ctx, tenant, ct, body, schemaAdmin, at); err != nil {
			t.Fatalf("delete field: %v", err)
		}

		revs := revisions(t, id)
		if len(revs) != 2 {
			t.Fatalf("want 2 revisions (create + bulk delete), got %d", len(revs))
		}
		// THE POINT: the newest revision is the POST-strip payload. Before this
		// change there was no second row at all, so the read rule resolved every
		// version after the deletion to the create revision — which still held
		// the deleted key.
		if payloadHasKey(t, revs[0].Payload, "body") {
			t.Fatalf("newest revision still holds the deleted key: %s", revs[0].Payload)
		}
		if !payloadHasKey(t, revs[0].Payload, "title") {
			t.Fatalf("the bulk revision lost an unrelated key: %s", revs[0].Payload)
		}
		// And the history is still a history: the pre-delete value is readable.
		if !payloadHasKey(t, revs[1].Payload, "body") {
			t.Fatalf("the pre-delete revision lost the key it is there to preserve: %s", revs[1].Payload)
		}
		// The bulk revision names the ADMIN who ran the schema change...
		requireAuthor(t, revs[0], domain.ActorKindHuman, schemaAdmin.UserID, nil)
		// ...and the revision beneath it still names the person who wrote that
		// content. Both halves are needed: the first alone passes against code
		// that stamps the admin everywhere, the second alone passes against the
		// old behaviour of copying the previous editor forward.
		requireAuthor(t, revs[1], domain.ActorKindHuman, &priorEditor, nil)
		if !revs[0].CreatedAt.Equal(at) {
			t.Fatalf("bulk revision is stamped %s, want the mutation's own instant %s", revs[0].CreatedAt, at)
		}
	})

	t.Run("rename_field records the moved key", func(t *testing.T) {
		ct := mkType(t, ctx, repo, tenant, "post_ren", [2]string{"body", "string"})
		id := mkEntry(t, ct, `{"body":"text"}`)

		if err := repo.RenameField(ctx, tenant, ct, "body", "content", schemaAdmin, baseTime.Add(time.Hour)); err != nil {
			t.Fatalf("rename field: %v", err)
		}
		revs := revisions(t, id)
		if len(revs) != 2 {
			t.Fatalf("want 2 revisions (create + rename), got %d", len(revs))
		}
		if payloadHasKey(t, revs[0].Payload, "body") || !payloadHasKey(t, revs[0].Payload, "content") {
			t.Fatalf("newest revision does not show the rename: %s", revs[0].Payload)
		}
		requireAuthor(t, revs[0], domain.ActorKindHuman, schemaAdmin.UserID, nil)
	})

	t.Run("an entry whose working copy lacks the key gets no revision and no error", func(t *testing.T) {
		// The collision case, and it is reachable rather than theoretical:
		// publish, then remove the key from the working copy, and the snapshot
		// still holds it. The bulk statement then touches this row (the WHERE
		// spans both copies) while `version` stands still — so an unguarded
		// insert re-inserts the version the last update already recorded, and
		// entry_revisions has no ON CONFLICT to swallow it.
		ct := mkType(t, ctx, repo, tenant, "post_pub", [2]string{"title", "string"}, [2]string{"body", "string"})
		id := mkEntry(t, ct, `{"title":"t","body":"published value"}`)
		publishedAt := baseTime.Add(time.Minute)
		if err := repo.SetEntryPublishState(ctx, &domain.Entry{
			ID: id, TenantID: tenant, ContentTypeID: ct.ID, Version: 1,
		}, domain.StatusPublished, &publishedAt); err != nil {
			t.Fatalf("publish: %v", err)
		}
		cur, err := repo.GetEntry(ctx, tenant, ct.ID, id)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		human := domain.ActorKindHuman
		cur.Payload = json.RawMessage(`{"title":"t"}`)
		cur.UpdatedAt = baseTime.Add(2 * time.Minute)
		cur.UpdatedBy, cur.UpdatedByKind = &priorEditor, &human
		if err := repo.UpdateEntry(ctx, cur); err != nil {
			t.Fatalf("drop the key from the working copy: %v", err)
		}
		before := revisions(t, id)
		versionBefore := before[0].Version

		body, _ := ct.FieldByKey("body")
		if err := repo.DeleteField(ctx, tenant, ct, body, schemaAdmin, baseTime.Add(time.Hour)); err != nil {
			t.Fatalf("delete field over a published-only key: %v", err)
		}

		after := revisions(t, id)
		if len(after) != len(before) {
			t.Fatalf("want no new revision for a row whose working copy did not change, went from %d to %d",
				len(before), len(after))
		}
		if after[0].Version != versionBefore {
			t.Fatalf("newest revision moved to version %d from %d", after[0].Version, versionBefore)
		}
		// The same guard covers the provenance columns, and it needs its own
		// assertion: updated_by answers "who last wrote the working copy", and
		// this row's working copy was not written. Stamping the admin here would
		// credit them with a write that did not happen.
		reloaded, err := repo.GetEntry(ctx, tenant, ct.ID, id)
		if err != nil {
			t.Fatalf("reload after the bulk delete: %v", err)
		}
		if reloaded.UpdatedBy == nil || *reloaded.UpdatedBy != priorEditor {
			t.Fatalf("updated_by is %v, want the editor who actually last wrote the working copy (%s)",
				reloaded.UpdatedBy, priorEditor)
		}

		// AND THE SAME FOR RENAME. Repeated rather than left to the reader
		// because the guard is a separate copy in a separate statement: with
		// only the delete case covered, dropping rename's JOIN passes every
		// test. Same setup, different survivor key so the two cannot interfere.
		ct2 := mkType(t, ctx, repo, tenant, "post_pub_ren", [2]string{"title", "string"}, [2]string{"body", "string"})
		id2 := mkEntry(t, ct2, `{"title":"t","body":"published value"}`)
		pubAt2 := baseTime.Add(time.Minute)
		if err := repo.SetEntryPublishState(ctx, &domain.Entry{
			ID: id2, TenantID: tenant, ContentTypeID: ct2.ID, Version: 1,
		}, domain.StatusPublished, &pubAt2); err != nil {
			t.Fatalf("publish for rename: %v", err)
		}
		cur2, err := repo.GetEntry(ctx, tenant, ct2.ID, id2)
		if err != nil {
			t.Fatalf("reload for rename: %v", err)
		}
		cur2.Payload = json.RawMessage(`{"title":"t"}`)
		cur2.UpdatedAt = baseTime.Add(2 * time.Minute)
		cur2.UpdatedBy, cur2.UpdatedByKind = &priorEditor, &human
		if err := repo.UpdateEntry(ctx, cur2); err != nil {
			t.Fatalf("drop the key from the working copy for rename: %v", err)
		}
		beforeRen := revisions(t, id2)
		if err := repo.RenameField(ctx, tenant, ct2, "body", "content", schemaAdmin, baseTime.Add(time.Hour)); err != nil {
			t.Fatalf("rename over a published-only key: %v", err)
		}
		if afterRen := revisions(t, id2); len(afterRen) != len(beforeRen) {
			t.Fatalf("rename wrote a revision for a row whose working copy did not change: %d -> %d",
				len(beforeRen), len(afterRen))
		}
	})
}
