package repository

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-006 Amendment 4, proven against the real thing. The service test can only
// say "the request carried MatchPublished"; whether the SQL then reads
// published_payload — for containment, for range comparisons, for ILIKE and IN
// — is a property of strings built in this package, and a fake would be a
// second implementation to keep in step. So every operator family is exercised
// here with a row whose working copy and snapshot DISAGREE, and the assertion
// is always the same: the snapshot answers, the working copy is silent.
func TestSnapshotFilter(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("snapshot"),
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
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The index 000039 exists to add. Without it every snapshot containment
	// filter is a sequential scan over the tenant's rows, and nothing else in
	// this test would notice — the answers would still be right.
	t.Run("published_payload has its own jsonb_path_ops GIN index", func(t *testing.T) {
		var def string
		if err := pool.QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE tablename = 'entries' AND indexname = 'idx_entries_published_payload_gin'`,
		).Scan(&def); err != nil {
			t.Fatalf("index missing: %v", err)
		}
		if !strings.Contains(def, "gin") || !strings.Contains(def, "published_payload jsonb_path_ops") {
			t.Fatalf("unexpected index definition: %s", def)
		}
	})

	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatal(err)
	}
	repo := NewPostgresContentRepository(pool, nil)
	title := domain.Field{ID: uuid.New(), ContentTypeID: typeID, Key: "title", Type: domain.FieldTypeString, EnumValues: []string{}, CreatedAt: time.Now().UTC()}
	price := domain.Field{ID: uuid.New(), ContentTypeID: typeID, Key: "price", Type: domain.FieldTypeNumber, EnumValues: []string{}, CreatedAt: time.Now().UTC()}
	tags := domain.Field{ID: uuid.New(), ContentTypeID: typeID, Key: "tags", Type: domain.FieldTypeString, Multiple: true, EnumValues: []string{}, CreatedAt: time.Now().UTC()}
	for _, f := range []*domain.Field{&title, &price, &tags} {
		if err := repo.AddField(ctx, "t", f); err != nil {
			t.Fatal(err)
		}
	}

	seed := func(status, payload, published string) uuid.UUID {
		id := uuid.New()
		var pub, ver, at any
		if published != "" {
			pub, ver = published, 1
		}
		// published_at tracks STATUS (000016), the snapshot tracks its own
		// columns (000020/000033): a retired row keeps published_payload but
		// has no published_at, which is exactly the shape 000033 allows.
		if status == domain.StatusPublished {
			at = time.Now().UTC()
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, locale, translation_group_id,
			                     published_payload, published_version, published_at)
			VALUES ($1,'t',$2,$3::jsonb,$4,'default',$5,$6::jsonb,$7,$8)`,
			id, typeID, payload, status, uuid.New(), pub, ver, at); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// The row this whole amendment is about: published once, then edited. The
	// working copy holds text that has never been released.
	edited := seed(domain.StatusPublished,
		`{"title":"secret-edit","price":5,"tags":["draft-only"]}`,
		`{"title":"public","price":10,"tags":["released"]}`)
	// Never published: its working copy would match "public" and must not.
	draft := seed(domain.StatusDraft, `{"title":"public","price":10,"tags":["released"]}`, "")
	// Unpublished after a release: 000033 keeps the snapshot, but the row is a
	// draft and the snapshot is nobody's to serve.
	retired := seed(domain.StatusDraft, `{"title":"gone","price":1}`, `{"title":"public","price":10,"tags":["released"]}`)

	ids := func(t *testing.T, f ListEntriesFilter) map[uuid.UUID]bool {
		t.Helper()
		f.TenantID, f.ContentTypeID, f.Limit = "t", typeID, 50
		items, _, err := repo.ListEntries(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		got := map[uuid.UUID]bool{}
		for _, it := range items {
			got[it.ID] = true
		}
		return got
	}
	snapshot := func(filters ...FieldFilter) ListEntriesFilter {
		return ListEntriesFilter{Status: domain.StatusPublished, MatchPublished: true, CursorPaged: true, Filters: filters}
	}
	only := func(t *testing.T, got map[uuid.UUID]bool, want ...uuid.UUID) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d", len(got), len(want))
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("row %s missing", id)
			}
		}
	}

	t.Run("eq matches the snapshot, not the working copy", func(t *testing.T) {
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpEq, Value: "public"})), edited)
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpEq, Value: "secret-edit"})))
	})
	t.Run("neq is evaluated on the snapshot too", func(t *testing.T) {
		// The snapshot title IS "public", so neq excludes the row even though
		// the working copy says otherwise. draft/retired are gone by status.
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpNeq, Value: "public"})))
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpNeq, Value: "secret-edit"})), edited)
	})
	t.Run("range comparisons cast the snapshot's value", func(t *testing.T) {
		only(t, ids(t, snapshot(FieldFilter{Field: price, Op: OpGte, Value: "10"})), edited)
		// Working-copy price is 5; a lt:10 that read payload would return the row.
		only(t, ids(t, snapshot(FieldFilter{Field: price, Op: OpLt, Value: "10"})))
	})
	t.Run("in / contains read the snapshot's text", func(t *testing.T) {
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpIn, Value: "public,other"})), edited)
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpIn, Value: "secret-edit,other"})))
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpContains, Value: "PUBL"})), edited)
		only(t, ids(t, snapshot(FieldFilter{Field: title, Op: OpContains, Value: "secret"})))
	})
	t.Run("has / nhas read the snapshot's array", func(t *testing.T) {
		only(t, ids(t, snapshot(FieldFilter{Field: tags, Op: OpHas, Value: "released"})), edited)
		only(t, ids(t, snapshot(FieldFilter{Field: tags, Op: OpHas, Value: "draft-only"})))
		only(t, ids(t, snapshot(FieldFilter{Field: tags, Op: OpNhas, Value: "draft-only"})), edited)
	})

	// The admin audience is the control: same predicates, working copy, and
	// the retired row's retained snapshot is invisible to it as well — it is
	// matched on payload like everything else.
	t.Run("admin lists still match the working copy", func(t *testing.T) {
		only(t, ids(t, ListEntriesFilter{Filters: []FieldFilter{{Field: title, Op: OpEq, Value: "secret-edit"}}}), edited)
		only(t, ids(t, ListEntriesFilter{Filters: []FieldFilter{{Field: title, Op: OpEq, Value: "public"}}}), draft)
		only(t, ids(t, ListEntriesFilter{Filters: []FieldFilter{{Field: title, Op: OpEq, Value: "gone"}}}), retired)
	})

	// The guard the flag is only safe behind. A service that set MatchPublished
	// and forgot the status override must not get an answer — it would be
	// evaluating predicates over the retired row's retained snapshot.
	t.Run("MatchPublished without Status = published is refused", func(t *testing.T) {
		_, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50, MatchPublished: true,
			Filters: []FieldFilter{{Field: title, Op: OpEq, Value: "public"}},
		})
		if err == nil {
			t.Fatal("unscoped snapshot match was answered")
		}
	})
}
