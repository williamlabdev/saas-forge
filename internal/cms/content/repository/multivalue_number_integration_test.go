package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Why a NUMBER field carries its own containment test, when tags already has one:
// number is the newest member of domain.AllowedMultipleTypes, and it got there by
// having this file executed rather than argued. The list used to refuse it on the
// grounds that the comparison operators emit `(payload ->> key)::numeric` and
// raise SQLSTATE 22P02 against an array — a reason carried in a comment and never
// run. Two things follow from that reason, and only one had ever been checked.
//
//  1. The cast really does raise 22P02, so the service's cardinality gate
//     (parseFilter's OpAllowedFor, parseSort's Multiple refusal) is LOAD-BEARING
//     rather than belt-and-braces: deleting either layer turns a 400 into a 500.
//     Allowing the flag did not weaken that gate, it relied on it — which is why
//     these assertions matter MORE now than they did while number was refused.
//  2. `has` on a numeric element already worked. containmentDoc runs the operand
//     through typedValue, so it builds {"scores":[7]} and not {"scores":["7"]},
//     and jsonb compares numbers as numeric. The operator layer already knew
//     everything it needed to about a multi-valued number; nothing here changed
//     to let the flag flip.
//
// Both are asserted against real Postgres because neither can be asserted anywhere
// else: memRepo does not implement Filters, and a Go re-implementation of `@>` and
// of ::numeric would be the vacuous pass this whole file exists to avoid.
//
// The comparison filters below are built by hand rather than through the service
// ON PURPOSE. The service refuses to construct them — that refusal is the thing
// under test — so the only way to see what it protects against is to go around it.
func TestMultiValueNumber_CastAndContainment(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("multivaluenum"),
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

	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','quiz','')`, typeID); err != nil {
		t.Fatal(err)
	}
	repo := NewPostgresContentRepository(pool, nil)

	// enum_values is TEXT[] NOT NULL and pgx sends a nil slice as SQL NULL; the
	// service's buildField normalises this, and a direct repository write has to.
	scores := domain.Field{
		ID: uuid.New(), ContentTypeID: typeID, Key: "scores",
		Type: domain.FieldTypeNumber, Multiple: true, CreatedAt: time.Now().UTC(),
		EnumValues: []string{},
	}
	if err := repo.AddField(ctx, "t", &scores); err != nil {
		t.Fatal(err)
	}

	seed := func(payload string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, locale, translation_group_id)
			VALUES ($1,'t',$2,$3::jsonb,'draft','default',$4)`, id, typeID, payload, uuid.New()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pair := seed(`{"scores":[3,7]}`)
	ten := seed(`{"scores":[10]}`)
	whole := seed(`{"scores":[4.0]}`)
	empty := seed(`{"scores":[]}`)

	listErr := func(t *testing.T, f ListEntriesFilter) error {
		t.Helper()
		f.TenantID, f.ContentTypeID, f.Limit = "t", typeID, 50
		_, _, err := repo.ListEntries(ctx, f)
		return err
	}
	assert22P02 := func(t *testing.T, err error, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s did not fail — if Postgres has stopped raising a cast error here, "+
				"the hazard the cardinality gate exists for is gone, and what needs "+
				"rewriting is the gate's justification, not this test", what)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s failed with a non-Postgres error, so this test is not measuring the cast: %v", what, err)
		}
		if pgErr.Code != "22P02" {
			t.Fatalf("%s raised SQLSTATE %s, not the 22P02 the refusal is justified by: %v", what, pgErr.Code, err)
		}
	}

	// (1) The refusal's premise, executed. Both of the service's gates are checked
	// separately because they are two different code paths guarding one hazard,
	// and a defence that exists twice needs each layer proven on its own.
	t.Run("a scalar comparison on a multi-valued number raises 22P02", func(t *testing.T) {
		err := listErr(t, ListEntriesFilter{
			Filters: []FieldFilter{{Field: scores, Op: OpGt, Value: "5"}},
		})
		assert22P02(t, err, "scores:gt:5")
	})

	t.Run("ordering by a multi-valued number raises 22P02", func(t *testing.T) {
		err := listErr(t, ListEntriesFilter{Sort: &SortSpec{Field: scores}})
		assert22P02(t, err, "sort=scores:asc")
	})

	// (2) The other half: what a multi-valued number CAN answer today, with no
	// change to the operator layer at all.
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
	has := func(v string) FieldFilter { return FieldFilter{Field: scores, Op: OpHas, Value: v} }

	t.Run("has matches a numeric element by value, not by spelling", func(t *testing.T) {
		got := list(t, has("7"))
		if !got[pair] {
			t.Fatal(`has:7 missed {"scores":[3,7]} — containmentDoc built a string element, so the operand never met the stored number`)
		}
		if got[ten] || got[empty] {
			t.Fatal("has:7 matched an entry that does not hold 7")
		}
	})

	t.Run("has is not a text prefix match", func(t *testing.T) {
		// The failure this rules out is the numeric analogue of has:ai matching
		// ["ai-native"]: if the operand were compared as text, "1" would be a
		// prefix of "10" under any ILIKE-shaped implementation.
		if list(t, has("1"))[ten] {
			t.Fatal(`has:1 matched {"scores":[10]} — the operand is being compared as text`)
		}
	})

	// typedValue is what makes containment numeric, so it is also the only thing
	// standing between a typo and the operand reaching jsonb. It has to fail as a
	// caller error before any SQL is built: `scores:has:abc` is a mistake someone
	// makes by hand in a URL, and it must read as one.
	t.Run("a non-numeric operand is a 400, not a query", func(t *testing.T) {
		err := listErr(t, ListEntriesFilter{
			Filters: []FieldFilter{{Field: scores, Op: OpHas, Value: "abc"}},
		})
		if err == nil {
			t.Fatal("has:abc was accepted — the operand never went through typedValue")
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("has:abc reached Postgres and failed there (SQLSTATE %s) — that is a 500 on a list page", pgErr.Code)
		}
		var appErr *apperrors.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("has:abc failed with an unclassified error, so the caller cannot be told what they got wrong: %v", err)
		}
		if appErr.HTTPStatus != 400 || appErr.Code != "CONTENT_FILTER_VALUE_INVALID" {
			t.Fatalf("has:abc gave %d/%s, want 400/CONTENT_FILTER_VALUE_INVALID", appErr.HTTPStatus, appErr.Code)
		}
	})

	t.Run("numeric spellings of the same value are the same element", func(t *testing.T) {
		// jsonb stores numbers as `numeric`, so 4.0 and 4 are one value. This is
		// what makes `has` safe on numbers without a canonicalisation step at
		// write time; if it were false, an entry would be findable only by the
		// exact spelling its author happened to type.
		for _, spelling := range []string{"4", "4.0", "4.00"} {
			if !list(t, has(spelling))[whole] {
				t.Fatalf(`has:%s missed {"scores":[4.0]} — numeric containment is spelling-sensitive after all`, spelling)
			}
		}
	})
}
