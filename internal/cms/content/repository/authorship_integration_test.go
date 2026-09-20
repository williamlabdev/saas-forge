package repository

import (
	"context"
	"encoding/json"
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

// Authorship columns against the real database. The service-level fake can
// mirror the assignments, but it cannot enforce a CHECK constraint and it cannot
// tell you whether the SQL actually wrote the columns — both read paths build
// their own statement, and a column added to one and forgotten in the other
// returns a zero value that looks like data rather than a bug.
func TestEntryAuthorship(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("authorship"),
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
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}

	repo := NewPostgresContentRepository(pool, nil)
	author, editor, publisher := uuid.New(), uuid.New(), uuid.New()

	// The constraint, first: it is the only guard that survives a future writer
	// who forgets the rule, and it is the one thing no Go fake can assert.
	t.Run("a draft may not name a publisher", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_by)
			VALUES ($1,'t',$2,'{}','draft',$3)`, uuid.New(), typeID, publisher)
		if err == nil {
			t.Fatal("expected entries_published_by_snapshot_check to reject a draft with published_by")
		}
	})

	t.Run("a published row may have no publisher", func(t *testing.T) {
		// Every row predating migration 000021 is exactly this shape. Requiring a
		// value here would have forced a fabricated one.
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_at, published_payload, published_version)
			VALUES ($1,'t',$2,'{}','published',NOW(),'{}',1)`, uuid.New(), typeID); err != nil {
			t.Fatalf("a published row with NULL published_by must be legal: %v", err)
		}
	})

	entryID := uuid.New()
	now := time.Now().UTC()

	t.Run("create records author and editor", func(t *testing.T) {
		e := &domain.Entry{
			ID: entryID, TenantID: "t", ContentTypeID: typeID,
			Payload:            json.RawMessage(`{"title":"v1"}`),
			Version:            1,
			Status:             domain.StatusDraft,
			Locale:             domain.DefaultLocale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &author,
			CreatedAt:          now, UpdatedAt: now,
		}
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		got := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireUUID(t, "created_by", got.CreatedBy, author)
		requireUUID(t, "updated_by", got.UpdatedBy, author)
		if got.PublishedBy != nil {
			t.Fatal("a draft must not have a publisher")
		}
	})

	t.Run("update moves updated_by and leaves created_by", func(t *testing.T) {
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		e.Payload = json.RawMessage(`{"title":"v2"}`)
		e.UpdatedAt = now.Add(time.Minute)
		e.UpdatedBy = &editor
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		got := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireUUID(t, "created_by", got.CreatedBy, author)
		requireUUID(t, "updated_by", got.UpdatedBy, editor)
	})

	t.Run("publish records the publisher, unpublish keeps it", func(t *testing.T) {
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		published := now.Add(2 * time.Minute)
		e.UpdatedAt = published
		e.UpdatedBy = &publisher
		e.PublishedBy = &publisher
		if err := repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &published); err != nil {
			t.Fatal(err)
		}
		got := mustGetEntry(t, ctx, repo, typeID, entryID)
		// Guard: prove it was set by the publish before asserting what a retract
		// does to it, or the assertion below would pass against a column nobody
		// ever wrote.
		requireUUID(t, "published_by", got.PublishedBy, publisher)

		// Retract with a DIFFERENT actor, and hand the repository THAT actor as
		// published_by — which is what the service does, unconditionally. Two
		// separate things are being pinned here:
		//
		//   updated_by must follow the person doing the retracting (binding both
		//   to one parameter would null it, and an unpublish has an editor), and
		//
		//   published_by must still name the PUBLISHER afterwards. Since
		//   ADR-014 §5.1 the snapshot survives a retract, so the column that
		//   describes it survives too; the retracting actor passed in here must
		//   not overwrite it. Passing `editor` rather than `publisher` is the
		//   whole point of the subtest — with the old CASE the column went NULL,
		//   and with a naive keep-the-parameter version it would say `editor`.
		got.UpdatedAt = published.Add(time.Minute)
		got.UpdatedBy = &editor
		got.PublishedBy = &editor
		if err := repo.SetEntryPublishState(ctx, got, domain.StatusDraft, nil); err != nil {
			t.Fatal(err)
		}
		// The in-memory struct, not just the row: the repository reads the
		// surviving value back through RETURNING precisely so the DTO built from
		// this struct does not name whoever took the entry down as its publisher.
		requireUUID(t, "published_by (returned struct)", got.PublishedBy, publisher)

		after := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireUUID(t, "published_by", after.PublishedBy, publisher)
		requireUUID(t, "updated_by", after.UpdatedBy, editor)
		requireUUID(t, "created_by", after.CreatedBy, author)

		// The snapshot itself outlives the retract, which is what published_by is
		// describing. Asserted here rather than left to the constraint subtests
		// because a version of this change that kept the actor but dropped the
		// content would satisfy every assertion above and still lose the thing
		// §5.1 exists to keep.
		if after.PublishedPayload == nil {
			t.Fatal("retract must keep published_payload — the actor above would then describe nothing")
		}
		if after.PublishedVersion == 0 {
			t.Fatal("retract must keep published_version")
		}
	})

	// Two statements, one behaviour: GetEntry and ListEntries select through the
	// shared entrySelectColumns, and this is what proves the shared list is
	// actually reaching both — a regression here is silent (zero values, not an
	// error), which is why it is asserted rather than assumed.
	t.Run("both read paths return authorship", func(t *testing.T) {
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		var found *domain.Entry
		for _, it := range items {
			if it.ID == entryID {
				found = it
			}
		}
		if found == nil {
			t.Fatal("seeded entry missing from ListEntries")
		}
		requireUUID(t, "created_by via ListEntries", found.CreatedBy, author)
		requireUUID(t, "updated_by via ListEntries", found.UpdatedBy, editor)
	})
}

