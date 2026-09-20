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

func permPool(t *testing.T, dbName string) (*pgxpool.Pool, context.Context) {
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
	return pool, ctx
}

// Field DEFINITION ORDER is load-bearing — the admin form renders it and the
// artifact preserves it rather than sorting — and until migration 000025 it was
// an accident: every field of a type shares one created_at, so ORDER BY
// created_at was a total tie resolved by physical layout.
//
// This test forces the physical order to differ from the definition order by
// rewriting the first row (an UPDATE moves the tuple to the end of the heap).
// Against the old ORDER BY it reads back with that field last; against ordinal
// it does not move. It is the mutation proof for 000025, not a restatement of
// the round-trip test.
func TestFieldOrder_SurvivesPhysicalReordering(t *testing.T) {
	pool, ctx := permPool(t, "fieldorder")
	repo := NewPostgresContentRepository(pool, nil)

	id := uuid.New()
	now := time.Now().UTC()
	want := []string{"title", "body", "state", "author"}
	fields := make([]domain.Field, 0, len(want))
	for _, k := range want {
		fields = append(fields, domain.Field{
			ID: uuid.New(), ContentTypeID: id, Key: k, Type: domain.FieldTypeString,
			EnumValues: []string{},
			// Every field carries the SAME timestamp, exactly as
			// CreateContentType produces. That is the tie.
			CreatedAt: now,
		})
	}
	if err := repo.CreateContentType(ctx, &domain.ContentType{
		ID: id, TenantID: "t1", Name: "article", CreatedAt: now, UpdatedAt: now, Fields: fields,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Rewrite the first-defined row. Postgres writes a new tuple version, which
	// lands after the others in the heap.
	if _, err := pool.Exec(ctx,
		`UPDATE content_type_fields SET label = 'moved' WHERE content_type_id = $1 AND key = 'title'`, id); err != nil {
		t.Fatalf("force a physical move: %v", err)
	}

	got, err := repo.GetContentTypeByName(ctx, "t1", "article")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.Fields) != len(want) {
		t.Fatalf("want %d fields, got %d", len(want), len(got.Fields))
	}
	for i, k := range want {
		if got.Fields[i].Key != k {
			t.Fatalf("field %d is %q, want %q — definition order is following physical layout, not the ordinal", i, got.Fields[i].Key, k)
		}
	}
}

// AddField appends. A field added later must land AFTER the ones defined with
// the type, which is the property the admin form depends on — a new field
// appearing in the middle of a form is a UI change nobody made.
func TestFieldOrder_AddFieldAppends(t *testing.T) {
	pool, ctx := permPool(t, "fieldorderadd")
	repo := NewPostgresContentRepository(pool, nil)

	id := uuid.New()
	now := time.Now().UTC()
	if err := repo.CreateContentType(ctx, &domain.ContentType{
		ID: id, TenantID: "t1", Name: "article", CreatedAt: now, UpdatedAt: now,
		Fields: []domain.Field{
			{ID: uuid.New(), ContentTypeID: id, Key: "title", Type: domain.FieldTypeString, EnumValues: []string{}, CreatedAt: now},
			{ID: uuid.New(), ContentTypeID: id, Key: "body", Type: domain.FieldTypeString, EnumValues: []string{}, CreatedAt: now},
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A key that sorts FIRST alphabetically and carries an EARLIER timestamp
	// than the fields already there, so neither of the fallback orderings could
	// produce a pass by accident.
	if err := repo.AddField(ctx, "t1", &domain.Field{
		ID: uuid.New(), ContentTypeID: id, Key: "abstract", Type: domain.FieldTypeString,
		EnumValues: []string{}, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("add field: %v", err)
	}

	got, err := repo.GetContentTypeByName(ctx, "t1", "article")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for i, k := range []string{"title", "body", "abstract"} {
		if got.Fields[i].Key != k {
			t.Fatalf("field %d is %q, want %q — AddField must append, not sort", i, got.Fields[i].Key, k)
		}
	}
}
