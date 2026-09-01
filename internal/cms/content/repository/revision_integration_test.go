package repository

import (
	"context"
	"encoding/json"
	"fmt"
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

// Entry revisions against the real database (ADR-014 §5).
//
// It has to be an integration test rather than a service-level one for the
// reason authorship_integration_test.go gives about its own columns, plus one
// this table adds: the revision is written by an INSERT ... SELECT that reads
// `entries` back inside the same transaction, so what it stores is decided by
// the database, not by anything a Go fake could stand in for. A fake would be
// asserting that the test's own expectation equals the test's own expectation.
//
// MUTATION VERIFICATION (actually run 2026-08-06; the results are recorded
// because they are not the tidy ones a reader would assume):
//
//   - Delete recordEntryRevision from CreateEntry → 4 red: "create stores
//     version 1", "an update ... leaves the old payload standing", "deleting the
//     entry", "RLS hides another tenant's".
//   - Delete it from UpdateEntry → 3 red: "an update ...", "an agent write",
//     AND "publish and unpublish ... store no revision".
//
// The read cap (added 0806, william's retention ruling) was mutated three ways,
// each aimed at a different assertion of the last subtest, and each turned that
// subtest and only that subtest red:
//
//   - Drop the LIMIT clause → 51 returned, the count assertion fires.
//   - entryRevisionListLimit 50 → 5 → red, which is the point of writing the
//     numbers out instead of importing them: the ruling's NUMBER is pinned, not
//     merely the fact that some cap exists.
//   - Turn the cap into a real purge (DELETE past 50 inside recordEntryRevision,
//     LIMIT left in place) → the list assertions all still pass and "the table
//     holds 50 revisions, want all 51" fires alone. That mutation is the one
//     worth keeping in mind: it is a plausible future edit that looks like
//     honouring the same ruling, and only the raw-count assertion can see the
//     difference between hiding rows and destroying them.
//
// THE SUBTESTS ARE NOT INDEPENDENT, and the second result is the one worth
// knowing. Its own sparseness assertions — the before/after counts across a
// publish and a retract — do survive that mutation; what fails is the pair at
// the end asserting that the newest stored revision trails the entry version
// and still holds the published payload, and those need an update revision to
// exist before they can say anything. Left that way on purpose: a history whose
// newest row is the create is not a history this table's read rule can be
// demonstrated against, and reporting that as green would be the more
// misleading of the two options.
func TestEntryRevisions(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("revisions"),
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
	author, editor, principal := uuid.New(), uuid.New(), uuid.New()
	agentID := "content-bot"

	entryID := uuid.New()
	// TRUNCATED TO THE PRECISION POSTGRES ACTUALLY STORES. timestamptz keeps
	// microseconds; a Go instant carries nanoseconds, so an untruncated `now`
	// comes back from the database as a DIFFERENT value and the created_at
	// assertion below compares 05:56:44.935521 against .935521627.
	//
	// It has to be truncated at the source rather than at the comparison because
	// every timestamp in this test derives from this one (now.Add(n*time.Minute)
	// carries the same remainder), so fixing one assertion would leave the trap
	// armed for the next one added.
	//
	// THIS BUG WAS INVISIBLE ON THE MACHINE IT WAS WRITTEN ON. Darwin's
	// time.Now() is microsecond-granular — the nanosecond field is always a
	// multiple of 1000 — so the truncation is a no-op locally and the test was
	// green here while failing on CI's Linux runner. Verified rather than
	// assumed: injecting .Add(627*time.Nanosecond) reproduces CI's exact failure
	// locally, and the same injection with this Truncate in place passes.
	// user/repository/postgres_integration_test.go:180 already carried the fix;
	// this file did not inherit it.
	now := time.Now().UTC().Truncate(time.Microsecond)

	revisions := func(t *testing.T, id uuid.UUID) []domain.EntryRevision {
		t.Helper()
		revs, err := repo.ListEntryRevisions(ctx, "t", id)
		if err != nil {
			t.Fatalf("list revisions: %v", err)
		}
		return revs
	}
	// Titles are compared through the JSON, never as raw bytes: jsonb is stored
	// decomposed and comes back with its own whitespace, so a byte comparison
	// would be testing Postgres's formatter.
	titleOf := func(t *testing.T, raw json.RawMessage) string {
		t.Helper()
		var doc struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("payload is not the document we stored: %v (%s)", err, raw)
		}
		return doc.Title
	}

	t.Run("create stores version 1 as the row itself", func(t *testing.T) {
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
		revs := revisions(t, entryID)
		if len(revs) != 1 {
			t.Fatalf("want 1 revision after a create, got %d", len(revs))
		}
		if revs[0].Version != 1 {
			t.Fatalf("want version 1, got %d", revs[0].Version)
		}
		// Hard-written, not read back off the entry: sourcing the expectation
		// from the row would pass against a revision that stored the wrong
		// entry's payload as readily as the right one.
		if got := titleOf(t, revs[0].Payload); got != "v1" {
			t.Fatalf("revision payload is %q, want %q", got, "v1")
		}
		requireAuthor(t, revs[0], "human", &author, nil)
		// One instant for the write and its revision — the join key §1's
		// attribution window is expressed in.
		if !revs[0].CreatedAt.Equal(now) {
			t.Fatalf("revision created_at is %s, want the entry's updated_at %s", revs[0].CreatedAt, now)
		}
	})

	t.Run("an update stores the new version and leaves the old payload standing", func(t *testing.T) {
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		e.Payload = json.RawMessage(`{"title":"v2"}`)
		e.UpdatedAt = now.Add(time.Minute)
		e.UpdatedBy = &editor
		human := domain.ActorKindHuman
		e.UpdatedByKind = &human
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		revs := revisions(t, entryID)
		if len(revs) != 2 {
			t.Fatalf("want 2 revisions after create+update, got %d", len(revs))
		}
		// Newest first.
		if got := titleOf(t, revs[0].Payload); got != "v2" {
			t.Fatalf("newest revision is %q, want %q", got, "v2")
		}
		requireAuthor(t, revs[0], "human", &editor, nil)

		// THE POINT OF THE WHOLE TABLE. The overwritten value is still readable
		// after the write that replaced it — that is what §5 buys, and without
		// this assertion a table that stored only the current payload twice
		// would pass everything above.
		if got := titleOf(t, revs[1].Payload); got != "v1" {
			t.Fatalf("the overwritten revision is %q, want the pre-update %q", got, "v1")
		}
		requireAuthor(t, revs[1], "human", &author, nil)
	})

	t.Run("an agent write names the agent and the principal answering for it", func(t *testing.T) {
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		e.Payload = json.RawMessage(`{"title":"v3"}`)
		e.UpdatedAt = now.Add(2 * time.Minute)
		e.UpdatedBy = &principal
		agent := domain.ActorKindAgent
		e.UpdatedByKind, e.UpdatedByAgent = &agent, &agentID
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		revs := revisions(t, entryID)
		if got := titleOf(t, revs[0].Payload); got != "v3" {
			t.Fatalf("newest revision is %q, want %q", got, "v3")
		}
		// The trio, all three of it: a kind alone would let the history say a bot
		// wrote this without saying which bot or who answers for it — the
		// half-written state ADR-013 §2 exists to keep out.
		requireAuthor(t, revs[0], "agent", &principal, &agentID)
	})

	t.Run("publish and unpublish bump the version and store no revision", func(t *testing.T) {
		before := revisions(t, entryID)
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		versionBefore := e.Version

		published := now.Add(3 * time.Minute)
		e.UpdatedAt, e.UpdatedBy, e.PublishedBy = published, &editor, &editor
		human := domain.ActorKindHuman
		e.UpdatedByKind, e.UpdatedByAgent = &human, nil
		if err := repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &published); err != nil {
			t.Fatal(err)
		}
		// The control. Without it, "no new revision" would also pass if the
		// publish had silently done nothing at all.
		after := mustGetEntry(t, ctx, repo, typeID, entryID)
		if after.Version <= versionBefore {
			t.Fatalf("publish did not bump the version (%d -> %d); the assertion below would prove nothing",
				versionBefore, after.Version)
		}
		if got := revisions(t, entryID); len(got) != len(before) {
			t.Fatalf("publish stored a revision: %d -> %d", len(before), len(got))
		}

		retracted := now.Add(4 * time.Minute)
		after.UpdatedAt, after.UpdatedBy = retracted, &editor
		if err := repo.SetEntryPublishState(ctx, after, domain.StatusDraft, nil); err != nil {
			t.Fatal(err)
		}
		if got := revisions(t, entryID); len(got) != len(before) {
			t.Fatalf("unpublish stored a revision: %d -> %d", len(before), len(got))
		}

		// Sparseness stated as an assertion rather than left to the header: the
		// newest revision is now BEHIND the entry's version, which is exactly why
		// a reader resolves "the content at version N" as the newest revision
		// with version <= N. A reader written as `version = N` would find nothing
		// here and report the entry as having no history.
		final := mustGetEntry(t, ctx, repo, typeID, entryID)
		newest := revisions(t, entryID)[0]
		if newest.Version >= final.Version {
			t.Fatalf("expected the newest revision (%d) to trail the entry version (%d) after two status writes",
				newest.Version, final.Version)
		}
		// And the lookup rule resolves the snapshot: what was published is the
		// payload of the newest revision at or below published_version.
		if got := titleOf(t, newest.Payload); got != "v3" {
			t.Fatalf("newest revision is %q, want the payload that was published, %q", got, "v3")
		}
	})

	t.Run("a refused update stores nothing", func(t *testing.T) {
		before := revisions(t, entryID)
		e := mustGetEntry(t, ctx, repo, typeID, entryID)
		e.Payload = json.RawMessage(`{"title":"never happened"}`)
		e.Version = e.Version - 1 // the version moved under us
		e.UpdatedAt = now.Add(5 * time.Minute)
		e.UpdatedBy = &editor
		if err := repo.UpdateEntry(ctx, e); err == nil {
			t.Fatal("expected a version conflict")
		}
		// The atomicity claim in recordEntryRevision's doc comment, tested rather
		// than asserted in prose: a revision surviving a rolled-back write is a
		// version of the content that never existed, offered to a later reader as
		// the thing to put back.
		after := revisions(t, entryID)
		if len(after) != len(before) {
			t.Fatalf("a refused update stored a revision: %d -> %d", len(before), len(after))
		}
		for _, rev := range after {
			if titleOf(t, rev.Payload) == "never happened" {
				t.Fatalf("version %d holds a payload that was never written", rev.Version)
			}
		}
	})

	t.Run("deleting the entry takes its revisions with it", func(t *testing.T) {
		// A separate entry: the sequence above is still the subject of the
		// assertions before this one.
		doomed := uuid.New()
		e := &domain.Entry{
			ID: doomed, TenantID: "t", ContentTypeID: typeID,
			Payload:            json.RawMessage(`{"title":"doomed"}`),
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
		if len(revisions(t, doomed)) == 0 {
			t.Fatal("nothing to cascade: the create stored no revision")
		}
		if err := repo.DeleteEntry(ctx, "t", typeID, doomed); err != nil {
			t.Fatal(err)
		}
		// Pinned deliberately, because it is a RULING and not a detail: the
		// history goes with the entry, which is defensible only while
		// content:delete stays outside the agent whitelist. If this assertion is
		// ever inverted, agent_gate.go's content:delete line has to be read
		// first — migration 000034 spells out why.
		if got := revisions(t, doomed); len(got) != 0 {
			t.Fatalf("want the revisions cascaded away with the entry, %d survived", len(got))
		}
		// The control: the other entry's history is untouched by a delete
		// elsewhere. Without it, a cascade that emptied the whole table passes.
		if len(revisions(t, entryID)) == 0 {
			t.Fatal("deleting one entry destroyed another's history")
		}
	})

	t.Run("RLS hides another tenant's revisions", func(t *testing.T) {
		// Seeded as superuser (which bypasses RLS), read back as a role that
		// does not — the only configuration in which the policies apply at all.
		otherType, otherEntry := uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'other','post','')`, otherType); err != nil {
			t.Fatalf("seed other type: %v", err)
		}
		if err := repo.CreateEntry(ctx, &domain.Entry{
			ID: otherEntry, TenantID: "other", ContentTypeID: otherType,
			Payload:            json.RawMessage(`{"title":"theirs"}`),
			Version:            1,
			Status:             domain.StatusDraft,
			Locale:             domain.DefaultLocale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &author,
			CreatedAt:          now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(ctx, `
			CREATE ROLE revapp LOGIN PASSWORD 'revpw' NOSUPERUSER;
			GRANT USAGE ON SCHEMA public TO revapp;
			GRANT SELECT, INSERT, UPDATE, DELETE ON content_types, entries, entry_revisions TO revapp;
		`); err != nil {
			t.Fatalf("create role: %v", err)
		}
		host, _ := container.Host(ctx)
		port, _ := container.MappedPort(ctx, "5432")
		app, err := pgxpool.New(ctx, "postgres://revapp:revpw@"+host+":"+port.Port()+"/revisions?sslmode=disable")
		if err != nil {
			t.Fatalf("connect as revapp: %v", err)
		}
		defer app.Close()

		countAs := func(t *testing.T, tenant string, id uuid.UUID) int {
			t.Helper()
			tx, err := app.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				t.Fatal(err)
			}
			var n int
			// No tenant_id predicate on purpose: the WHERE a forgetful caller
			// omits is precisely what the policy has to survive.
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM entry_revisions WHERE entry_id = $1`, id).Scan(&n); err != nil {
				t.Fatalf("count as revapp: %v", err)
			}
			return n
		}
		if n := countAs(t, "other", otherEntry); n == 0 {
			t.Fatal("the owning tenant cannot see its own revisions; the isolation assertion below would prove nothing")
		}
		if n := countAs(t, "t", otherEntry); n != 0 {
			t.Fatalf("tenant t read %d of another tenant's revisions", n)
		}
	})

	// william's 0806 retention ruling, landed as a read cap (revision_repository.go).
	//
	// THE NUMBERS BELOW ARE WRITTEN OUT RATHER THAN TAKEN FROM
	// entryRevisionListLimit, and that is the whole reason this subtest is worth
	// its runtime. Sourcing them from the constant would make the test agree with
	// whatever the constant says, so changing 50 to 5 — the exact edit the
	// constant's doc comment calls a ruling rather than a tuning — would stay
	// green. A ruling that no test can see being broken is not landed.
	t.Run("the read stops at 50 while every row stays stored", func(t *testing.T) {
		// Its own entry: 51 versions on the shared one would rewrite the history
		// the subtests above are asserting against.
		long := uuid.New()
		if err := repo.CreateEntry(ctx, &domain.Entry{
			ID: long, TenantID: "t", ContentTypeID: typeID,
			Payload:            json.RawMessage(`{"title":"n1"}`),
			Version:            1,
			Status:             domain.StatusDraft,
			Locale:             domain.DefaultLocale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &author,
			CreatedAt:          now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		// One past the cap, not far past it: 51 versions is the smallest history
		// that can tell "capped at 50" apart from "returns everything".
		human := domain.ActorKindHuman
		for v := 2; v <= 51; v++ {
			e := mustGetEntry(t, ctx, repo, typeID, long)
			e.Payload = json.RawMessage(fmt.Sprintf(`{"title":"n%d"}`, v))
			e.UpdatedAt = now.Add(time.Duration(v) * time.Minute)
			e.UpdatedBy = &editor
			e.UpdatedByKind = &human
			if err := repo.UpdateEntry(ctx, e); err != nil {
				t.Fatalf("update to version %d: %v", v, err)
			}
		}

		revs := revisions(t, long)
		if len(revs) != 50 {
			t.Fatalf("want the read capped at 50 revisions, got %d", len(revs))
		}
		// Newest first still, and it is the newest 50 that survive the cap rather
		// than whichever 50 the storage engine hands back first.
		if got := titleOf(t, revs[0].Payload); got != "n51" {
			t.Fatalf("newest returned revision is %q, want %q", got, "n51")
		}
		if revs[len(revs)-1].Version != 2 {
			t.Fatalf("oldest returned revision is version %d, want 2 — version 1 is the one past the cap",
				revs[len(revs)-1].Version)
		}

		// THE ASSERTION THAT TELLS A READ CAP FROM A PURGE, and without it this
		// whole subtest passes just as happily against a retention job that
		// deleted version 1. Read through the pool rather than the repository on
		// purpose: the method under test is the thing doing the hiding, so asking
		// it whether the row exists would be asking the cap about itself.
		var stored int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM entry_revisions WHERE entry_id = $1`, long).Scan(&stored); err != nil {
			t.Fatalf("count stored revisions: %v", err)
		}
		if stored != 51 {
			t.Fatalf("the table holds %d revisions, want all 51 — the cap is on the read, not on what is kept", stored)
		}
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT payload FROM entry_revisions WHERE entry_id = $1 AND version = 1`, long).Scan(&raw); err != nil {
			t.Fatalf("version 1 is unreadable past the cap: %v", err)
		}
		if got := titleOf(t, raw); got != "n1" {
			t.Fatalf("the revision past the cap holds %q, want the original %q", got, "n1")
		}
	})
}

func requireAuthor(t *testing.T, rev domain.EntryRevision, kind string, user *uuid.UUID, agent *string) {
	t.Helper()
	if rev.AuthorKind == nil {
		t.Fatalf("version %d has no author kind, want %q", rev.Version, kind)
	}
	if *rev.AuthorKind != kind {
		t.Fatalf("version %d author kind is %q, want %q", rev.Version, *rev.AuthorKind, kind)
	}
	requireUUID(t, "author_user_id", rev.AuthorUserID, *user)
	switch {
	case agent == nil && rev.AuthorAgentID != nil:
		t.Fatalf("version %d names agent %q, want none", rev.Version, *rev.AuthorAgentID)
	case agent != nil && rev.AuthorAgentID == nil:
		t.Fatalf("version %d names no agent, want %q", rev.Version, *agent)
	case agent != nil && *rev.AuthorAgentID != *agent:
		t.Fatalf("version %d names agent %q, want %q", rev.Version, *rev.AuthorAgentID, *agent)
	}
}
