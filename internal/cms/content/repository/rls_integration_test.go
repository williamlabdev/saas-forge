package repository

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// This suite proves TKT-R6b: Postgres RLS blocks cross-tenant rows even for a
// query that "forgets" the tenant WHERE clause — but ONLY under a non-superuser
// role (superusers and, without FORCE, table owners bypass RLS). It therefore
// creates a dedicated non-superuser role and connects as it.

func dockerUp() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

// contentMigrationDir is the migrations directory of the package under test.
func contentMigrationDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "migrations")
}

func loadContentRLSMigrations(t *testing.T) string {
	t.Helper()
	dir := contentMigrationDir()
	// Derived, not hand-listed. This used to be a literal slice of filenames,
	// which meant a new migration was exercised here only if someone remembered
	// to add it — and forgetting produced a PASS, not a failure. Filenames carry
	// a zero-padded numeric prefix, so lexical order is migration order.
	names, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatalf("no migrations discovered in %s — glob pattern is wrong", dir)
	}
	sort.Strings(names)
	var sql string
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		sql += string(b) + "\n"
	}
	return sql
}

// schemaObjects reads the live catalog into a set of stable keys. It is the
// yardstick the rollback test measures each down migration against: a down
// migration is judged by what it actually removed from here, not by whether the
// file ran without error — an ALTER that matches nothing still "succeeds".
func schemaObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT 'table:' || table_name FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		UNION ALL
		SELECT 'column:' || table_name || '.' || column_name FROM information_schema.columns
		  WHERE table_schema = 'public'
		UNION ALL
		SELECT 'index:' || indexname FROM pg_indexes WHERE schemaname = 'public'
		UNION ALL
		SELECT 'constraint:' || rel.relname || '.' || con.conname
		  FROM pg_constraint con
		  JOIN pg_class rel ON rel.oid = con.conrelid
		  JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		  WHERE ns.nspname = 'public'
		UNION ALL
		SELECT 'policy:' || tablename || '.' || policyname FROM pg_policies WHERE schemaname = 'public'
		UNION ALL
		SELECT 'function:' || p.proname FROM pg_proc p
		  JOIN pg_namespace ns ON ns.oid = p.pronamespace
		  WHERE ns.nspname = 'public'`)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	defer rows.Close()
	objects := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan catalog row: %v", err)
		}
		objects[key] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	return objects
}

var (
	reSQLComment    = regexp.MustCompile(`--[^\n]*`)
	reAlterTable    = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	reDropColumn    = regexp.MustCompile(`(?is)DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	reDropConstrain = regexp.MustCompile(`(?is)DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	reDropTable     = regexp.MustCompile(`(?is)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	reDropIndex     = regexp.MustCompile(`(?is)^\s*DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	reDropPolicy    = regexp.MustCompile(`(?is)^\s*DROP\s+POLICY\s+(?:IF\s+EXISTS\s+)?(\w+)\s+ON\s+(\w+)`)
	reDropFunction  = regexp.MustCompile(`(?is)^\s*DROP\s+FUNCTION\s+(?:IF\s+EXISTS\s+)?(\w+)`)
)

// dropClaims turns a down migration into the set of catalog keys its own text
// says it removes. The rollback test then holds the file to its word.
//
// This is deliberately a parser over the corpus that exists rather than a full
// SQL grammar. A statement shape it does not recognise yields no claims, which
// the caller treats as a failure rather than a pass — so an unparsed form shows
// up as "extend this parser", never as silent coverage loss.
func dropClaims(sql string) []string {
	var claims []string
	for _, stmt := range strings.Split(reSQLComment.ReplaceAllString(sql, ""), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		switch {
		case reAlterTable.MatchString(stmt):
			table := reAlterTable.FindStringSubmatch(stmt)[1]
			for _, m := range reDropColumn.FindAllStringSubmatch(stmt, -1) {
				claims = append(claims, "column:"+table+"."+m[1])
			}
			for _, m := range reDropConstrain.FindAllStringSubmatch(stmt, -1) {
				claims = append(claims, "constraint:"+table+"."+m[1])
			}
		case reDropPolicy.MatchString(stmt):
			m := reDropPolicy.FindStringSubmatch(stmt)
			claims = append(claims, "policy:"+m[2]+"."+m[1])
		case reDropTable.MatchString(stmt):
			claims = append(claims, "table:"+reDropTable.FindStringSubmatch(stmt)[1])
		case reDropIndex.MatchString(stmt):
			claims = append(claims, "index:"+reDropIndex.FindStringSubmatch(stmt)[1])
		case reDropFunction.MatchString(stmt):
			claims = append(claims, "function:"+reDropFunction.FindStringSubmatch(stmt)[1])
		}
	}
	return claims
}

func TestRLS_BlocksCrossTenantForNonSuperuser(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("rls"),
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

	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	super, err := pgxpool.New(ctx, superDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer super.Close()

	// Schema + RLS policies, and seed both tenants (superuser bypasses RLS).
	if _, err := super.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	typeA, typeB := uuid.New(), uuid.New()
	entryA, entryB := uuid.New(), uuid.New()
	seed := func(typeID uuid.UUID, tenant string, entryID uuid.UUID) {
		if _, err := super.Exec(ctx, `INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,$2,$3,'')`,
			typeID, tenant, "t_"+tenant); err != nil {
			t.Fatalf("seed type: %v", err)
		}
		if _, err := super.Exec(ctx, `INSERT INTO entries (id, tenant_id, content_type_id, payload) VALUES ($1,$2,$3,'{}')`,
			entryID, tenant, typeID); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}
	seed(typeA, "tenant-a", entryA)
	seed(typeB, "tenant-b", entryB)
	// An orphaned empty-tenant row (R1/R2 history): must be invisible to every
	// scoped read, and must NOT leak via the residual-'' pooled connection.
	typeEmpty, entryEmpty := uuid.New(), uuid.New()
	seed(typeEmpty, "", entryEmpty)

	// A dedicated NON-superuser role — the only way RLS actually applies.
	if _, err := super.Exec(ctx, `
		CREATE ROLE rlsapp LOGIN PASSWORD 'rlspw' NOSUPERUSER;
		GRANT USAGE ON SCHEMA public TO rlsapp;
		GRANT SELECT, INSERT, UPDATE, DELETE ON content_types, entries, content_type_fields TO rlsapp;
	`); err != nil {
		t.Fatalf("create role: %v", err)
	}

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	appDSN := "postgres://rlsapp:rlspw@" + host + ":" + port.Port() + "/rls?sslmode=disable"
	app, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as rlsapp: %v", err)
	}
	defer app.Close()

	countEntries := func(t *testing.T, setTenant, tenant string) int {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if setTenant == "set" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		// NOTE: deliberately NO tenant WHERE clause — this is the "forgotten
		// filter" RLS must catch.
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM entries`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// GUC unset → fail-closed: policy compares against NULL, nothing visible.
	if n := countEntries(t, "unset", ""); n != 0 {
		t.Fatalf("unset tenant should see 0 entries, saw %d", n)
	}
	// GUC set to '' (the residual a committed SET LOCAL leaves, and the
	// empty-tenant orphan case) → NULLIF collapses it to NULL → still 0.
	if n := countEntries(t, "set", ""); n != 0 {
		t.Fatalf("empty tenant must be fail-closed (NULLIF), saw %d", n)
	}
	// Scoped to tenant-a → sees only tenant-a's single row, despite no WHERE.
	if n := countEntries(t, "set", "tenant-a"); n != 1 {
		t.Fatalf("tenant-a should see exactly its 1 entry, saw %d", n)
	}
	if n := countEntries(t, "set", "tenant-b"); n != 1 {
		t.Fatalf("tenant-b should see exactly its 1 entry, saw %d", n)
	}

	// Residual-connection check: run a scoped tx, commit, then query the SAME
	// pool with NO GUC set. The reused connection carries the residual '' —
	// which NULLIF must still fail-close to 0 (not leak the empty-tenant row).
	for i := 0; i < 5; i++ { // loop to make connection reuse likely
		func() {
			tx, err := app.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-a', true)`)
			_ = tx.QueryRow(ctx, `SELECT 1 FROM entries LIMIT 1`).Scan(new(int))
			_ = tx.Commit(ctx)
		}()
	}
	var residual int
	if err := app.QueryRow(ctx, `SELECT COUNT(*) FROM entries`).Scan(&residual); err != nil {
		t.Fatal(err)
	}
	if residual != 0 {
		t.Fatalf("post-commit reused connection must not leak rows, saw %d", residual)
	}

	// Cross-tenant read by explicit id is invisible even though the row exists.
	tx, _ := app.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-a', true)`)
	var got int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM entries WHERE id = $1`, entryB).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("tenant-a must not see tenant-b's entry by id, saw %d", got)
	}

	// WITH CHECK: writing a row for another tenant is rejected.
	_, err = tx.Exec(ctx, `INSERT INTO entries (id, tenant_id, content_type_id, payload) VALUES ($1,'tenant-b',$2,'{}')`,
		uuid.New(), typeB)
	if err == nil {
		t.Fatal("inserting a tenant-b row while scoped to tenant-a must violate the RLS WITH CHECK")
	}

	// The REAL repository (withTenant + RLS together) under the non-superuser
	// role: tenant-a's repo must not reach tenant-b's entry even though it
	// exists — proves the production code path, not just raw SQL.
	repo := NewPostgresContentRepository(app, nil)
	if _, err := repo.GetEntry(ctx, "tenant-a", typeB, entryB); err == nil {
		t.Fatal("repo.GetEntry for tenant-a must not return tenant-b's entry")
	}
	// And its own entry IS reachable through the repo.
	if _, err := repo.GetEntry(ctx, "tenant-a", typeA, entryA); err != nil {
		t.Fatalf("repo.GetEntry for tenant-a's own entry: %v", err)
	}
	// CountEntriesForTenant via the repo sees only tenant-a's row.
	if n, err := repo.CountEntriesForTenant(ctx, "tenant-a"); err != nil || n != 1 {
		t.Fatalf("repo.CountEntriesForTenant(tenant-a) = %d, %v; want 1", n, err)
	}
}

// TestEntryStatusConstraints proves migration 000016's CHECK constraints hold in
// a real Postgres. The service enforces the same rules, but the DB is the layer
// that must survive a future writer that forgets them (an import job, a manual
// fix-up, the public delivery path) — so it is worth proving directly.
func TestEntryStatusConstraints(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("status"),
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
	if _, err := pool.Exec(ctx, `INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}

	// insertSnapshot sets the 000020 snapshot columns independently of status —
	// that decoupling is exactly what entries_published_snapshot_check governs.
	insertSnapshot := func(status string, publishedAt, snapshot, snapshotVersion any) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_at, published_payload, published_version)
			VALUES ($1,'t',$2,'{}',$3,$4,$5,$6)`,
			uuid.New(), typeID, status, publishedAt, snapshot, snapshotVersion)
		return err
	}

	// insertPublishedBy adds the 000021 actor column to the same decoupling, for
	// the constraint 000033 re-tied to the snapshot rather than to the status.
	insertPublishedBy := func(status string, publishedAt, snapshot, snapshotVersion any, publishedBy uuid.UUID) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_at, published_payload, published_version, published_by)
			VALUES ($1,'t',$2,'{}',$3,$4,$5,$6,$7)`,
			uuid.New(), typeID, status, publishedAt, snapshot, snapshotVersion, publishedBy)
		return err
	}

	// insert keeps the snapshot consistent with the status, so the status and
	// timestamp cases below still test what they were written to test rather
	// than tripping over 000020's constraint first.
	insert := func(status string, publishedAt any) error {
		var snapshot, snapshotVersion any
		if status == "published" {
			snapshot, snapshotVersion = "{}", 1
		}
		return insertSnapshot(status, publishedAt, snapshot, snapshotVersion)
	}

	now := time.Now().UTC()

	t.Run("draft without timestamp is accepted", func(t *testing.T) {
		if err := insert("draft", nil); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("published with timestamp is accepted", func(t *testing.T) {
		if err := insert("published", now); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	// The value domain: anything outside AllowedStatuses() must be refused by
	// the DB, not merely by Go.
	t.Run("unknown status is refused", func(t *testing.T) {
		if err := insert("archived", nil); err == nil {
			t.Fatal("expected entries_status_check to reject status=archived")
		}
	})
	// The coupling: published must always carry a timestamp, and a draft must
	// never carry one — this is what stops a half-written publish from being
	// invisible to the delivery path's ordering/caching.
	t.Run("published without timestamp is refused", func(t *testing.T) {
		if err := insert("published", nil); err == nil {
			t.Fatal("expected entries_published_at_check to reject published with NULL published_at")
		}
	})
	t.Run("draft with timestamp is refused", func(t *testing.T) {
		if err := insert("draft", now); err == nil {
			t.Fatal("expected entries_published_at_check to reject draft with a published_at")
		}
	})
	// migration 000020, narrowed by 000033: a published entry must have something
	// to serve. Without this, a writer could mark a row published with nothing
	// (delivery renders an empty document).
	//
	// 000020 also enforced the converse — an unpublished row had to have NO
	// snapshot — and ADR-014 §5.1 revokes exactly that half, so the subtest that
	// pinned it is now the "retract keeps it" case further down. What replaces it
	// as the guard against a stale snapshot being served is not a constraint at
	// all: every delivery read path filters on status explicitly.
	t.Run("published without a snapshot is refused", func(t *testing.T) {
		if err := insertSnapshot("published", now, nil, nil); err == nil {
			t.Fatal("expected entries_published_snapshot_check to reject published with NULL published_payload")
		}
	})
	t.Run("published without a snapshot version is refused", func(t *testing.T) {
		if err := insertSnapshot("published", now, "{}", nil); err == nil {
			t.Fatal("expected entries_published_snapshot_check to reject published with NULL published_version")
		}
	})
	// The half 000033 revoked, asserted in the POSITIVE direction so it cannot
	// rot into silence: a draft carrying a snapshot is the state ADR-014 §5.1
	// exists to produce, and a constraint that quietly came back would make the
	// retract path fail at runtime instead of here.
	t.Run("draft carrying a snapshot is allowed", func(t *testing.T) {
		if err := insertSnapshot("draft", nil, "{}", 1); err != nil {
			t.Fatalf("a retracted entry must be able to hold its snapshot: %v", err)
		}
	})

	// 000033's replacement coupling. published_by describes the SNAPSHOT, so it
	// is now tied to the snapshot's existence rather than to the status — the
	// three columns must keep describing one copy (ADR-006).
	t.Run("published_by without a snapshot is refused", func(t *testing.T) {
		if err := insertPublishedBy("draft", nil, nil, nil, uuid.New()); err == nil {
			t.Fatal("expected entries_published_by_snapshot_check_v2 to reject a releaser for a snapshot that does not exist")
		}
	})
	t.Run("published_by on a retracted snapshot is allowed", func(t *testing.T) {
		if err := insertPublishedBy("draft", nil, "{}", 1, uuid.New()); err != nil {
			t.Fatalf("a retracted entry must keep naming who released it: %v", err)
		}
	})

	// has_unpublished_changes is computed in SQL (unpublishedChangesExpr), so its
	// exactness is a property of real Postgres and cannot be proven by a Go
	// mirror. The criterion used to be `version <> published_version` alone,
	// which reported "changed" whenever the counter moved — including on a save
	// that stored the value the row already held (ADR-006).
	t.Run("unpublished-changes flag compares content, not just the version counter", func(t *testing.T) {
		repo := NewPostgresContentRepository(pool, nil)
		id := uuid.New()
		// The counters differ; the two payload literals hold the same content
		// written differently, which jsonb normalises away. The flag must follow
		// the content, and the counter must not get a vote.
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, version, status, published_at, published_payload, published_version)
			VALUES ($1,'t',$2,'{"a": 1, "b": 2}',5,'published',NOW(),'{"b":2,"a":1}',4)`, id, typeID); err != nil {
			t.Fatalf("seed: %v", err)
		}
		e, err := repo.GetEntry(ctx, "t", typeID, id)
		if err != nil {
			t.Fatal(err)
		}
		if e.Version == e.PublishedVersion {
			t.Fatal("guard: the versions must differ, or this case proves nothing about the payload comparison")
		}
		if e.HasUnpublishedChanges {
			t.Fatal("payload equals the snapshot — nothing is pending, whatever the counters say")
		}

		// The phantom case end to end: saving the content that is already live.
		// The write bumps version, and the flag must still be false.
		e.Payload = json.RawMessage(`{"b":2,"a":1}`)
		e.UpdatedAt = time.Now().UTC()
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatalf("re-save identical content: %v", err)
		}
		if e.Version != 6 {
			t.Fatalf("version=%d — the write must bump it, or the phantom case is not reproduced", e.Version)
		}
		if e.HasUnpublishedChanges {
			t.Fatal("re-saving identical content is not an unpublished change")
		}

		// And a real edit still flags, from the same RETURNING clause.
		e.Payload = json.RawMessage(`{"a":9,"b":2}`)
		e.UpdatedAt = time.Now().UTC()
		if err := repo.UpdateEntry(ctx, e); err != nil {
			t.Fatalf("real edit: %v", err)
		}
		if !e.HasUnpublishedChanges {
			t.Fatal("a changed payload must be reported as pending")
		}
		// The read path must agree with what the write path returned.
		reread, err := repo.GetEntry(ctx, "t", typeID, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reread.HasUnpublishedChanges {
			t.Fatal("GetEntry disagrees with UpdateEntry about the same row")
		}

		// A draft has no snapshot to differ from; the flag is about released
		// content, and NULL published_version must not leak through as true.
		draftID := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, version, status)
			VALUES ($1,'t',$2,'{"a":1}',3,'draft')`, draftID, typeID); err != nil {
			t.Fatalf("seed draft: %v", err)
		}
		d, err := repo.GetEntry(ctx, "t", typeID, draftID)
		if err != nil {
			t.Fatal(err)
		}
		if d.HasUnpublishedChanges {
			t.Fatal("a draft is unpublished, not 'has unpublished changes'")
		}

		// ListEntries builds its column list by hand, so the expression sitting in
		// the select list is one more thing rows.Scan has to line up with; and it
		// is the path an editor's index page actually uses.
		items, _, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID: "t", ContentTypeID: typeID, Limit: 100,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var seenPending, seenDraft bool
		for _, it := range items {
			switch it.ID {
			case id:
				seenPending = true
				if !it.HasUnpublishedChanges {
					t.Fatal("ListEntries disagrees with GetEntry about the pending row")
				}
			case draftID:
				seenDraft = true
				if it.HasUnpublishedChanges {
					t.Fatal("ListEntries flagged a draft")
				}
			}
		}
		if !seenPending || !seenDraft {
			t.Fatalf("guard: both rows must come back (pending=%v draft=%v)", seenPending, seenDraft)
		}

		// Publishing is what clears it, and SetEntryPublishState returns the
		// recomputed flag from its own RETURNING — the one place where reading a
		// pre-UPDATE value would silently leave every fresh publish "pending".
		pending, err := repo.GetEntry(ctx, "t", typeID, id)
		if err != nil {
			t.Fatal(err)
		}
		if !pending.HasUnpublishedChanges {
			t.Fatal("guard: the row must still be pending before we publish it")
		}
		now := time.Now().UTC()
		pending.UpdatedAt = now
		if err := repo.SetEntryPublishState(ctx, pending, "published", &now); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if pending.HasUnpublishedChanges {
			t.Fatal("nothing is pending immediately after a publish")
		}
		if reread, err := repo.GetEntry(ctx, "t", typeID, id); err != nil {
			t.Fatal(err)
		} else if reread.HasUnpublishedChanges {
			t.Fatal("GetEntry disagrees with SetEntryPublishState right after a publish")
		}
	})

	// Backfill semantics: an insert that predates the column (no status given)
	// must land on draft — fail-closed, never world-readable by default.
	t.Run("default is draft", func(t *testing.T) {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload) VALUES ($1,'t',$2,'{}')`,
			id, typeID); err != nil {
			t.Fatalf("insert without status: %v", err)
		}
		var status string
		var publishedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT status, published_at FROM entries WHERE id=$1`, id).
			Scan(&status, &publishedAt); err != nil {
			t.Fatal(err)
		}
		if status != "draft" || publishedAt != nil {
			t.Fatalf("status=%q published_at=%v — pre-existing rows must default to draft", status, publishedAt)
		}
	})

	// The (tenant, translation_group_id, locale) unique index is what makes
	// "the English version of this article" unambiguous.
	t.Run("duplicate locale in a group is refused", func(t *testing.T) {
		group := uuid.New()
		ins := func(locale string) error {
			_, err := pool.Exec(ctx, `
				INSERT INTO entries (id, tenant_id, content_type_id, payload, locale, translation_group_id)
				VALUES ($1,'t',$2,'{}',$3,$4)`, uuid.New(), typeID, locale, group)
			return err
		}
		if err := ins("en"); err != nil {
			t.Fatalf("first en: %v", err)
		}
		if err := ins("zh-TW"); err != nil {
			t.Fatalf("sibling locale must be allowed: %v", err)
		}
		if err := ins("en"); err == nil {
			t.Fatal("a second 'en' in the same group must violate idx_entries_group_locale")
		}
	})

	// Backfill semantics: an insert that predates localisation still lands in a
	// valid state — its own group, default locale.
	t.Run("insert without locale defaults to its own group", func(t *testing.T) {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload) VALUES ($1,'t',$2,'{}')`,
			id, typeID); err != nil {
			t.Fatalf("legacy-shaped insert: %v", err)
		}
		var locale string
		var group uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT locale, translation_group_id FROM entries WHERE id=$1`, id).
			Scan(&locale, &group); err != nil {
			t.Fatal(err)
		}
		if locale != "default" || group == uuid.Nil {
			t.Fatalf("locale=%q group=%v — must default to its own group", locale, group)
		}
	})

	t.Run("invalid locale tag is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, locale)
			VALUES ($1,'t',$2,'{}','zh TW')`, uuid.New(), typeID)
		if err == nil {
			t.Fatal("entries_locale_check must reject a locale with whitespace")
		}
	})

	// AssetIsPublished is the gate the whole media delivery path rests on. It is
	// a JOIN, so it is worth proving against real SQL rather than only a mirror.
	t.Run("asset is public only while a published entry references it", func(t *testing.T) {
		repo := NewPostgresContentRepository(pool, nil)
		assetID, entryID := uuid.New(), uuid.New()

		if _, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, uploaded_at)
			VALUES ($1,'t',$2, NOW())`, assetID, "t/"+assetID.String()); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status)
			VALUES ($1,'t',$2,'{}','draft')`, entryID, typeID); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
		// The WORKING copy references the asset. Under ADR-006 that alone must
		// never open the bytes: entry_media is the draft's reference set.
		if _, err := pool.Exec(ctx, `
			INSERT INTO entry_media (entry_id, asset_id, tenant_id) VALUES ($1,$2,'t')`,
			entryID, assetID); err != nil {
			t.Fatalf("seed link: %v", err)
		}

		ok, err := repo.AssetIsPublished(ctx, "t", assetID)
		if err != nil || ok {
			t.Fatalf("draft reference must not make the asset public (ok=%v err=%v)", ok, err)
		}

		// Publishing means writing the snapshot — the CHECK will not let a row
		// claim published without one, which is why this is not a bare status
		// flip any more.
		if _, err := pool.Exec(ctx, `
			UPDATE entries SET status='published', published_at=NOW(),
			       published_payload = payload, published_version = version
			WHERE id=$1`, entryID); err != nil {
			t.Fatal(err)
		}
		// Still not public: the entry is published, but the SNAPSHOT's reference
		// set is empty. A gate reading entry_media would wrongly open here.
		ok, err = repo.AssetIsPublished(ctx, "t", assetID)
		if err != nil || ok {
			t.Fatalf("a published entry whose snapshot does not reference the asset must not open it (ok=%v err=%v)", ok, err)
		}

		if _, err := pool.Exec(ctx, `
			INSERT INTO entry_media_published (entry_id, asset_id, tenant_id) VALUES ($1,$2,'t')`,
			entryID, assetID); err != nil {
			t.Fatalf("seed published link: %v", err)
		}
		ok, err = repo.AssetIsPublished(ctx, "t", assetID)
		if err != nil || !ok {
			t.Fatalf("published reference must make the asset public (ok=%v err=%v)", ok, err)
		}

		// Another tenant must not see it as public, even naming the right id.
		ok, err = repo.AssetIsPublished(ctx, "other", assetID)
		if err != nil || ok {
			t.Fatalf("the gate must be tenant-scoped (ok=%v err=%v)", ok, err)
		}

		// Deleting the entry cascades the link away, withdrawing access.
		if _, err := pool.Exec(ctx, `DELETE FROM entries WHERE id=$1`, entryID); err != nil {
			t.Fatal(err)
		}
		ok, err = repo.AssetIsPublished(ctx, "t", assetID)
		if err != nil || ok {
			t.Fatalf("removing the last reference must withdraw access (ok=%v err=%v)", ok, err)
		}
	})

	// The schema guards read both copies (bothCopies), and ADR-014 §5.1 changes
	// what "the other copy" can be. While a retract nulled the snapshot, only a
	// LIVE snapshot could ever match that half; now a retracted one can, and
	// without the status qualifier the guards would quietly start refusing schema
	// changes on behalf of content that is offline and has been for months.
	//
	// §5.1 says the opposite is intended: a retained snapshot that no longer
	// validates is a restore-time problem, handled by pruneUndefined and
	// validateAndNormalize on that path. Asserted against CountEntriesWithField
	// because it is the guard with the simplest predicate — the qualifier is
	// shared, so one guard proves the rendering.
	t.Run("schema guards ignore a retracted entry's snapshot", func(t *testing.T) {
		repo := NewPostgresContentRepository(pool, nil)
		guardTypeID := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','guarded','')`,
			guardTypeID); err != nil {
			t.Fatalf("seed type: %v", err)
		}
		// The key exists ONLY in the snapshot. That is the whole case: if the
		// working copy carried it too, the payload half would match and the
		// qualifier being tested would make no difference to the count.
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_payload, published_version)
			VALUES ($1,'t',$2,'{}','draft','{"legacy":1}',1)`, uuid.New(), guardTypeID); err != nil {
			t.Fatalf("seed retracted entry: %v", err)
		}

		n, err := repo.CountEntriesWithField(ctx, "t", guardTypeID, "legacy")
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("a retracted snapshot must not block a schema change, got %d entries counted", n)
		}

		// The control: the same key in a LIVE snapshot still counts, or the
		// assertion above would be satisfied by a guard that had stopped reading
		// the snapshot at all — which is the bug 000020's bothCopies exists to
		// prevent (a value edited out of the working copy but still being served).
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_at, published_payload, published_version)
			VALUES ($1,'t',$2,'{}','published',NOW(),'{"legacy":1}',1)`, uuid.New(), guardTypeID); err != nil {
			t.Fatalf("seed live entry: %v", err)
		}
		n, err = repo.CountEntriesWithField(ctx, "t", guardTypeID, "legacy")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("a live snapshot must still be guarded, got %d", n)
		}
	})

	// ADR-014 §5.1, the half that is easy to miss: the snapshot's ASSET
	// references have to outlive a retract too, because they are part of the
	// snapshot. 000020 built this table so the delivery gate would never read the
	// working copy's references — an image dropped from a draft must not revoke
	// bytes the live snapshot still needs. A retained snapshot whose references
	// were cleared is that same failure reached from the other side: nothing
	// would keep those bytes alive, and the snapshot §5.1 preserved would come
	// back missing its images.
	//
	// Both directions are asserted, and they are what make this test worth
	// writing: retention alone could be had by never withdrawing access, which
	// would leak a retracted entry's media to the public. The gate stays shut
	// because it joins entries.status — presence in this table is not access.
	t.Run("a retract keeps the snapshot's asset references without serving them", func(t *testing.T) {
		repo := NewPostgresContentRepository(pool, nil)
		assetID, entryID := uuid.New(), uuid.New()

		if _, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, uploaded_at)
			VALUES ($1,'t',$2, NOW())`, assetID, "t/"+assetID.String()); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, version)
			VALUES ($1,'t',$2,'{}','draft',1)`, entryID, typeID); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO entry_media (entry_id, asset_id, tenant_id) VALUES ($1,$2,'t')`,
			entryID, assetID); err != nil {
			t.Fatalf("seed link: %v", err)
		}

		// Through the real write path, not a hand-written UPDATE: the retention
		// being tested lives in SetEntryPublishState's branching, and a test that
		// wrote the rows itself would pass with that branching deleted.
		e := &domain.Entry{
			TenantID: "t", ContentTypeID: typeID, ID: entryID,
			Payload: json.RawMessage(`{}`), Version: 1, UpdatedAt: time.Now().UTC(),
		}
		published := time.Now().UTC()
		if err := repo.SetEntryPublishState(ctx, e, domain.StatusPublished, &published); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if ok, err := repo.AssetIsPublished(ctx, "t", assetID); err != nil || !ok {
			t.Fatalf("publishing must open the asset (ok=%v err=%v)", ok, err)
		}

		if err := repo.SetEntryPublishState(ctx, e, domain.StatusDraft, nil); err != nil {
			t.Fatalf("retract: %v", err)
		}

		var refs int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM entry_media_published WHERE entry_id = $1`, entryID).Scan(&refs); err != nil {
			t.Fatal(err)
		}
		if refs != 1 {
			t.Fatalf("the retained snapshot must keep its asset references, got %d rows", refs)
		}
		if ok, err := repo.AssetIsPublished(ctx, "t", assetID); err != nil || ok {
			t.Fatalf("a retracted entry's media must not stay public (ok=%v err=%v)", ok, err)
		}
	})

	// Run LAST: it rolls the schema back and leaves nothing for a later subtest
	// to stand on. An unrunnable rollback is a silent failure — nobody finds out
	// until they need it at 3am.
	t.Run("every down migration reverts what it claims", func(t *testing.T) {
		// Derived, not hand-listed. This used to be a literal slice of filenames
		// while the up side was globbed, so a new migration was exercised here
		// only if someone remembered to add it — and forgetting produced a PASS,
		// not a failure. That is exactly how 000023's down stayed unexercised
		// until 000024 was written. Adding a migration now costs no edit here.
		names, err := filepath.Glob(filepath.Join(contentMigrationDir(), "*.down.sql"))
		if err != nil {
			t.Fatalf("glob down migrations: %v", err)
		}
		if len(names) == 0 {
			t.Fatal("no down migrations discovered — glob pattern is wrong")
		}
		// Newest first — a rollback runs in reverse, and 000020's constraint
		// references the columns 000016 drops. Zero-padded numeric prefixes make
		// reverse lexical order the right order.
		sort.Sort(sort.Reverse(sort.StringSlice(names)))

		var rowsBefore int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM entries`).Scan(&rowsBefore); err != nil {
			t.Fatal(err)
		}
		if rowsBefore == 0 {
			t.Fatal("no entries rows to protect — the data-survival check below would pass vacuously")
		}

		objects := schemaObjects(t, ctx, pool)
		ran := 0
		for _, path := range names {
			name := filepath.Base(path)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read down migration %s: %v", name, err)
			}
			claims := dropClaims(string(body))

			// Stop before the base tables go. Everything past this point has
			// already been exercised, and the data-survival check needs `entries`
			// to still exist — a full teardown would make every assertion here
			// pass for the emptiest of reasons.
			basePlan := false
			for _, claim := range claims {
				if claim == "table:entries" {
					basePlan = true
				}
			}
			if basePlan {
				t.Logf("stopping before %s — it drops the base tables this subtest still reads", name)
				break
			}
			if len(claims) == 0 {
				t.Fatalf("%s: no drop claims parsed. Either this down migration removes "+
					"nothing — a rollback that silently does not roll back — or it uses a "+
					"statement shape dropClaims does not recognise yet, in which case extend "+
					"the parser rather than let the file through unchecked", name)
			}

			// What this migration's UP dropped is what its down may RESTORE — the
			// one legal way a down adds to the catalog. A REPLACEMENT migration
			// (000028 swaps a CHECK for a wider one) must put the old object back,
			// or the state it hands to the next down never existed and THAT down
			// finds nothing to drop. Anything added beyond this set is still an
			// error: a down inventing objects is not reverting. This is also why a
			// replacement must rename (_v2): the catalog is tracked by NAME, so a
			// same-name swap is invisible in both directions and would read as
			// "ran and changed nothing".
			upBody, err := os.ReadFile(strings.TrimSuffix(path, ".down.sql") + ".up.sql")
			if err != nil {
				t.Fatalf("read up migration for %s: %v — every down must have its up", name, err)
			}
			restorable := map[string]bool{}
			for _, claim := range dropClaims(string(upBody)) {
				restorable[claim] = true
			}

			if _, err := pool.Exec(ctx, string(body)); err != nil {
				t.Fatalf("down migration %s failed: %v", name, err)
			}
			ran++
			after := schemaObjects(t, ctx, pool)

			// 1. It did what its own text says. Checked immediately, before an
			//    older down migration drops the table the evidence lives on.
			for _, claim := range claims {
				if after[claim] {
					t.Errorf("%s claims to drop %s, but it is still in the catalog", name, claim)
				}
			}
			// 2. It only removed or restored, and it changed SOMETHING. An ALTER
			//    that matches nothing still reports success — running clean is not
			//    reverting. Compared as sets, not lengths: a replacement's down
			//    removes one object and restores another, which a length check
			//    would misread as a no-op.
			changed := false
			for key := range after {
				if !objects[key] {
					if !restorable[key] {
						t.Errorf("%s added %s — a down migration may only remove, or restore what its own up dropped", name, key)
					}
					changed = true
				}
			}
			for key := range objects {
				if !after[key] {
					changed = true
				}
			}
			if !changed {
				t.Errorf("%s ran without error and changed nothing in the catalog", name)
			}
			objects = after

			// 3. Dropping a column must not drop data.
			var n int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM entries`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != rowsBefore {
				t.Fatalf("%s took the entries row count %d → %d — a schema rollback must not destroy data",
					name, rowsBefore, n)
			}
		}
		if ran == 0 {
			t.Fatal("no down migration ran — the glob or the stop condition is wrong")
		}
		t.Logf("exercised %d down migrations", ran)
	})
}

