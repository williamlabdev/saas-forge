package repository

import (
	"context"
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

// GetEntriesByIDs against the real thing — ADR-006 Amendment 6.
//
// Three of its four properties are unfalsifiable in a fake, because a fake would
// be a second implementation of the same predicate and would share any bug in
// it:
//
//   - "live" is status = published AND published_payload IS NOT NULL. Migration
//     000033 made an unpublish RETAIN the snapshot, so a retracted entry is a row
//     that satisfies HALF the predicate. A by-ids fetch that checked only the
//     snapshot would serve it, and nothing in the response would say so.
//   - Tenant isolation has two independent layers here — the WHERE clause and
//     the RLS policy — and this test runs as a NON-superuser so the second one
//     actually applies. Populate takes ids from a payload, which is tenant data;
//     an id another tenant put there is the exact attack the policy exists for.
//     Both spellings of matchPublished are checked against it, because the admin
//     expansion (Amendment 7) is the one that runs with the state filter OFF and
//     must therefore lean on tenancy alone.
//   - ONE statement for the whole page is the feature's entire economic case,
//     and `id = ANY($2)` is what makes it hold at 500 ids. An IN-list built by
//     string concatenation passes every small test and hits the parameter limit
//     in production.
func TestGetEntriesByIDs(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("byids"),
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

	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	super, err := pgxpool.New(ctx, superDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer super.Close()
	if _, err := super.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mineType, theirsType := uuid.New(), uuid.New()
	for _, tc := range []struct {
		id     uuid.UUID
		tenant string
		name   string
	}{{mineType, "t", "author"}, {theirsType, "other", "author"}} {
		if _, err := super.Exec(ctx,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,$2,$3,'')`,
			tc.id, tc.tenant, tc.name); err != nil {
			t.Fatalf("seed type: %v", err)
		}
	}

	// seed inserts one entry in whatever half-state the case needs. The
	// snapshot and the working copy differ on every row, so a read that
	// returned the wrong column would be visible rather than merely equal.
	// entries_published_at_check (migration 000016) ties published_at to status:
	// exactly the published rows may carry one.
	seed := func(tenant string, typeID uuid.UUID, status string, snapshot any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var publishedAt any
		if status == domain.StatusPublished {
			publishedAt = time.Now().UTC()
		}
		if _, err := super.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, locale, translation_group_id,
			                     published_payload, published_version, published_at, created_at, updated_at)
			VALUES ($1,$2,$3,'{"name":"working copy"}'::jsonb,$4,'default',$5,$6::jsonb,1,$7,now(),now())`,
			id, tenant, typeID, status, uuid.New(), snapshot, publishedAt); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
		return id
	}
	live1 := seed("t", mineType, "published", `{"name":"live one"}`)
	live2 := seed("t", mineType, "published", `{"name":"live two"}`)
	neverPublished := seed("t", mineType, "draft", nil)
	// The 000033 case: unpublished, snapshot RETAINED.
	retracted := seed("t", mineType, "draft", `{"name":"retracted"}`)
	foreign := seed("other", theirsType, "published", `{"name":"another tenant"}`)

	// A dedicated non-superuser role — the only way RLS actually applies.
	if _, err := super.Exec(ctx, `
		CREATE ROLE byidsapp LOGIN PASSWORD 'byidspw' NOSUPERUSER;
		GRANT USAGE ON SCHEMA public TO byidsapp;
		GRANT SELECT, INSERT, UPDATE, DELETE ON content_types, entries, content_type_fields TO byidsapp;
	`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	app, err := pgxpool.New(ctx, "postgres://byidsapp:byidspw@"+host+":"+port.Port()+"/byids?sslmode=disable")
	if err != nil {
		t.Fatalf("connect as byidsapp: %v", err)
	}
	defer app.Close()
	repo := NewPostgresContentRepository(app, nil)

	idsOf := func(rows []*domain.Entry) map[uuid.UUID]*domain.Entry {
		out := make(map[uuid.UUID]*domain.Entry, len(rows))
		for _, e := range rows {
			out[e.ID] = e
		}
		return out
	}
	everything := []uuid.UUID{live1, live2, neverPublished, retracted, foreign, uuid.New()}

	t.Run("matchPublished serves only the live ones", func(t *testing.T) {
		rows, err := repo.GetEntriesByIDs(ctx, "t", everything, true)
		if err != nil {
			t.Fatal(err)
		}
		got := idsOf(rows)
		if len(got) != 2 {
			t.Fatalf("want exactly the two live rows, got %d", len(got))
		}
		for _, want := range []uuid.UUID{live1, live2} {
			if _, ok := got[want]; !ok {
				t.Fatalf("live row %s missing", want)
			}
		}
		if _, ok := got[retracted]; ok {
			t.Fatal("a retracted entry still holds published_payload — the status half of the predicate is what excludes it")
		}
		if _, ok := got[neverPublished]; ok {
			t.Fatal("a draft has no snapshot to serve")
		}
	})

	t.Run("another tenant's id is invisible under both layers", func(t *testing.T) {
		for _, matchPublished := range []bool{true, false} {
			rows, err := repo.GetEntriesByIDs(ctx, "t", []uuid.UUID{foreign}, matchPublished)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("matchPublished=%v: another tenant's published entry was returned", matchPublished)
			}
		}
		// And the row is real — the emptiness above is scoping, not a bad seed.
		rows, err := repo.GetEntriesByIDs(ctx, "other", []uuid.UUID{foreign}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("the foreign row must exist for its own tenant, got %d", len(rows))
		}
	})

	// The admin expansion — ADR-006 Amendment 7. It is the SAME method with the
	// flag off rather than a second query, which is what keeps the two audiences
	// from drifting in what "another tenant's id" means.
	t.Run("without matchPublished every state comes back", func(t *testing.T) {
		rows, err := repo.GetEntriesByIDs(ctx, "t", everything, false)
		if err != nil {
			t.Fatal(err)
		}
		got := idsOf(rows)
		if len(got) != 4 {
			t.Fatalf("want the four rows of tenant t, got %d", len(got))
		}
		for _, want := range []uuid.UUID{live1, live2, neverPublished, retracted} {
			if _, ok := got[want]; !ok {
				t.Fatalf("row %s missing", want)
			}
		}
	})

	// The row a never-published target expands into. The admin projection serves
	// `payload` as `data`, so a fetch that could not carry the working copy of a
	// row with no snapshot would render the target as an empty document — the
	// failure mode this widening exists to avoid, and one no assertion about the
	// row COUNT above can see.
	t.Run("a never-published row carries its working copy", func(t *testing.T) {
		rows, err := repo.GetEntriesByIDs(ctx, "t", []uuid.UUID{neverPublished}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows", len(rows))
		}
		e := rows[0]
		if string(e.Payload) != `{"name": "working copy"}` && string(e.Payload) != `{"name":"working copy"}` {
			t.Fatalf("payload not scanned as the working copy: %s", e.Payload)
		}
		if len(e.PublishedPayload) != 0 {
			t.Fatalf("a never-published row must have no snapshot, got %s", e.PublishedPayload)
		}
		if e.Status != domain.StatusDraft {
			t.Fatalf("status not scanned: %q", e.Status)
		}
	})

	t.Run("the scanned row carries what the caller decides with", func(t *testing.T) {
		rows, err := repo.GetEntriesByIDs(ctx, "t", []uuid.UUID{live1}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows", len(rows))
		}
		e := rows[0]
		// ContentTypeID: populateFor drops a row whose type is not the one the
		// relation field declares, and cannot do that if the column is not read.
		if e.ContentTypeID != mineType {
			t.Fatalf("content_type_id not scanned: %v", e.ContentTypeID)
		}
		// PublishedPayload: the delivery projection serves it AS `data`. Reading
		// the working copy here would ship an unreleased edit.
		if string(e.PublishedPayload) != `{"name": "live one"}` && string(e.PublishedPayload) != `{"name":"live one"}` {
			t.Fatalf("published_payload not scanned as the snapshot: %s", e.PublishedPayload)
		}
		if e.Status != domain.StatusPublished {
			t.Fatalf("status not scanned: %q", e.Status)
		}
	})

	t.Run("no ids is not a query", func(t *testing.T) {
		rows, err := repo.GetEntriesByIDs(ctx, "t", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("got %d rows for an empty id list", len(rows))
		}
	})

	t.Run("ids that name nothing are empty, not an error", func(t *testing.T) {
		rows, err := repo.GetEntriesByIDs(ctx, "t", []uuid.UUID{uuid.New(), uuid.New()}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("got %d rows", len(rows))
		}
	})

	// The cap the service enforces, fetched in ONE statement. 500 ids is well
	// past what any IN-list spelling survives comfortably, and the point of
	// ANY($2) is that the id count never becomes a parameter count.
	t.Run("the whole id budget in one statement", func(t *testing.T) {
		rows, err := super.Query(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, locale, translation_group_id,
			                     published_payload, published_version, published_at, created_at, updated_at)
			SELECT gen_random_uuid(), 't', $1, '{}'::jsonb, 'published', 'default', gen_random_uuid(),
			       '{"name":"bulk"}'::jsonb, 1, now(), now(), now()
			FROM generate_series(1, 500)
			RETURNING id`, mineType)
		if err != nil {
			t.Fatalf("bulk seed: %v", err)
		}
		var bulk []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			bulk = append(bulk, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(bulk) != 500 {
			t.Fatalf("seeded %d rows", len(bulk))
		}

		got, err := repo.GetEntriesByIDs(ctx, "t", bulk, true)
		if err != nil {
			t.Fatalf("500 ids: %v", err)
		}
		if len(got) != 500 {
			t.Fatalf("want all 500 back, got %d", len(got))
		}
	})
}
