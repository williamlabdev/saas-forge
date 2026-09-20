package repository

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
)

// loadIAMOutboxMigration returns the SQL that creates integration_outbox — it
// lives in the user module's migrations, and the content loader deliberately
// only globs its own directory. 000001 rides along because 000002 alters the
// users table it creates (same pairing outbox's own integration test loads).
func loadIAMOutboxMigration(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "user", "migrations")
	var sql string
	for _, name := range []string{"000001_init_users.up.sql", "000002_iam_outbox.up.sql"} {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixed test-local path
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sql += string(b) + "\n"
	}
	return sql
}

// TestContentEventsAndWebhooks pins the transactional-outbox half of ADR-011
// against a real database: every entry write leaves exactly one event in
// integration_outbox IN THE SAME transaction, and the webhook registry obeys
// the same tenant scoping as every content table.
func TestContentEventsAndWebhooks(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("webhooks"),
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
	if _, err := pool.Exec(ctx, loadIAMOutboxMigration(t)); err != nil {
		t.Fatalf("migrate outbox: %v", err)
	}
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate content: %v", err)
	}

	ob := outbox.NewPostgresRepository(pool)
	repo := NewPostgresContentRepository(pool, ob)

	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','article','')`, typeID); err != nil {
		t.Fatal(err)
	}

	eventRows := func(entryID uuid.UUID) []string {
		rows, err := pool.Query(ctx,
			`SELECT event_type FROM integration_outbox WHERE aggregate_id = $1 ORDER BY created_at`, entryID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var et string
			if err := rows.Scan(&et); err != nil {
				t.Fatal(err)
			}
			out = append(out, et)
		}
		return out
	}

	now := time.Now().UTC()
	e := &domain.Entry{
		ID: uuid.New(), TenantID: "t", ContentTypeID: typeID,
		Payload: []byte(`{}`), Locale: domain.DefaultLocale,
		TranslationGroupID: uuid.New(), CreatedAt: now, UpdatedAt: now,
	}

	t.Run("every entry write leaves its event in the same database", func(t *testing.T) {
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &now); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetEntryPublishState(ctx, e, domain.StatusDraft, nil); err != nil {
			t.Fatal(err)
		}
		if err := repo.DeleteEntry(ctx, "t", typeID, e.ID); err != nil {
			t.Fatal(err)
		}
		got := eventRows(e.ID)
		want := []string{
			outbox.EventContentEntryCreated,
			outbox.EventContentEntryUpdated,
			outbox.EventContentEntryPublished,
			outbox.EventContentEntryUnpublished,
			outbox.EventContentEntryDeleted,
		}
		if len(got) != len(want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("events = %v, want %v", got, want)
			}
		}
	})

	t.Run("the payload names the type and the delete emits nothing on a miss", func(t *testing.T) {
		var payload []byte
		if err := pool.QueryRow(ctx, `
			SELECT payload FROM integration_outbox WHERE aggregate_id = $1 AND event_type = $2`,
			e.ID, outbox.EventContentEntryPublished,
		).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		p, err := outbox.ParseContentPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		if p.ContentType != "article" || p.TenantID != "t" || p.Locale != domain.DefaultLocale {
			t.Fatalf("payload = %+v — consumers think in type names and tenants", p)
		}

		miss := uuid.New()
		if err := repo.DeleteEntry(ctx, "t", typeID, miss); err == nil {
			t.Fatal("expected not-found")
		}
		if events := eventRows(miss); len(events) != 0 {
			t.Fatalf("a 404 delete emitted %v", events)
		}
	})

	t.Run("a rolled-back write leaves no event behind", func(t *testing.T) {
		// The whole reason this is an outbox and not an HTTP call in the handler:
		// the event's existence is decided by the transaction's fate.
		victim := &domain.Entry{
			ID: uuid.New(), TenantID: "t", ContentTypeID: typeID,
			Payload: []byte(`{}`), Locale: domain.DefaultLocale,
			TranslationGroupID: uuid.New(), CreatedAt: now, UpdatedAt: now,
		}
		err := repo.WithTx(ctx, "t", func(txRepo ContentRepository) error {
			if err := txRepo.CreateEntry(ctx, victim); err != nil {
				return err
			}
			return context.Canceled // any error: force the rollback
		})
		if err == nil {
			t.Fatal("expected the injected error")
		}
		if events := eventRows(victim.ID); len(events) != 0 {
			t.Fatalf("rolled-back create left events %v — the outbox pattern is broken", events)
		}
	})

	t.Run("webhook registry is tenant-scoped and feeds the directory", func(t *testing.T) {
		w := &domain.Webhook{
			ID: uuid.New(), TenantID: "t", URL: "https://build.example/hook",
			Secret: "0123456789abcdef0123456789abcdef", Active: true, CreatedAt: now,
		}
		if err := repo.CreateWebhook(ctx, w); err != nil {
			t.Fatal(err)
		}
		inactive := &domain.Webhook{
			ID: uuid.New(), TenantID: "t", URL: "https://paused.example/hook",
			Secret: "0123456789abcdef0123456789abcdef", Active: false, CreatedAt: now,
		}
		if err := repo.CreateWebhook(ctx, inactive); err != nil {
			t.Fatal(err)
		}

		eps, err := repo.ActiveWebhookEndpoints(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		if len(eps) != 1 || eps[0].ID != w.ID || eps[0].Secret != w.Secret {
			t.Fatalf("directory = %+v — it must serve exactly the ACTIVE endpoints, secrets included", eps)
		}

		other, err := repo.ListWebhooks(ctx, "other-tenant")
		if err != nil {
			t.Fatal(err)
		}
		if len(other) != 0 {
			t.Fatalf("cross-tenant list saw %d webhooks", len(other))
		}
		if err := repo.DeleteWebhook(ctx, "other-tenant", w.ID); err == nil {
			t.Fatal("cross-tenant delete must read as not-found")
		}
	})

	t.Run("the DB refuses a webhook the validator would refuse", func(t *testing.T) {
		bad := &domain.Webhook{
			ID: uuid.New(), TenantID: "t", URL: "ftp://example.com/hook",
			Secret: "0123456789abcdef0123456789abcdef", Active: true, CreatedAt: now,
		}
		if err := repo.CreateWebhook(ctx, bad); err == nil {
			t.Fatal("content_webhooks_url_check let a non-http scheme through")
		}
	})
}
