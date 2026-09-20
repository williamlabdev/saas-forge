package repository

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Publish revisions against the real database (ADR-018).
//
// This has to be an integration test, and for a sharper reason than most of the
// files around it: the revision is written by an INSERT ... SELECT that reads
// `entries` BACK inside the publishing transaction, so every value it stores —
// the ordinal, the snapshot, the timestamp, the publisher — is chosen by the
// database from the row the UPDATE just produced. A Go fake reproducing that
// arithmetic would be asserting that the test's expectation equals the test's
// expectation. The three properties below are unreachable from the service
// package entirely: same-transaction atomicity, RLS, and the ON DELETE CASCADE.
//
// The service package's publish_revision_test.go says so in as many words at the
// head of its own "what a publish records" section.

func TestEntryPublishRevisions(t *testing.T) {
	ctx, pool, container := startContentDB(t, "pubrevs")
	repo := NewPostgresContentRepository(pool, nil)

	// Truncated for the reason revision_integration_test.go documents at length:
	// timestamptz stores microseconds, a Go instant carries nanoseconds, and the
	// difference is invisible on Darwin and red on a Linux runner.
	now := time.Now().UTC().Truncate(time.Microsecond)

	typeID := uuid.New()
	require.NoError(t, execOne(ctx, pool,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID))

	editor := uuid.New()

	// newEntry seeds a draft. Each subtest gets its own so a retention loop
	// cannot contaminate a cascade assertion.
	newEntry := func(t *testing.T, tenant string, ctID uuid.UUID, title string) *domain.Entry {
		t.Helper()
		e := &domain.Entry{
			ID: uuid.New(), TenantID: tenant, ContentTypeID: ctID,
			Payload:            json.RawMessage(`{"title":"` + title + `"}`),
			Version:            1,
			Status:             domain.StatusDraft,
			Locale:             domain.DefaultLocale,
			TranslationGroupID: uuid.New(),
			CreatedBy:          &editor,
			CreatedAt:          now, UpdatedAt: now,
		}
		require.NoError(t, repo.CreateEntry(ctx, e))
		return e
	}

	publish := func(t *testing.T, e *domain.Entry, at time.Time, opts ...domain.PublishOrigin) {
		t.Helper()
		e.UpdatedAt = at
		e.UpdatedBy = &editor
		e.PublishedBy = &editor
		published := at
		require.NoError(t, repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &published, opts...))
	}

	// rawCount goes around the repository entirely. It is the only way to tell
	// "hidden by a read cap" apart from "destroyed by a purge", which is the
	// exact distinction ADR-018 departs from 000034 on.
	rawCount := func(t *testing.T, entryID uuid.UUID) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM entry_publish_revisions WHERE entry_id = $1`, entryID).Scan(&n))
		return n
	}

	t.Run("a publish stores the snapshot the database just produced", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "february")
		publish(t, e, now.Add(time.Minute))

		revs, err := repo.ListEntryPublishRevisions(ctx, "t", e.ID)
		require.NoError(t, err)
		require.Len(t, revs, 1)
		r := revs[0]

		assert.Equal(t, 1, r.RevisionNo, "the ordinal starts at 1")
		// published_version, not version: the two differ, and the column exists to
		// join back to the schedule pin and to 000034's history.
		assert.Equal(t, e.PublishedVersion, r.Version)
		// NOT entries.published_at — that column means "first release since the
		// last unpublish" and would stamp every revision of a long-lived entry
		// with one instant. The release's own clock reading is updated_at.
		assert.True(t, r.PublishedAt.Equal(now.Add(time.Minute)),
			"revision published_at is %s, want the release's updated_at %s", r.PublishedAt, now.Add(time.Minute))
		require.NotNil(t, r.PublishedBy)
		assert.Equal(t, editor, *r.PublishedBy)
		assert.Equal(t, "", r.Via, "a hand-pressed publish must not claim a mechanism")
		assert.Nil(t, r.ViaScheduleID)
		assert.Empty(t, r.Payload, "the list projection must not carry the payload")

		// The payload is only on the detail read, and it is the SNAPSHOT rather
		// than the working copy.
		full, err := repo.GetEntryPublishRevision(ctx, "t", e.ID, 1)
		require.NoError(t, err)
		var doc struct {
			Title string `json:"title"`
		}
		require.NoError(t, json.Unmarshal(full.Payload, &doc))
		assert.Equal(t, "february", doc.Title)
	})

	t.Run("the ordinal ascends per entry and is not shared between entries", func(t *testing.T) {
		a := newEntry(t, "t", typeID, "a1")
		b := newEntry(t, "t", typeID, "b1")
		publish(t, a, now.Add(2*time.Minute))
		publish(t, b, now.Add(3*time.Minute))

		// Two different entries both start at 1. A global sequence would give the
		// second one a number nobody can count to.
		aRevs, err := repo.ListEntryPublishRevisions(ctx, "t", a.ID)
		require.NoError(t, err)
		bRevs, err := repo.ListEntryPublishRevisions(ctx, "t", b.ID)
		require.NoError(t, err)
		require.Len(t, aRevs, 1)
		require.Len(t, bRevs, 1)
		assert.Equal(t, 1, aRevs[0].RevisionNo)
		assert.Equal(t, 1, bRevs[0].RevisionNo)

		// Retract then re-publish: the retract adds nothing, the re-publish is 2.
		retractedAt := now.Add(4 * time.Minute)
		a.UpdatedAt = retractedAt
		require.NoError(t, repo.SetEntryPublishState(ctx, a, domain.StatusDraft, nil))
		assert.Equal(t, 1, rawCount(t, a.ID), "a retract wrote a release row")

		publish(t, a, now.Add(5*time.Minute))
		aRevs, err = repo.ListEntryPublishRevisions(ctx, "t", a.ID)
		require.NoError(t, err)
		require.Len(t, aRevs, 2)
		assert.Equal(t, 2, aRevs[0].RevisionNo, "the list must be newest-first")
		assert.Equal(t, 1, aRevs[1].RevisionNo)
	})

	t.Run("a scheduled release records which schedule fired it", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "scheduled")
		schedID := uuid.New()
		publish(t, e, now.Add(6*time.Minute),
			domain.PublishOrigin{Via: domain.ActivityViaSchedule, ScheduleID: &schedID})

		revs, err := repo.ListEntryPublishRevisions(ctx, "t", e.ID)
		require.NoError(t, err)
		require.Len(t, revs, 1)
		assert.Equal(t, domain.ActivityViaSchedule, revs[0].Via)
		require.NotNil(t, revs[0].ViaScheduleID)
		assert.Equal(t, schedID, *revs[0].ViaScheduleID)
	})

	t.Run("a malformed origin is refused before it reaches the table", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "bad origin")
		e.UpdatedAt = now.Add(7 * time.Minute)
		at := e.UpdatedAt
		// 'schedule' with nothing to point at is an audit hole; the CHECK
		// constraint would catch it, but the Go guard has to catch it first or
		// the error a caller sees is a raw constraint violation.
		err := repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &at,
			domain.PublishOrigin{Via: domain.ActivityViaSchedule})
		require.Error(t, err)
		assert.Equal(t, 0, rawCount(t, e.ID))

		// And the other direction: a schedule id on a publish nobody scheduled.
		id := uuid.New()
		err = repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &at,
			domain.PublishOrigin{ScheduleID: &id})
		require.Error(t, err)
		assert.Equal(t, 0, rawCount(t, e.ID))

		// Two callers disagreeing about why a publish happened is not something
		// to average.
		err = repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &at,
			domain.PublishOrigin{}, domain.PublishOrigin{})
		require.Error(t, err)
	})

	t.Run("retention destroys the oldest rather than hiding it", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "long lived")
		total := domain.MaxPublishRevisionsPerEntry + 3
		for i := 0; i < total; i++ {
			publish(t, e, now.Add(time.Duration(10+i)*time.Minute))
			// Back to draft so the next call is a real publish rather than a
			// re-publish of an already-live entry (both record, but alternating
			// keeps the fixture honest about what a release is).
			e.UpdatedAt = now.Add(time.Duration(10+i)*time.Minute + time.Second)
			require.NoError(t, repo.SetEntryPublishState(ctx, e, domain.StatusDraft, nil))
		}

		// The RAW count, not the list. A read LIMIT would satisfy every assertion
		// below except this one, and a read LIMIT is precisely what ADR-018 chose
		// NOT to do — the payloads have to actually leave the table.
		assert.Equal(t, domain.MaxPublishRevisionsPerEntry, rawCount(t, e.ID),
			"retention is hiding rows instead of purging them")

		revs, err := repo.ListEntryPublishRevisions(ctx, "t", e.ID)
		require.NoError(t, err)
		require.Len(t, revs, domain.MaxPublishRevisionsPerEntry)
		assert.Equal(t, total, revs[0].RevisionNo, "the newest release was purged")
		assert.Equal(t, total-domain.MaxPublishRevisionsPerEntry+1, revs[len(revs)-1].RevisionNo,
			"the window kept the wrong end")

		// The purged ordinal is a 404 and not an empty payload.
		_, err = repo.GetEntryPublishRevision(ctx, "t", e.ID, 1)
		assert.ErrorIs(t, err, apperrors.ErrNotFound)
	})

	t.Run("the release and its revision are one transaction", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "atomic")
		versionBefore := e.Version

		boom := errors.New("boom")
		err := repo.WithTx(ctx, "t", func(bound ContentRepository) error {
			inner := *e
			inner.UpdatedAt = now.Add(30 * time.Minute)
			inner.PublishedBy = &editor
			at := inner.UpdatedAt
			if err := bound.SetEntryPublishState(ctx, &inner, domain.StatusPublished, &at); err != nil {
				return err
			}
			// Visible INSIDE the transaction — otherwise the assertion after the
			// rollback would pass against a write that never happened at all.
			revs, err := bound.ListEntryPublishRevisions(ctx, "t", e.ID)
			if err != nil {
				return err
			}
			require.Len(t, revs, 1, "the revision was not written inside the publishing transaction")
			return boom
		})
		require.ErrorIs(t, err, boom)

		// BOTH halves are gone. A revision written on its own connection would
		// survive here and leave the table claiming a release the entry never had.
		assert.Equal(t, 0, rawCount(t, e.ID), "the revision outlived the rolled-back publish")
		stored := mustGetEntry(t, ctx, repo, typeID, e.ID)
		assert.Equal(t, versionBefore, stored.Version)
		assert.Equal(t, domain.StatusDraft, stored.Status)
	})

	t.Run("deleting the entry takes its releases with it", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "doomed")
		publish(t, e, now.Add(40*time.Minute))
		require.Equal(t, 1, rawCount(t, e.ID))

		require.NoError(t, repo.DeleteEntry(ctx, "t", typeID, e.ID))
		assert.Equal(t, 0, rawCount(t, e.ID),
			"the FK is missing its ON DELETE CASCADE and the delete would have failed or orphaned rows")
	})

	t.Run("RLS hides another tenant's releases from a forgotten WHERE", func(t *testing.T) {
		// A second tenant with its own type and entry, both published.
		otherType := uuid.New()
		require.NoError(t, execOne(ctx, pool,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'other','post','')`, otherType))
		mine := newEntry(t, "t", typeID, "mine")
		theirs := newEntry(t, "other", otherType, "theirs")
		publish(t, mine, now.Add(50*time.Minute))
		publish(t, theirs, now.Add(51*time.Minute))

		// The repository path first: asking for the other tenant's entry under my
		// tenant must find nothing, even though the row exists.
		revs, err := repo.ListEntryPublishRevisions(ctx, "t", theirs.ID)
		require.NoError(t, err)
		assert.Empty(t, revs)
		_, err = repo.GetEntryPublishRevision(ctx, "t", theirs.ID, 1)
		assert.ErrorIs(t, err, apperrors.ErrNotFound)

		// Then the policy itself, under a NON-SUPERUSER role and with NO tenant
		// predicate in the query. This is the half the repository cannot prove:
		// its own WHERE clause would produce the same answer with RLS disabled.
		require.NoError(t, execOne(ctx, pool, `
			CREATE ROLE revapp LOGIN PASSWORD 'revpw' NOSUPERUSER;
			GRANT USAGE ON SCHEMA public TO revapp;
			GRANT SELECT, INSERT, DELETE ON entry_publish_revisions TO revapp;
		`))
		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432")
		require.NoError(t, err)
		app, err := pgxpool.New(ctx, "postgres://revapp:revpw@"+host+":"+port.Port()+"/pubrevs?sslmode=disable")
		require.NoError(t, err)
		defer app.Close()

		countAll := func(t *testing.T, setTenant bool, tenant string) int {
			t.Helper()
			tx, err := app.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			if setTenant {
				_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
				require.NoError(t, err)
			}
			var n int
			// Deliberately NO tenant predicate: this is the forgotten filter.
			require.NoError(t, tx.QueryRow(ctx, `SELECT COUNT(*) FROM entry_publish_revisions`).Scan(&n))
			return n
		}

		assert.Zero(t, countAll(t, false, ""), "an unset tenant must fail closed")
		assert.Zero(t, countAll(t, true, ""), "an empty tenant must fail closed")
		assert.Equal(t, 1, countAll(t, true, "other"),
			"the other tenant must see exactly its own single release")

		// A write scoped to one tenant may not plant a row in another's.
		tx, err := app.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'other', true)`)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO entry_publish_revisions
				(id, tenant_id, entry_id, revision_no, payload, version, published_at)
			VALUES ($1, 't', $2, 99, '{}'::jsonb, 1, now())`, uuid.New(), mine.ID)
		assert.Error(t, err, "the INSERT policy let a row through into another tenant")
		_ = tx.Rollback(ctx)
	})

	t.Run("a published revision cannot be edited after the fact", func(t *testing.T) {
		e := newEntry(t, "t", typeID, "immutable")
		publish(t, e, now.Add(60*time.Minute))

		// There is no UPDATE policy, deliberately: a revision is the record of
		// what went out. Asserted under the non-superuser role, because the owner
		// would need FORCE alone to be stopped and FORCE is what this checks.
		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432")
		require.NoError(t, err)
		require.NoError(t, execOne(ctx, pool, `GRANT UPDATE ON entry_publish_revisions TO revapp`))
		app, err := pgxpool.New(ctx, "postgres://revapp:revpw@"+host+":"+port.Port()+"/pubrevs?sslmode=disable")
		require.NoError(t, err)
		defer app.Close()

		tx, err := app.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 't', true)`)
		require.NoError(t, err)
		tag, err := tx.Exec(ctx, `UPDATE entry_publish_revisions SET payload = '{"title":"rewritten"}'::jsonb`)
		if err == nil {
			assert.Zero(t, tag.RowsAffected(),
				"an UPDATE policy exists; a release record was rewritten after the fact")
		}
	})

	// Retention DELETEs rows, which makes DELETE the one write path on this table
	// where a forgotten predicate destroys another tenant's history rather than
	// merely exposing it. 000034 has no delete path at all, so the DELETE policy
	// is surface this feature invented and it gets its own proof. Depends on the
	// `revapp` role the RLS subtest above creates.
	t.Run("the DELETE policy stops a purge that forgot its tenant", func(t *testing.T) {
		purgeType := uuid.New()
		require.NoError(t, execOne(ctx, pool,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'other','purge','')`, purgeType))
		mine := newEntry(t, "t", typeID, "mine-purge")
		theirs := newEntry(t, "other", purgeType, "theirs-purge")
		publish(t, mine, now.Add(70*time.Minute))
		publish(t, theirs, now.Add(71*time.Minute))

		// Counted as the superuser, which RLS does not apply to — otherwise the
		// "survived" assertion would be reading the same filtered view it is
		// supposed to be checking.
		countTenant := func(t *testing.T, tenant string) int {
			t.Helper()
			var n int
			require.NoError(t, pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM entry_publish_revisions WHERE tenant_id = $1`, tenant).Scan(&n))
			return n
		}
		beforeMine := countTenant(t, "t")
		beforeTheirs := countTenant(t, "other")
		require.NotZero(t, beforeMine, "nothing of mine to delete — the assertion below would pass vacuously")
		require.NotZero(t, beforeTheirs, "nothing of theirs to protect — the assertion below would pass vacuously")

		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432")
		require.NoError(t, err)
		app, err := pgxpool.New(ctx, "postgres://revapp:revpw@"+host+":"+port.Port()+"/pubrevs?sslmode=disable")
		require.NoError(t, err)
		defer app.Close()

		tx, err := app.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 't', true)`)
		require.NoError(t, err)
		// Deliberately unqualified: this is the purge whose WHERE someone dropped.
		tag, err := tx.Exec(ctx, `DELETE FROM entry_publish_revisions`)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		assert.EqualValues(t, beforeMine, tag.RowsAffected(),
			"the unqualified DELETE reached %d rows but the caller's tenant only had %d",
			tag.RowsAffected(), beforeMine)
		assert.Zero(t, countTenant(t, "t"), "the caller's own rows should have gone")
		assert.Equal(t, beforeTheirs, countTenant(t, "other"),
			"another tenant's release history was destroyed by a purge that forgot its WHERE")
	})

	// Runs LAST: it drops the table and rebuilds it, so every row written above
	// is gone afterwards. The generic rollback test in rls_integration_test.go
	// proves each down migration REMOVES what it claims; it never runs an up
	// again, so "the table can be rebuilt on a database that has already had it"
	// is unproven there — and that is the shape of a real rollback, which is
	// always followed by a re-deploy.
	t.Run("000042 survives up down up, and publishing still records", func(t *testing.T) {
		read := func(t *testing.T, name string) string {
			t.Helper()
			b, err := os.ReadFile(filepath.Join(contentMigrationDir(), name))
			require.NoError(t, err)
			return string(b)
		}

		require.NoError(t, execOne(ctx, pool, read(t, "000042_entry_publish_revisions.down.sql")))
		var present bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT to_regclass('public.entry_publish_revisions') IS NOT NULL`).Scan(&present))
		require.False(t, present, "the down migration reported success and left the table behind")

		// The re-up is the half that actually fails in practice: a policy, index
		// or constraint the down forgot to take with the table survives and the
		// CREATE trips over its own leftovers.
		require.NoError(t, execOne(ctx, pool, read(t, "000042_entry_publish_revisions.up.sql")),
			"the table could not be rebuilt after its own rollback")

		// And the rebuilt table is functional, not merely present: the whole
		// write path — INSERT ... SELECT, the ordinal, the RLS policies — has to
		// still work, which a catalog check cannot tell you.
		e := newEntry(t, "t", typeID, "after-rollback")
		publish(t, e, now.Add(80*time.Minute))
		revs, err := repo.ListEntryPublishRevisions(ctx, "t", e.ID)
		require.NoError(t, err)
		require.Len(t, revs, 1)
		assert.Equal(t, 1, revs[0].RevisionNo,
			"the ordinal must start over at 1 on a table that was rebuilt empty")
	})
}

// execOne is the two-line boilerplate around pool.Exec that every seed in this
// file would otherwise repeat.
func execOne(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}
