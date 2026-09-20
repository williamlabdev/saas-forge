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
)

// The admin media library against the real database.
//
// memRepo's ListMediaAssets is hand-rolled Go and cannot catch what only SQL
// can get wrong: whether ILIKE actually escapes a caller's literal `%`/`_`,
// whether RLS confines the COUNT and the page to one tenant the same way it
// confines every other list, and whether the LIKE-prefix `kind` filter reads
// content_type the way Postgres reads it rather than the way Go's strings
// package does.
func TestListMediaAssets_Postgres(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("medialist"),
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

	repo := NewPostgresContentRepository(pool, nil)

	// insert goes straight to SQL: what is under test is the WHERE/ORDER BY
	// Postgres runs, not the Go struct that would otherwise sit in the way.
	insert := func(t *testing.T, tenant, filename, contentType string, uploaded bool, createdAt time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var uploadedAt any
		if uploaded {
			uploadedAt = createdAt
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, content_type, size_bytes, filename, uploaded_at, created_at)
			VALUES ($1,$2,$3,$4,1,$5,$6,$7)`,
			id, tenant, tenant+"/"+id.String(), contentType, filename, uploadedAt, createdAt)
		if err != nil {
			t.Fatalf("insert %s: %v", filename, err)
		}
		return id
	}

	base := time.Now().UTC().Add(-time.Hour)
	storefront := insert(t, "t1", "Storefront.png", "image/png", true, base.Add(3*time.Minute))
	_ = insert(t, "t1", "reservation-only.png", "image/png", false, base.Add(2*time.Minute))
	brochure := insert(t, "t1", "brochure.pdf", "application/pdf", true, base.Add(1*time.Minute))
	wild := insert(t, "t1", "100%_off.png", "image/png", true, base)
	other := insert(t, "t2", "other-tenant.png", "image/png", true, base.Add(4*time.Minute))

	t.Run("excludes reservations and other tenants, newest first", func(t *testing.T) {
		items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if total != 3 {
			t.Fatalf("total = %d, want 3 (reservation and t2's asset must not count)", total)
		}
		if len(items) != 3 {
			t.Fatalf("len(items) = %d, want 3", len(items))
		}
		if items[0].ID != storefront || items[len(items)-1].ID != wild {
			t.Fatal("items must be ordered created_at DESC")
		}
		for _, a := range items {
			if a.ID == other {
				t.Fatal("another tenant's asset leaked through")
			}
		}
	})

	t.Run("kind=image reads content_type as a LIKE prefix, not a Go substring", func(t *testing.T) {
		items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{ContentTypePrefix: "image/", Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if total != 2 {
			t.Fatalf("total = %d, want 2 (png x2, pdf excluded)", total)
		}
		for _, a := range items {
			if a.ID == brochure {
				t.Fatal("the pdf must not match kind=image")
			}
		}
	})

	t.Run("a literal %% and _ in the query match themselves, not the wildcard", func(t *testing.T) {
		items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{Query: "100%_off", Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(items) != 1 || items[0].ID != wild {
			t.Fatalf("unescaped ILIKE would also match brochure/storefront; got total=%d items=%d", total, len(items))
		}
	})

	t.Run("q matches case-insensitively", func(t *testing.T) {
		items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{Query: "store", Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(items) != 1 || items[0].ID != storefront {
			t.Fatalf("ILIKE %%store%% must match Storefront.png case-insensitively; total=%d items=%d", total, len(items))
		}
	})

	t.Run("limit and offset page the same tenant-scoped result", func(t *testing.T) {
		items, total, err := repo.ListMediaAssets(ctx, "t1", MediaListFilter{Limit: 1, Offset: 1})
		if err != nil {
			t.Fatal(err)
		}
		if total != 3 {
			t.Fatalf("total must count the whole filtered set regardless of the page, got %d", total)
		}
		if len(items) != 1 || items[0].ID != brochure {
			t.Fatal("offset=1 of the newest-first order must land on brochure.pdf")
		}
	})
}
