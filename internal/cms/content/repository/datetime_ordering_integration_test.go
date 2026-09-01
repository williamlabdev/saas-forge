package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// A datetime field is ordered by INSTANT, not by how the instant was spelled.
//
// This cannot be asserted against memRepo: the ordering under test is a SQL
// expression, and a Go fake would sort with Go's own comparison — the second
// implementation that passes for the emptiest of reasons. It is the same class
// of blind spot that let RenameField ship broken (SQLSTATE 42P18 on every call)
// against a green fake.
//
// The fixtures are chosen so text order and chronological order are INVERTED.
// Nothing here would catch a regression otherwise: pick two values with the
// same offset and a plain text comparison sorts them correctly, and the test
// passes whether or not the cast exists.
func TestDateTimeOrderingIsByInstantNotByText(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("dtorder"),
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
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','booking','')`, typeID); err != nil {
		t.Fatal(err)
	}
	repo := NewPostgresContentRepository(pool, nil)

	startsAt := domain.Field{
		ID: uuid.New(), ContentTypeID: typeID, Key: "starts_at",
		Type: domain.FieldTypeDateTime, CreatedAt: time.Now().UTC(),
		// enum_values is TEXT[] NOT NULL and pgx sends a nil slice as SQL NULL;
		// this test writes through the repository, not the service, so it
		// normalises for itself (same note as the multi-value suite).
		EnumValues: []string{},
	}
	if err := repo.AddField(ctx, "t", &startsAt); err != nil {
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

	// 01:00Z, written in Taipei local time — the spelling a booking UI produces.
	early := seed(`{"starts_at":"2026-08-01T09:00:00+08:00"}`)
	// 05:00Z, four hours LATER, but its text begins "...T05" so a text
	// comparison ranks it BEFORE the 09:00+08:00 spelling above.
	late := seed(`{"starts_at":"2026-08-01T05:00:00Z"}`)

	t.Run("ascending sort follows the instant", func(t *testing.T) {
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50,
			Sort: &SortSpec{Field: startsAt},
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("want 2 rows, got %d", len(items))
		}
		if items[0].ID != early || items[1].ID != late {
			t.Fatalf("sorted by text, not by instant: 09:00+08:00 (01:00Z) must precede 05:00Z")
		}
	})

	t.Run("a range bound selects by the instant it names", func(t *testing.T) {
		// 03:00Z sits BETWEEN the two instants, so exactly one row qualifies.
		// Under text comparison both do: "...T09:00:00+08:00" > "...T03:00:00Z"
		// because '9' > '3', and that wrong row is returned with no error at all.
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50,
			Filters: []FieldFilter{{Field: startsAt, Op: OpGte, Value: "2026-08-01T03:00:00Z"}},
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) != 1 || items[0].ID != late {
			t.Fatalf("range filter compared text: want only the 05:00Z row, got %d row(s)", len(items))
		}
	})

	t.Run("offset spelling does not change which instants qualify", func(t *testing.T) {
		// The SAME bound as above, spelled in +08:00. A correct implementation
		// cannot tell these two queries apart; a text comparison gives them
		// different answers, which is the defect stated as plainly as it gets.
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50,
			Filters: []FieldFilter{{Field: startsAt, Op: OpGte, Value: "2026-08-01T11:00:00+08:00"}},
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) != 1 || items[0].ID != late {
			t.Fatalf("the bound's spelling changed the result set: got %d row(s)", len(items))
		}
	})

	t.Run("a malformed bound is a 400, not a cast error", func(t *testing.T) {
		// The cast is what makes the comparison correct AND what turns a typo
		// into SQLSTATE 22P02 — a 500 on a list page. The guard runs before the
		// value is ever bound.
		_, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50,
			Filters: []FieldFilter{{Field: startsAt, Op: OpGte, Value: "not-a-timestamp"}},
		})
		if err == nil {
			t.Fatal("a malformed datetime bound was accepted")
		}
		var appErr *apperrors.AppError
		if !errors.As(err, &appErr) || appErr.Code != "CONTENT_FILTER_VALUE_INVALID" {
			t.Fatalf("want CONTENT_FILTER_VALUE_INVALID, got %v — a cast error reaching the caller is a 500", err)
		}
	})
}
