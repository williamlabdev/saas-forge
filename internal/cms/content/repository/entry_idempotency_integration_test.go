package repository

import (
	"bytes"
	"context"
	"encoding/json"
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

// entry_idempotency against the real database (ADR-013 §9).
//
// It has to be an integration test, and the reason is the one the memRepo note
// gives: every claim below lives in SQL and nowhere else. The PRIMARY KEY is
// what makes a concurrent second insert fail — the service's whole race
// handling hangs off that error and a Go map returning it proves only that the
// fake was written to. The CASCADE is a database rule; the tenant isolation is
// an RLS policy; the transaction is a transaction. A fake can agree with all
// four while none of them exist.
func TestEntryIdempotency(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("idempotency"),
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
	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	// The other tenant needs its own type row: entries reference one, and a
	// cross-tenant test that shared a type would be testing nothing about
	// tenants.
	otherTypeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'other','post','')`, otherTypeID); err != nil {
		t.Fatalf("seed other type: %v", err)
	}

	// Truncated to what timestamptz stores — see revision_integration_test.go for
	// why this is done at the SOURCE and why the bug it prevents is structurally
	// invisible on a Darwin machine.
	now := time.Now().UTC().Truncate(time.Microsecond)

	newEntry := func(t *testing.T, tenant string, ctID uuid.UUID, title string) uuid.UUID {
		t.Helper()
		e := &domain.Entry{
			ID: uuid.New(), TenantID: tenant, ContentTypeID: ctID,
			Payload: json.RawMessage(`{"title":"` + title + `"}`),
			Version: 1, Locale: domain.DefaultLocale, TranslationGroupID: uuid.New(),
			Status: domain.StatusDraft, CreatedAt: now, UpdatedAt: now,
		}
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatalf("create entry: %v", err)
		}
		return e.ID
	}
	rows := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM entry_idempotency`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	fp := []byte{0xde, 0xad, 0xbe, 0xef}

	t.Run("a record round-trips with its fingerprint intact", func(t *testing.T) {
		entryID := newEntry(t, "t", typeID, "round-trip")
		rec := EntryIdempotency{
			TenantID: "t", ActorKey: "human:" + uuid.NewString(), Key: "round-trip-0001",
			Fingerprint: fp, EntryID: entryID,
		}
		if err := repo.RecordEntryIdempotency(ctx, rec); err != nil {
			t.Fatalf("record: %v", err)
		}
		got, err := repo.FindEntryIdempotency(ctx, "t", rec.ActorKey, rec.Key)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if got == nil {
			t.Fatal("the record we just wrote is not there")
		}
		if got.EntryID != entryID {
			t.Fatalf("entry id: got %s want %s", got.EntryID, entryID)
		}
		// bytea, not text: a column typed wrongly would round-trip a printable
		// digest and mangle this one.
		if !bytes.Equal(got.Fingerprint, fp) {
			t.Fatalf("fingerprint: got %x want %x", got.Fingerprint, fp)
		}
	})

	t.Run("an unspent key is nil, nil — not an error", func(t *testing.T) {
		got, err := repo.FindEntryIdempotency(ctx, "t", "human:nobody", "never-used-0001")
		if err != nil {
			t.Fatalf("looking for a key nobody spent must not be an error: %v", err)
		}
		if got != nil {
			t.Fatalf("got a record for a key nobody spent: %+v", got)
		}
	})

	t.Run("the primary key refuses a second insert", func(t *testing.T) {
		actor := "human:" + uuid.NewString()
		const key = "collide-0001"
		first := newEntry(t, "t", typeID, "first")
		if err := repo.RecordEntryIdempotency(ctx, EntryIdempotency{
			TenantID: "t", ActorKey: actor, Key: key, Fingerprint: fp, EntryID: first,
		}); err != nil {
			t.Fatalf("first record: %v", err)
		}
		second := newEntry(t, "t", typeID, "second")
		err := repo.RecordEntryIdempotency(ctx, EntryIdempotency{
			TenantID: "t", ActorKey: actor, Key: key, Fingerprint: fp, EntryID: second,
		})
		// The specific error matters, not merely that it failed: the service
		// branches on it to replay the winner, and a generic wrapped error there
		// would surface to the caller as a 500 on a retry that should have
		// succeeded.
		if !errors.Is(err, ErrIdempotencyKeyTaken) {
			t.Fatalf("want ErrIdempotencyKeyTaken, got %v", err)
		}
	})

	t.Run("the same key under a different actor is a different record", func(t *testing.T) {
		const key = "shared-key-0001"
		a, b := "human:"+uuid.NewString(), "human:"+uuid.NewString()
		aEntry, bEntry := newEntry(t, "t", typeID, "a"), newEntry(t, "t", typeID, "b")

		if err := repo.RecordEntryIdempotency(ctx, EntryIdempotency{
			TenantID: "t", ActorKey: a, Key: key, Fingerprint: fp, EntryID: aEntry,
		}); err != nil {
			t.Fatalf("a: %v", err)
		}
		// This is the ruling of 2026-08-06 expressed in the schema. Drop actor_key
		// from the PRIMARY KEY and this insert fails instead.
		if err := repo.RecordEntryIdempotency(ctx, EntryIdempotency{
			TenantID: "t", ActorKey: b, Key: key, Fingerprint: fp, EntryID: bEntry,
		}); err != nil {
			t.Fatalf("b must not collide with a: %v", err)
		}

		got, err := repo.FindEntryIdempotency(ctx, "t", b, key)
		if err != nil || got == nil {
			t.Fatalf("find b: %v %+v", err, got)
		}
		if got.EntryID != bEntry {
			t.Fatal("b's key resolved a's entry — the namespace is not per-issuer")
		}
	})

	t.Run("another tenant's record is invisible", func(t *testing.T) {
		actor := "human:" + uuid.NewString()
		const key = "tenant-scoped-0001"
		otherEntry := newEntry(t, "other", otherTypeID, "theirs")
		if err := repo.RecordEntryIdempotency(ctx, EntryIdempotency{
			TenantID: "other", ActorKey: actor, Key: key, Fingerprint: fp, EntryID: otherEntry,
		}); err != nil {
			t.Fatalf("record for other tenant: %v", err)
		}
		got, err := repo.FindEntryIdempotency(ctx, "t", actor, key)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if got != nil {
			t.Fatal("a key spent in another tenant resolved here")
		}
	})

	t.Run("deleting the entry takes the record with it", func(t *testing.T) {
		actor := "human:" + uuid.NewString()
		entryID := newEntry(t, "t", typeID, "doomed")
		if err := repo.RecordEntryIdempotency(ctx, EntryIdempotency{
			TenantID: "t", ActorKey: actor, Key: "cascade-0001", Fingerprint: fp, EntryID: entryID,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := repo.DeleteEntry(ctx, "t", typeID, entryID); err != nil {
			t.Fatalf("delete entry: %v", err)
		}
		got, err := repo.FindEntryIdempotency(ctx, "t", actor, "cascade-0001")
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		// Without the cascade this row survives pointing at nothing, and the next
		// replay hands the caller a 404 from a create.
		if got != nil {
			t.Fatal("the record outlived the entry it names")
		}
	})

	t.Run("the entry and its record commit or roll back together", func(t *testing.T) {
		before := rows(t)
		boom := errors.New("boom")
		var abandoned uuid.UUID

		err := repo.WithTx(ctx, "t", func(r ContentRepository) error {
			e := &domain.Entry{
				ID: uuid.New(), TenantID: "t", ContentTypeID: typeID,
				Payload: json.RawMessage(`{"title":"abandoned"}`),
				Version: 1, Locale: domain.DefaultLocale, TranslationGroupID: uuid.New(),
				Status: domain.StatusDraft, CreatedAt: now, UpdatedAt: now,
			}
			if err := r.CreateEntry(ctx, e); err != nil {
				return err
			}
			abandoned = e.ID
			if err := r.RecordEntryIdempotency(ctx, EntryIdempotency{
				TenantID: "t", ActorKey: "human:rollback", Key: "rollback-0001",
				Fingerprint: fp, EntryID: e.ID,
			}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("want the callback's error back, got %v", err)
		}
		if rows(t) != before {
			t.Fatal("the idempotency record survived a rolled-back transaction")
		}
		// The entry half matters more: an entry committed without its record is
		// the duplicate the whole mechanism exists to prevent, and the caller who
		// retries would create a second one.
		if _, err := repo.GetEntry(ctx, "t", typeID, abandoned); err == nil {
			t.Fatal("the entry survived a rolled-back transaction")
		}
	})
}
