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

// ADR-021 against the real thing, for the two claims a fake cannot answer:
//
//   - pg_trgm/ILIKE actually finds a Chinese substring. Postgres's built-in
//     text search parser is the reason this feature exists at all (migration
//     000047's own header: `to_tsvector('simple', '我愛台灣')` is ONE token,
//     never matched by a search for `台灣`) — a Go fake that does `strings.
//     Contains` would pass whether or not the SQL got this right, since Go's
//     Contains has no tokenizer to get wrong in the first place.
//   - SearchEntries spans content types AND tenants correctly. memRepo has no
//     analogue for this query at all (SearchEntries, like ListPendingReview,
//     has no content_type_id in its WHERE), so whether the real WHERE keeps
//     tenant isolation while dropping type confinement is untestable in Go.
func TestSearchEntriesCrossTypeChineseAndMultiTermAnd(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("searchentries"),
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

	postType, pageType := uuid.New(), uuid.New()
	for _, tc := range []struct {
		id   uuid.UUID
		name string
	}{{postType, "post"}, {pageType, "page"}} {
		_, err := pool.Exec(ctx,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t1',$2,'')`, tc.id, tc.name)
		require.NoError(t, err)
	}
	otherType := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t2','post','')`, otherType)
	require.NoError(t, err)

	// insert writes one row with an explicit search_text and updated_at, so
	// ordering (updated_at DESC) can be asserted on purpose rather than by
	// accident of insertion order.
	insert := func(tenant string, typeID uuid.UUID, searchText string, updatedAt time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, search_text, updated_at)
			VALUES ($1,$2,$3,'{}'::jsonb,$4,$5)`,
			id, tenant, typeID, searchText, updatedAt)
		require.NoError(t, err)
		return id
	}

	base := time.Now().UTC().Truncate(time.Second)
	// Chinese substring, spanning both t1 content types — pg_trgm's actual
	// job. postA is older than pageB, so the ordering assertion below (newest
	// first) is meaningful rather than a coincidence of insertion order.
	postA := insert("t1", postType, "我愛台灣 hello world", base.Add(-2*time.Minute))
	pageB := insert("t1", pageType, "台灣的天氣很好", base.Add(-1*time.Minute))
	// No match: doesn't mention Taiwan at all.
	insert("t1", postType, "completely unrelated content", base)
	// Multi-term AND fixture: only postD has BOTH terms.
	postD := insert("t1", postType, "unicorn ranch open today", base.Add(1*time.Minute))
	// "unicorn" alone, deliberately NOT containing the substring "ranch"
	// anywhere (an earlier draft of this fixture said "no ranch here", which
	// itself contains "ranch" and made the AND assertion pass for the wrong
	// reason) — this row must fail the two-term query on the second term.
	insert("t1", postType, "unicorn only, nothing else nearby", base.Add(2*time.Minute))
	// Same Chinese text, but a different tenant — must never come back for t1.
	insert("t2", otherType, "我愛台灣", base)

	t.Run("chinese substring spans content types, newest first", func(t *testing.T) {
		rows, err := repo.SearchEntries(ctx, SearchEntriesFilter{TenantID: "t1", Query: "台灣"})
		require.NoError(t, err)
		require.Len(t, rows, 2, "expected exactly postA and pageB")
		// pageB is newer (base-1m) than postA (base-2m): updated_at DESC means
		// pageB comes first.
		assert.Equal(t, pageB, rows[0].ID)
		assert.Equal(t, postA, rows[1].ID)
	})

	t.Run("multi-term query is AND, not OR", func(t *testing.T) {
		rows, err := repo.SearchEntries(ctx, SearchEntriesFilter{TenantID: "t1", Query: "unicorn ranch"})
		require.NoError(t, err)
		require.Len(t, rows, 1, "only the row containing BOTH terms")
		assert.Equal(t, postD, rows[0].ID)
	})

	t.Run("tenant isolation", func(t *testing.T) {
		rows, err := repo.SearchEntries(ctx, SearchEntriesFilter{TenantID: "t2", Query: "台灣"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		for _, r := range rows {
			// No t1 row should ever surface under t2's tenant id.
			assert.NotEqual(t, postA, r.ID)
			assert.NotEqual(t, pageB, r.ID)
		}
	})

	t.Run("no match returns empty, not an error", func(t *testing.T) {
		rows, err := repo.SearchEntries(ctx, SearchEntriesFilter{TenantID: "t1", Query: "nonexistent-zzz"})
		require.NoError(t, err)
		assert.Empty(t, rows)
	})
}

// TestListEntriesQueryDraftVersusPublishedColumn is the per-type `q`
// counterpart (buildWhere's predicateSearchColumn), proving MatchPublished
// actually switches which of the two columns ILIKE runs against — the
// property ADR-021 relies on to keep an editor's in-progress draft out of the
// public delivery audience's search results. A CJK substring is used on both
// sides so this also exercises the same pg_trgm behaviour as the cross-type
// test above, on the single-type path instead.
func TestListEntriesQueryDraftVersusPublishedColumn(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("listquery"),
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

	typeID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t1','post','')`, typeID)
	require.NoError(t, err)

	// A published entry whose WORKING copy has moved on from what is actually
	// live — the same "edited but not republished" shape pending_review's own
	// fixture uses, here to prove the two search columns are genuinely
	// independent rather than one being a stale copy of the other.
	id := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO entries (id, tenant_id, content_type_id, payload, search_text,
		                     status, published_at, published_payload, published_search_text, published_version)
		VALUES ($1,'t1',$2,'{}'::jsonb,'秋季新品芒果乾','published',now(),'{}'::jsonb,'舊款芒果乾禮盒',1)`,
		id, typeID)
	require.NoError(t, err)

	find := func(query string, matchPublished bool) []*domain.Entry {
		t.Helper()
		status := ""
		if matchPublished {
			status = domain.StatusPublished
		}
		rows, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t1", ContentTypeID: typeID, Query: query,
			MatchPublished: matchPublished, Status: status,
		})
		require.NoError(t, err)
		return rows
	}

	t.Run("draft term matches the working copy", func(t *testing.T) {
		rows := find("新品", false)
		require.Len(t, rows, 1)
		assert.Equal(t, id, rows[0].ID)
	})

	t.Run("draft term does not leak into the published column", func(t *testing.T) {
		assert.Empty(t, find("新品", true), "published_search_text does not contain 新品 — MatchPublished must not fall back to the working copy")
	})

	t.Run("published term matches the snapshot", func(t *testing.T) {
		rows := find("禮盒", true)
		require.Len(t, rows, 1)
		assert.Equal(t, id, rows[0].ID)
	})

	t.Run("published term is absent from the working copy", func(t *testing.T) {
		assert.Empty(t, find("禮盒", false), "search_text does not contain 禮盒 — the draft-side query must not match the snapshot's text")
	})
}