// Keyset pagination against the real thing. The fake repo in the service tests
// can only prove the contract; this proves the SQL — specifically that the
// row-value comparison `(created_at, id) < ($1, $2)` orders identically to
// `ORDER BY created_at DESC, id DESC`. Every row here deliberately shares one
// created_at, which is the case a created_at-only cursor gets wrong and the
// case a seeding run actually produces.
func TestListEntries_CursorPagingOverIdenticalTimestamps(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("cursor"),
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
	if _, err := pool.Exec(ctx, `INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	const n = 7
	shared := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, published_at, published_payload, published_version, created_at, updated_at)
			VALUES ($1,'t',$2,'{}','published',$3,'{}',1,$3,$3)`,
			uuid.New(), typeID, shared); err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
	}

	repo := NewPostgresContentRepository(pool, nil)
	var seen []uuid.UUID
	var after *EntryCursor
	for page := 0; ; page++ {
		if page > n+2 {
			t.Fatal("did not terminate — the cursor predicate is not advancing")
		}
		const size = 2
		rows, total, err := repo.ListEntries(ctx, ListEntriesFilter{
			TenantID:      "t",
			ContentTypeID: typeID,
			Status:        "published",
			Limit:         size,
			CursorPaged:   true,
			After:         after,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if total != 0 {
			t.Fatalf("cursor mode must not run COUNT(*); got total=%d", total)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen = append(seen, r.ID)
		}
		last := rows[len(rows)-1]
		after = &EntryCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(seen) != n {
		t.Fatalf("paged over %d rows, want %d — a shared created_at made the window skip or repeat", len(seen), n)
	}
	uniq := map[uuid.UUID]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("row %s came back on two different pages", id)
		}
		uniq[id] = true
	}
	// The page order must be the declared total order, descending by id here
	// since every created_at is equal.
	ordered := append([]uuid.UUID{}, seen...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() > ordered[j].String() })
	for i := range seen {
		if seen[i] != ordered[i] {
			t.Fatalf("position %d: got %s want %s — paged order is not ORDER BY created_at DESC, id DESC", i, seen[i], ordered[i])
		}
	}
}