// Last-write provenance (ADR-014 §4, migration 000031) against the real
// database. Separate from TestEntryAuthorship above because the interesting
// assertions here are the CHECK constraints and the NULL state, neither of
// which the Go fake in the service package can hold an opinion about.
func TestEntryUpdateProvenance(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("updateprov"),
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
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}

	repo := NewPostgresContentRepository(pool, nil)
	principal, otherPerson := uuid.New(), uuid.New()
	botName := "content-bot"

	// insertRaw goes around the repository on purpose: these cases are about what
	// the SCHEMA refuses, and the schema is the guard that outlives every writer.
	insertRaw := func(t *testing.T, updatedBy *uuid.UUID, kind, agentID *string) error {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, updated_by, updated_by_kind, updated_by_agent)
			VALUES ($1,'t',$2,'{}','draft',$3,$4,$5)`,
			uuid.New(), typeID, updatedBy, kind, agentID)
		return err
	}
	strptr := func(s string) *string { return &s }
	agentKind, humanKind := domain.ActorKindAgent, domain.ActorKindHuman

	// The NULL state is asserted FIRST and as a positive: it is the one the
	// biconditional would have destroyed had it been copied from 000030 verbatim
	// (`(kind='agent') = (agent IS NOT NULL)` is NULL when kind is NULL, and a
	// CHECK passes on NULL — so the verbatim copy admits case 2 below instead).
	// Without this subtest, a schema that rejected "not recorded" outright would
	// still pass every other assertion in this file.
	t.Run("both columns NULL is legal — not recorded", func(t *testing.T) {
		if err := insertRaw(t, &principal, nil, nil); err != nil {
			t.Fatalf("a row whose last write predates 000031 must be legal: %v", err)
		}
	})

	t.Run("an agent id without the agent kind is refused", func(t *testing.T) {
		if err := insertRaw(t, &principal, nil, strptr(botName)); err == nil {
			t.Fatal("expected entries_updated_by_agent_kind_check to reject a half-written pair")
		}
		if err := insertRaw(t, &principal, &humanKind, strptr(botName)); err == nil {
			t.Fatal("expected entries_updated_by_agent_kind_check to reject a human row naming an agent")
		}
	})

	t.Run("the agent kind without an agent id is refused", func(t *testing.T) {
		if err := insertRaw(t, &principal, &agentKind, nil); err == nil {
			t.Fatal("expected entries_updated_by_agent_kind_check to reject the same bug from the other side")
		}
	})

	t.Run("an agent write with nobody answerable is refused", func(t *testing.T) {
		if err := insertRaw(t, nil, &agentKind, strptr(botName)); err == nil {
			t.Fatal("expected entries_updated_by_agent_has_principal_check to reject an unanswerable agent write")
		}
	})

	entryID := uuid.New()
	now := time.Now().UTC()

	t.Run("create copies the created pair onto the updated pair", func(t *testing.T) {
		e := &domain.Entry{
			ID: entryID, TenantID: "t", ContentTypeID: typeID,
			Payload:            json.RawMessage(`{"title":"v1"}`),
			Version:            1,
			Status:             domain.StatusDraft,
			Locale:             domain.DefaultLocale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &principal,
			CreatedByKind:      domain.ActorKindHuman,
			CreatedAt:          now, UpdatedAt: now,
		}
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		// The struct the caller still holds, not just the row: the service projects
		// its response DTO from it, so a create that wrote the columns but left the
		// in-memory entry blank would answer "unknown" for a row it just authored.
		requireKind(t, "updated_by_kind on the returned struct", e.UpdatedByKind, domain.ActorKindHuman)

		got := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireKind(t, "updated_by_kind", got.UpdatedByKind, domain.ActorKindHuman)
		if got.UpdatedByAgent != nil {
			t.Fatalf("a human create must not name an agent, got %q", *got.UpdatedByAgent)
		}
	})

	// The scenario ADR-014 opens with, and the whole reason this migration is not
	// just created_by_* read twice: a human's entry that a bot edited later.
	t.Run("an agent edit moves the updated pair and leaves the created pair", func(t *testing.T) {
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		e.Payload = json.RawMessage(`{"title":"v2"}`)
		e.UpdatedAt = now.Add(time.Minute)
		e.UpdatedBy = &principal
		e.UpdatedByKind, e.UpdatedByAgent = &agentKind, &botName
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		got := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireKind(t, "updated_by_kind", got.UpdatedByKind, domain.ActorKindAgent)
		if got.UpdatedByAgent == nil || *got.UpdatedByAgent != botName {
			t.Fatalf("updated_by_agent = %v, want %q", got.UpdatedByAgent, botName)
		}
		// created_by_* describes how the row came into being and must not follow.
		// Without this half, a bug that overwrote BOTH pairs on every edit would
		// pass everything above.
		if got.CreatedByKind != domain.ActorKindHuman {
			t.Fatalf("created_by_kind = %q, want human — a later edit does not restate authorship", got.CreatedByKind)
		}
		if got.CreatedByAgent != nil {
			t.Fatalf("created_by_agent = %q, want nil", *got.CreatedByAgent)
		}
	})

	// Publish and unpublish are writes, so the pair has to move with updated_by
	// there too — otherwise a person pressing publish over a bot's draft leaves
	// the row saying the bot made the last change.
	t.Run("publish and unpublish move the pair with updated_by", func(t *testing.T) {
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		published := now.Add(2 * time.Minute)
		e.UpdatedAt = published
		e.UpdatedBy = &otherPerson
		e.UpdatedByKind, e.UpdatedByAgent = &humanKind, nil
		e.PublishedBy = &otherPerson
		if err := repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &published); err != nil {
			t.Fatal(err)
		}
		got := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireUUID(t, "updated_by after publish", got.UpdatedBy, otherPerson)
		requireKind(t, "updated_by_kind after publish", got.UpdatedByKind, domain.ActorKindHuman)
		if got.UpdatedByAgent != nil {
			t.Fatalf("publishing by hand must clear the agent id the previous edit left, got %q", *got.UpdatedByAgent)
		}

		// And back the other way, so the assertion above is not just "the column
		// happens to hold human": an agent retracting sets the pair to agent.
		got.UpdatedAt = published.Add(time.Minute)
		got.UpdatedBy = &principal
		got.UpdatedByKind, got.UpdatedByAgent = &agentKind, &botName
		if err := repo.SetEntryPublishState(ctx, got, domain.StatusDraft, nil); err != nil {
			t.Fatal(err)
		}
		after := mustGetEntry(t, ctx, repo, typeID, entryID)
		requireKind(t, "updated_by_kind after retract", after.UpdatedByKind, domain.ActorKindAgent)
		if after.UpdatedByAgent == nil || *after.UpdatedByAgent != botName {
			t.Fatalf("updated_by_agent after retract = %v, want %q", after.UpdatedByAgent, botName)
		}
	})

	// The shared-column-list regression, for the new pair: GetEntry and
	// ListEntries build their own statements and a column reaching only one of
	// them comes back as a zero value, which reads as data rather than a bug.
	t.Run("both read paths return the updated pair", func(t *testing.T) {
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		var found *domain.Entry
		for _, it := range items {
			if it.ID == entryID {
				found = it
			}
		}
		if found == nil {
			t.Fatal("seeded entry missing from ListEntries")
		}
		requireKind(t, "updated_by_kind via ListEntries", found.UpdatedByKind, domain.ActorKindAgent)
		if found.UpdatedByAgent == nil || *found.UpdatedByAgent != botName {
			t.Fatalf("updated_by_agent via ListEntries = %v, want %q", found.UpdatedByAgent, botName)
		}
	})
}

func requireKind(t *testing.T, what string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil (unrecorded), want %q", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %q, want %q", what, *got, want)
	}
}

func mustGetEntry(t *testing.T, ctx context.Context, repo *PostgresContentRepository, typeID, id uuid.UUID) *domain.Entry {
	t.Helper()
	e, err := repo.GetEntry(ctx, "t", typeID, id)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	return e
}

func requireUUID(t *testing.T, what string, got *uuid.UUID, want uuid.UUID) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %s", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %s, want %s", what, *got, want)
	}
}
