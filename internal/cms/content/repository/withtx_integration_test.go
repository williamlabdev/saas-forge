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
)

// Several schema verbs, one transaction, all or nothing.
//
// This suite only exists against Postgres, and the reason SURVIVED the fake
// changing under it. memRepo's WithTx used to keep whatever the callback wrote;
// as of ADR-013 step 7 it snapshot-restores its collections, because a service
// test needed to see a create rolled back. That makes the fake more faithful
// and changes nothing about why this file is here: a Go slice restored by a
// deferred assignment is not evidence that Postgres issued a ROLLBACK, and it
// cannot see the failure modes that live in SQL — a constraint that fires
// mid-statement, a savepoint that was never opened, RLS refusing a row the
// callback thought it wrote. The fake now agrees with this suite; agreement is
// not proof, and the only thing that can be wrong here is the database.
func TestWithTxIsAllOrNothing(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("withtx"),
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

	newType := func(name string) *domain.ContentType {
		now := time.Now().UTC()
		id := uuid.New()
		return &domain.ContentType{
			ID: id, TenantID: "t", Name: name, Label: name,
			CreatedAt: now, UpdatedAt: now,
			Fields: []domain.Field{{
				ID: uuid.New(), ContentTypeID: id, Key: "title",
				Type: domain.FieldTypeString, EnumValues: []string{}, CreatedAt: now,
			}},
		}
	}

	t.Run("a failure partway leaves nothing behind", func(t *testing.T) {
		boom := errors.New("boom")
		// Two verbs, then a refusal. Without a shared transaction the type and
		// its extra field are already committed by the time the third step
		// fails — which is exactly the half-applied schema ADR-007 refuses to
		// produce, arriving through the importer instead of through an endpoint.
		err := repo.WithTx(ctx, "t", func(r ContentRepository) error {
			ct := newType("halfway")
			if err := r.CreateContentType(ctx, ct); err != nil {
				return err
			}
			extra := domain.Field{
				ID: uuid.New(), ContentTypeID: ct.ID, Key: "body",
				Type: domain.FieldTypeText, EnumValues: []string{}, CreatedAt: time.Now().UTC(),
			}
			if err := r.AddField(ctx, "t", &extra); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("want the callback's error back, got %v", err)
		}
		if _, err := repo.GetContentTypeByName(ctx, "t", "halfway"); err == nil {
			t.Fatal("the type survived a failed transaction — the verbs are committing independently")
		}
		// The type row is gone; assert the FIELD rows went with it. They live in
		// content_type_fields, which has no RLS and no tenant column, so a field
		// left behind would be invisible to every tenant-scoped query and would
		// only surface when the name was reused.
		var orphans int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM content_type_fields WHERE key IN ('title','body')`).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if orphans != 0 {
			t.Fatalf("%d orphan field row(s) survived the rollback", orphans)
		}
	})

	t.Run("success commits every verb", func(t *testing.T) {
		err := repo.WithTx(ctx, "t", func(r ContentRepository) error {
			ct := newType("article")
			if err := r.CreateContentType(ctx, ct); err != nil {
				return err
			}
			extra := domain.Field{
				ID: uuid.New(), ContentTypeID: ct.ID, Key: "body",
				Type: domain.FieldTypeText, EnumValues: []string{}, CreatedAt: time.Now().UTC(),
			}
			return r.AddField(ctx, "t", &extra)
		})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		ct, err := repo.GetContentTypeByName(ctx, "t", "article")
		if err != nil {
			t.Fatalf("the type did not commit: %v", err)
		}
		if _, ok := ct.FieldByKey("body"); !ok {
			t.Fatal("the second verb's field did not commit — the callback is not sharing one transaction")
		}
	})

	t.Run("a second tenant inside the transaction is refused", func(t *testing.T) {
		// app.tenant_id is set once, when the transaction opens. An operation
		// for another tenant would run under the FIRST tenant's RLS scope and
		// write rows attributed to it — cross-tenant corruption arriving through
		// a door ADR-007's cascade join closed elsewhere.
		err := repo.WithTx(ctx, "t", func(r ContentRepository) error {
			other := newType("intruder")
			other.TenantID = "other"
			return r.CreateContentType(ctx, other)
		})
		if err == nil {
			t.Fatal("a write for a second tenant was accepted inside the transaction")
		}
		var leaked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM content_types WHERE name = 'intruder'`).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatalf("%d row(s) written for the wrong tenant", leaked)
		}
	})
}
