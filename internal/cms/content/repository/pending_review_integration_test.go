package repository

import (
	"context"
	"os"
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

// agentIDFor supplies the agent name that must accompany an agent kind, and
// nothing for any other writer — the pairing migration 000030 enforces.
func agentIDFor(kind any) any {
	if s, ok := kind.(string); ok && s == domain.ActorKindAgent {
		return "content-bot"
	}
	return nil
}

// The release queue against the real database (ADR-014 §2).
//
// This is the half memRepo structurally cannot answer. The fake REWRITES
// pendingReviewExpr in Go, so a service test proves only that the fake and the
// service agree; whether the SQL selects the same set is a separate claim, and
// two of its three moving parts have no Go analogue at all:
//
//   - `payload IS DISTINCT FROM published_payload` is jsonb SEMANTIC equality.
//     The same document written two ways is not a difference to Postgres, and a
//     Go byte comparison would call it one — putting a row in the queue that
//     nobody edited.
//   - the query spans content types, which is the one thing ListEntries cannot
//     do, so the WHERE has no content_type_id and only the database can say
//     whether tenant isolation still holds without it.
func TestPendingReviewSpansTypesAndStates(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("pending"),
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

	// TWO types, because a queue assembled per type would pass a single-type
	// fixture while still being unable to do the one thing this query exists for.
	postType, pageType := uuid.New(), uuid.New()
	for _, tc := range []struct {
		id   uuid.UUID
		name string
	}{{postType, "post"}, {pageType, "page"}} {
		_, err := pool.Exec(ctx,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t1',$2,'')`, tc.id, tc.name)
		require.NoError(t, err)
	}
	// A second tenant's waiting draft. If the WHERE ever loses its tenant clause
	// this row is what says so — and losing it is likelier here than in
	// ListEntries, which is additionally narrowed by content_type_id.
	otherType := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t2','post','')`, otherType)
	require.NoError(t, err)

	// insert writes one row in a chosen state. Payload and snapshot are set
	// independently so the jsonb comparison can be exercised directly, which is
	// the point of going to the database at all.
	// An agent write must also name the principal it answers for — migration
	// 000030's entries_updated_by_agent_has_principal_check, which rejected the
	// first version of this fixture. Setting the kind without the principal is
	// not a state the platform can be in, so a test that asserts against one is
	// asserting about nothing.
	insert := func(tenant string, typeID uuid.UUID, title, status string, snapshot any, kind any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var publishedAt, snapshotVersion, updatedBy any
		if status == domain.StatusPublished {
			publishedAt = time.Now().UTC()
		}
		if snapshot != nil {
			snapshotVersion = 1
		}
		if kind != nil {
			updatedBy = uuid.New()
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status,
			                     published_at, published_payload, published_version,
			                     updated_by, updated_by_kind, updated_by_agent)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			id, tenant, typeID, `{"title":"`+title+`"}`, status,
			publishedAt, snapshot, snapshotVersion, updatedBy, kind, agentIDFor(kind))
		require.NoError(t, err)
		return id
	}

	liveEdited := insert("t1", postType, "live-edited", domain.StatusPublished, `{"title":"the released text"}`, nil)
	liveClean := insert("t1", postType, "live-clean", domain.StatusPublished, `{"title":"live-clean"}`, nil)
	freshDraft := insert("t1", pageType, "fresh-draft", domain.StatusDraft, nil, nil)
	retracted := insert("t1", pageType, "retracted", domain.StatusDraft, `{"title":"retracted"}`, nil)
	otherTenant := insert("t2", otherType, "not yours", domain.StatusDraft, nil, nil)

	rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
	require.NoError(t, err)

	got := map[uuid.UUID]bool{}
	for _, e := range rows {
		got[e.ID] = true
	}

	// Written out, not derived from the fixture: an expected set computed by the
	// same rule the query uses would agree with any rule at all.
	assert.True(t, got[liveEdited], "published, and the working copy has moved away from the snapshot")
	assert.True(t, got[freshDraft], "never released — the half `status = published` drops")
	assert.True(t, got[retracted], "taken down, and whether it goes back up is a decision waiting for someone")
	assert.False(t, got[liveClean], "live and identical to the snapshot: waiting on nobody")
	assert.False(t, got[otherTenant], "another tenant's queue is not this tenant's business")
	assert.Len(t, rows, 3)

	// Both types are represented, stated on its own because a single-type result
	// of the right SIZE would satisfy the assertions above.
	t.Run("the queue spans content types", func(t *testing.T) {
		types := map[uuid.UUID]bool{}
		for _, e := range rows {
			types[e.ContentTypeID] = true
		}
		assert.True(t, types[postType])
		assert.True(t, types[pageType])
	})

	// jsonb semantics, which is the reason the difference test is done in SQL and
	// the reason a Go fake cannot stand in for this. Same document, different
	// text: not a difference, so not in the queue.
	t.Run("a payload rewritten but not changed is not pending", func(t *testing.T) {
		id := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status,
			                     published_at, published_payload, published_version)
			VALUES ($1,'t1',$2,$3,'published',now(),$4,1)`,
			id, postType, `{"n": 1.0, "title": "x"}`, `{"title":"x","n":1}`)
		require.NoError(t, err)

		rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
		require.NoError(t, err)
		for _, e := range rows {
			assert.NotEqual(t, id, e.ID,
				"jsonb calls these documents equal; a byte comparison would put an "+
					"unedited entry in front of a reviewer")
		}
	})

	t.Run("agent-touched work sorts first", func(t *testing.T) {
		// Inserted LAST and left as a draft, so insertion order and created_at
		// order would both put it behind the rows above.
		agentRow := insert("t1", postType, "agent draft", domain.StatusDraft, nil, domain.ActorKindAgent)

		rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		assert.Equal(t, agentRow, rows[0].ID,
			"the queue answers 'what needs me', and a person's own work needs them least")
	})

	t.Run("limit bounds the read", func(t *testing.T) {
		rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1", Limit: 2})
		require.NoError(t, err)
		assert.Len(t, rows, 2)
	})
}
