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

// Containment semantics against the real thing. None of this can be asserted
// against a Go fake: memRepo does not implement Filters at all, and a hand-
// written containment would be a SECOND implementation to keep in step — the
// vacuous-pass problem, dressed up to look meaningful. The whole design rests on
// two Postgres facts, and they are pinned here rather than assumed.
func TestMultiValueContainment(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("multivalue"),
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

	// The premise the whole operator design rests on. If this ever becomes true,
	// `has` could have been spelled `eq` and this test says so out loud.
	t.Run("a scalar containment document does NOT match an array", func(t *testing.T) {
		var scalarMatches, arrayMatches bool
		if err := pool.QueryRow(ctx, `
			SELECT '{"tags":["ai","ml"]}'::jsonb @> '{"tags":"ai"}'::jsonb,
			       '{"tags":["ai","ml"]}'::jsonb @> '{"tags":["ai"]}'::jsonb`,
		).Scan(&scalarMatches, &arrayMatches); err != nil {
			t.Fatal(err)
		}
		if scalarMatches {
			t.Fatal("Postgres matched a scalar against a nested array — the has/eq split is built on this being false")
		}
		if !arrayMatches {
			t.Fatal("Postgres did not match a one-element array against a nested array — containmentDoc's wrapping would be wrong")
		}
	})

	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatal(err)
	}
	repo := NewPostgresContentRepository(pool, nil)

	tags := domain.Field{
		ID: uuid.New(), ContentTypeID: typeID, Key: "tags",
		Type: domain.FieldTypeString, Multiple: true, CreatedAt: time.Now().UTC(),
		// enum_values is TEXT[] NOT NULL and pgx sends a nil slice as SQL NULL.
		// buildField normalises this for callers coming through the service; this
		// test writes to the repository directly, so it has to do it itself.
		EnumValues: []string{},
	}
	if err := repo.AddField(ctx, "t", &tags); err != nil {
		t.Fatal(err)
	}

	// Write-then-read, because the two column lists are separate statements and a
	// column written but not selected comes back false — every multi field would
	// quietly behave as a scalar on the next request.
	t.Run("the multiple flag survives insert and load", func(t *testing.T) {
		ct, err := repo.GetContentTypeByName(ctx, "t", "post")
		if err != nil {
			t.Fatal(err)
		}
		f, ok := ct.FieldByKey("tags")
		if !ok {
			t.Fatal("field vanished")
		}
		if !f.Multiple {
			t.Fatal("multiple came back false — insertField and loadFields have drifted")
		}
	})

	seed := func(payload string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, locale, translation_group_id)
			VALUES ($1,'t',$2,$3::jsonb,'draft','default',$4)`, id, typeID, payload, uuid.New()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	both := seed(`{"tags":["ai","ml"]}`)
	reversed := seed(`{"tags":["ml","ai"]}`)
	prefixOnly := seed(`{"tags":["ai-native"]}`)
	empty := seed(`{"tags":[]}`)
	noKey := seed(`{}`)

	list := func(t *testing.T, filters ...FieldFilter) map[uuid.UUID]bool {
		t.Helper()
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50, Filters: filters,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := map[uuid.UUID]bool{}
		for _, it := range items {
			got[it.ID] = true
		}
		return got
	}
	has := func(v string) FieldFilter { return FieldFilter{Field: tags, Op: OpHas, Value: v} }
	nhas := func(v string) FieldFilter { return FieldFilter{Field: tags, Op: OpNhas, Value: v} }

	t.Run("has matches any element, not just the first", func(t *testing.T) {
		byFirst := list(t, has("ai"))
		bySecond := list(t, has("ml"))
		for _, id := range []uuid.UUID{both, reversed} {
			if !byFirst[id] || !bySecond[id] {
				t.Fatalf("entry %s must match both has:ai and has:ml regardless of element order", id)
			}
		}
	})

	t.Run("has is not a substring match", func(t *testing.T) {
		if list(t, has("ai"))[prefixOnly] {
			t.Fatal(`has:ai matched ["ai-native"] — that is the ILIKE behaviour this operator exists to avoid`)
		}
	})

	t.Run("repeated filters compose as all-of", func(t *testing.T) {
		got := list(t, has("ai"), has("ml"))
		if !got[both] || !got[reversed] {
			t.Fatal("all-of should match an entry holding both")
		}
		if got[prefixOnly] || got[empty] || got[noKey] {
			t.Fatal("all-of matched an entry missing one of the values")
		}
	})

	t.Run("nhas covers other values, an empty array, and a missing key", func(t *testing.T) {
		got := list(t, nhas("ai"))
		for name, id := range map[string]uuid.UUID{"empty array": empty, "no key at all": noKey} {
			if !got[id] {
				t.Fatalf("nhas:ai must match an entry with %s — a row that has no tags does not have this tag", name)
			}
		}
		if got[both] {
			t.Fatal("nhas:ai matched an entry that has ai")
		}
	})

	// Pins the index claim rather than asserting it in a comment. jsonb_path_ops
	// exists for exactly this query shape; if the plan stops using it, the
	// operator is quietly doing a sequential scan on the public read path.
	t.Run("has uses the payload GIN index", func(t *testing.T) {
		// Enough rows in this one type that containment is the selective
		// predicate; with a handful the tenant+type btree is obviously better and
		// the plan says nothing about whether GIN is reachable at all.
		for range 2000 {
			seed(`{"tags":["filler"]}`)
		}
		if _, err := pool.Exec(ctx, `ANALYZE entries`); err != nil {
			t.Fatal(err)
		}
		rows, err := pool.Query(ctx, `
			EXPLAIN (COSTS OFF) SELECT id FROM entries
			WHERE tenant_id = 't' AND content_type_id = $1 AND payload @> '{"tags":["ai"]}'::jsonb`, typeID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "idx_entries_payload_gin") {
			t.Fatalf("containment did not reach the GIN index:\n%s", plan.String())
		}
	})
}
