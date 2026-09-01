package repository

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// The activity record against the real database (ADR-014 §3, migration 000032).
//
// Everything here is something the service-level fake structurally cannot
// answer: a CHECK constraint has no Go analogue, and the two halves of the
// round trip build their own statement — a column written by the INSERT and
// forgotten in the SELECT comes back as a zero value that reads like data.
func TestContentActivity(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("activity"),
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
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, loadContentRLSMigrations(t))
	require.NoError(t, err, "migrate")

	repo := NewPostgresContentRepository(pool, nil)
	principal := uuid.New()
	agentID := "content-bot"
	entryID := uuid.New()

	base := func() *domain.Activity {
		return &domain.Activity{
			ID:         uuid.New(),
			TenantID:   "t1",
			OccurredAt: time.Now().UTC(),
			ActorKind:  domain.ActorKindHuman,
			Action:     domain.ActivityEntryUpdate,
			TargetType: "post",
			Outcome:    domain.ActivityOutcomeSuccess,
		}
	}

	t.Run("round trip carries every column", func(t *testing.T) {
		row := base()
		row.ActorKind = domain.ActorKindAgent
		row.ActorUserID = &principal
		row.ActorAgentID = &agentID
		row.TargetEntryID = &entryID
		row.TargetTitle = "Spring launch"
		row.ChangedKeys = []string{"body", "title"}
		require.NoError(t, repo.RecordActivity(ctx, row))

		got, err := repo.ListActivity(ctx, ActivityFilter{TenantID: "t1", EntryID: &entryID})
		require.NoError(t, err)
		require.Len(t, got, 1)
		g := got[0]
		// Field by field rather than a struct compare, because a struct compare
		// would pass on a SELECT that dropped a column IF the INSERT had also
		// dropped it — which is the exact drift a shared column list exists to
		// prevent and this test exists to catch.
		assert.Equal(t, row.ID, g.ID)
		assert.Equal(t, domain.ActorKindAgent, g.ActorKind)
		require.NotNil(t, g.ActorUserID)
		assert.Equal(t, principal, *g.ActorUserID)
		require.NotNil(t, g.ActorAgentID)
		assert.Equal(t, agentID, *g.ActorAgentID)
		assert.Equal(t, domain.ActivityEntryUpdate, g.Action)
		assert.Equal(t, "post", g.TargetType)
		require.NotNil(t, g.TargetEntryID)
		assert.Equal(t, entryID, *g.TargetEntryID)
		assert.Equal(t, "Spring launch", g.TargetTitle)
		assert.Equal(t, domain.ActivityOutcomeSuccess, g.Outcome)
		assert.Empty(t, g.ErrorCode)
		assert.Equal(t, []string{"body", "title"}, g.ChangedKeys)
		assert.WithinDuration(t, row.OccurredAt, g.OccurredAt, time.Second)
	})

	t.Run("a nil key list stores as empty, not NULL", func(t *testing.T) {
		row := base()
		row.ChangedKeys = nil
		require.NoError(t, repo.RecordActivity(ctx, row),
			"a nil slice reaching a NOT NULL array column would violate the constraint")
	})

	// --- the CHECK constraints, one refusal each ------------------------------
	//
	// These are what the fake cannot mirror. Each writes a row the service
	// should never build and requires the database to be the last word on it.

	refused := []struct {
		name string
		mut  func(*domain.Activity)
	}{
		{
			// The ratchet from 000030/000031: a bot acting with nobody
			// accountable is the shape ADR-013 §2 keeps out.
			name: "an agent line with no principal",
			mut: func(a *domain.Activity) {
				a.ActorKind = domain.ActorKindAgent
				a.ActorAgentID = &agentID
				a.ActorUserID = nil
			},
		},
		{
			name: "an agent id beside a human kind",
			mut: func(a *domain.Activity) {
				a.ActorKind = domain.ActorKindHuman
				a.ActorAgentID = &agentID
			},
		},
		{
			name: "an agent kind with no agent id",
			mut: func(a *domain.Activity) {
				a.ActorKind = domain.ActorKindAgent
				a.ActorUserID = &principal
				a.ActorAgentID = nil
			},
		},
		{
			name: "a fourth actor kind",
			mut:  func(a *domain.Activity) { a.ActorKind = "robot" },
		},
		{
			// A denied line that does not say what refused it is the one thing a
			// reader of a refusal needs and cannot get anywhere else.
			name: "a refusal with no error code",
			mut:  func(a *domain.Activity) { a.Outcome = domain.ActivityOutcomeDenied },
		},
		{
			name: "a success carrying an error code",
			mut:  func(a *domain.Activity) { a.ErrorCode = "SOMETHING_BROKE" },
		},
		{
			// A refusal changed nothing; claiming otherwise reports an effect
			// that did not happen.
			name: "a refusal claiming changed keys",
			mut: func(a *domain.Activity) {
				a.Outcome = domain.ActivityOutcomeDenied
				a.ErrorCode = "FORBIDDEN"
				a.ChangedKeys = []string{"title"}
			},
		},
		{
			name: "a title with no entry to hang it on",
			mut: func(a *domain.Activity) {
				a.TargetEntryID = nil
				a.TargetTitle = "orphan"
			},
		},
		{
			name: "an unnamed action",
			mut:  func(a *domain.Activity) { a.Action = "" },
		},
		{
			name: "a third outcome",
			mut:  func(a *domain.Activity) { a.Outcome = "maybe" },
		},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			row := base()
			tc.mut(row)
			require.Error(t, repo.RecordActivity(ctx, row))
		})
	}

	// The control for the whole block above: a well-formed agent row is
	// accepted. Without it, a table that refused every INSERT would pass all ten
	// refusals and mean nothing.
	t.Run("accepts a well-formed agent row", func(t *testing.T) {
		row := base()
		row.ActorKind = domain.ActorKindAgent
		row.ActorUserID = &principal
		row.ActorAgentID = &agentID
		require.NoError(t, repo.RecordActivity(ctx, row))
	})

	t.Run("accepts a well-formed service row", func(t *testing.T) {
		row := base()
		row.ActorKind = domain.ActorKindService
		row.Outcome = domain.ActivityOutcomeDenied
		row.ErrorCode = "FORBIDDEN"
		require.NoError(t, repo.RecordActivity(ctx, row),
			"the delivery credential's refused write is the third kind's only emitter")
	})

	// --- reads -----------------------------------------------------------------

	t.Run("another tenant sees none of it", func(t *testing.T) {
		got, err := repo.ListActivity(ctx, ActivityFilter{TenantID: "t2"})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("newest first, and the limit is honoured", func(t *testing.T) {
		tenant := "ordered"
		start := time.Now().UTC().Add(-time.Hour)
		for i := range 5 {
			row := base()
			row.TenantID = tenant
			row.OccurredAt = start.Add(time.Duration(i) * time.Minute)
			require.NoError(t, repo.RecordActivity(ctx, row))
		}
		got, err := repo.ListActivity(ctx, ActivityFilter{TenantID: tenant, Limit: 2})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.True(t, got[0].OccurredAt.After(got[1].OccurredAt), "newest first")
		assert.WithinDuration(t, start.Add(4*time.Minute), got[0].OccurredAt, time.Second)
	})

	t.Run("Since excludes the boundary row", func(t *testing.T) {
		tenant := "sinced"
		cut := time.Now().UTC().Truncate(time.Millisecond)
		older, newer := base(), base()
		older.TenantID, older.OccurredAt = tenant, cut.Add(-time.Minute)
		newer.TenantID, newer.OccurredAt = tenant, cut.Add(time.Minute)
		atCut := base()
		atCut.TenantID, atCut.OccurredAt = tenant, cut
		for _, r := range []*domain.Activity{older, atCut, newer} {
			require.NoError(t, repo.RecordActivity(ctx, r))
		}
		got, err := repo.ListActivity(ctx, ActivityFilter{TenantID: tenant, Since: &cut})
		require.NoError(t, err)
		require.Len(t, got, 1, "Since is exclusive — §1's attribution window starts AFTER the last release, "+
			"and including the boundary would attribute the previous release's own row into this one's diff")
		assert.Equal(t, newer.ID, got[0].ID)
	})

	t.Run("the limit is capped", func(t *testing.T) {
		got, err := repo.ListActivity(ctx, ActivityFilter{TenantID: "ordered", Limit: 100000})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(got), activityLimitMax)
	})

	// --- per-field authors (ADR-014 §6, step 4) --------------------------------

	// authorsFor is a map view of the result, which every assertion below wants.
	// The ABSENCE of a key is the interesting half — that is what the console
	// renders as unknown — so this deliberately does not default it to anything.
	authorsFor := func(t *testing.T, tenant string, entry uuid.UUID, since *time.Time) map[string]*domain.FieldAuthor {
		t.Helper()
		got, err := repo.EntryFieldAuthors(ctx, tenant, entry, since)
		require.NoError(t, err)
		out := map[string]*domain.FieldAuthor{}
		for _, fa := range got {
			out[fa.Key] = fa
		}
		return out
	}

	t.Run("the newest write to a key wins, and an untouched key is absent", func(t *testing.T) {
		tenant, entry := "authors", uuid.New()
		start := time.Now().UTC().Add(-time.Hour)

		first := base()
		first.TenantID, first.TargetEntryID, first.OccurredAt = tenant, &entry, start
		first.ChangedKeys = []string{"title", "body"}
		require.NoError(t, repo.RecordActivity(ctx, first))

		second := base()
		second.TenantID, second.TargetEntryID, second.OccurredAt = tenant, &entry, start.Add(time.Minute)
		second.ActorKind, second.ActorUserID, second.ActorAgentID = domain.ActorKindAgent, &principal, &agentID
		second.ChangedKeys = []string{"title"}
		require.NoError(t, repo.RecordActivity(ctx, second))

		got := authorsFor(t, tenant, entry, nil)
		require.Contains(t, got, "title")
		assert.Equal(t, domain.ActorKindAgent, got["title"].ActorKind,
			"the LAST writer of a field is the one who has to answer for it in the diff")
		require.NotNil(t, got["title"].ActorAgentID)
		assert.Equal(t, agentID, *got["title"].ActorAgentID)
		require.NotNil(t, got["title"].ActorUserID, "an agent line names the principal who answers for it")

		require.Contains(t, got, "body")
		assert.Equal(t, domain.ActorKindHuman, got["body"].ActorKind,
			"the later row did not name body, so it did not take it over")

		assert.NotContains(t, got, "summary",
			"a key nobody wrote has NO author — absence is what renders as unknown, "+
				"and inventing a row here is what would let a caller fall back to updated_by")
	})

	t.Run("since excludes the boundary row", func(t *testing.T) {
		tenant, entry := "authors-since", uuid.New()
		cut := time.Now().UTC().Truncate(time.Millisecond)

		before := base()
		before.TenantID, before.TargetEntryID, before.OccurredAt = tenant, &entry, cut.Add(-time.Minute)
		before.ChangedKeys = []string{"released"}
		require.NoError(t, repo.RecordActivity(ctx, before))

		atCut := base()
		atCut.TenantID, atCut.TargetEntryID, atCut.OccurredAt = tenant, &entry, cut
		atCut.ChangedKeys = []string{"at-the-boundary"}
		require.NoError(t, repo.RecordActivity(ctx, atCut))

		after := base()
		after.TenantID, after.TargetEntryID, after.OccurredAt = tenant, &entry, cut.Add(time.Minute)
		after.ChangedKeys = []string{"pending"}
		require.NoError(t, repo.RecordActivity(ctx, after))

		got := authorsFor(t, tenant, entry, &cut)
		assert.Contains(t, got, "pending")
		assert.NotContains(t, got, "released",
			"a change that was already published is not part of what publishing would change")
		assert.NotContains(t, got, "at-the-boundary",
			"the window is EXCLUSIVE: the release's own row belongs to the release before it")

		// Control: without the bound the same rows are all there, so the assertion
		// above is testing the bound and not an empty table.
		assert.Contains(t, authorsFor(t, tenant, entry, nil), "released")
	})

	t.Run("a key is attributed however long ago it was written", func(t *testing.T) {
		// The reason this fold is DISTINCT ON in SQL rather than a page of
		// activity folded in Go: a paged read attributes only what fits on the
		// page, and reports "unknown" for a field whose record it simply did not
		// reach. Here the rare key is older than activityLimitMax churn rows.
		tenant, entry := "authors-deep", uuid.New()
		start := time.Now().UTC().Add(-24 * time.Hour)

		rare := base()
		rare.TenantID, rare.TargetEntryID, rare.OccurredAt = tenant, &entry, start
		rare.ChangedKeys = []string{"rare"}
		require.NoError(t, repo.RecordActivity(ctx, rare))

		for i := range activityLimitMax + 50 {
			churn := base()
			churn.TenantID, churn.TargetEntryID = tenant, &entry
			churn.OccurredAt = start.Add(time.Duration(i+1) * time.Second)
			churn.ChangedKeys = []string{"churn"}
			require.NoError(t, repo.RecordActivity(ctx, churn))
		}

		got := authorsFor(t, tenant, entry, nil)
		assert.Contains(t, got, "rare",
			"%d newer rows must not hide the last write to a field the diff is showing", activityLimitMax+50)
		assert.Len(t, got, 2, "one row per KEY, not per activity line")
	})

	t.Run("neither another tenant nor another entry bleeds in", func(t *testing.T) {
		tenant, mine, theirs := "authors-isolated", uuid.New(), uuid.New()
		row := base()
		row.TenantID, row.TargetEntryID = tenant, &mine
		row.ChangedKeys = []string{"mine"}
		require.NoError(t, repo.RecordActivity(ctx, row))

		assert.Contains(t, authorsFor(t, tenant, mine, nil), "mine")
		assert.Empty(t, authorsFor(t, tenant, theirs, nil), "another entry of the same tenant")
		assert.Empty(t, authorsFor(t, "authors-other-tenant", mine, nil), "another tenant")
	})
}

// TestContentActivityAppendOnlyIsNotEnforcedHere states, rather than pretends,
// what migration 000032's missing UPDATE/DELETE policies do and do not buy.
//
// This container connects as a SUPERUSER, which bypasses RLS entirely — the same
// caveat migration 000014 records for every other content table. So the policies
// are present-but-dormant here and an UPDATE would succeed; asserting that it
// fails would be asserting a property this harness cannot produce, and asserting
// that it succeeds would read as approval of the write.
//
// What IS checkable here, and what actually carries the guarantee day to day, is
// that the repository offers no way to do it: append-only is a property of the
// code surface first and of the policies second.
func TestContentActivityAppendOnlyIsNotEnforcedHere(t *testing.T) {
	iface := reflect.TypeOf((*ContentRepository)(nil)).Elem()
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		if name == "RecordActivity" || name == "ListActivity" {
			continue
		}
		assert.NotContains(t, strings.ToLower(name), "activity",
			"%s: the activity record is append-only — adding an update or delete verb here "+
				"needs a ruling, not a method", name)
	}
}
