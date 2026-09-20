// Verification for ADR-012, on a real Postgres.
//
// Not memRepo, and not a fake: every property here — DDL rolling back with its
// ledger row, a checksum surviving a restart, "does this database have tables"
// — lives in the layer a Go fake replaces. A fake would pass these tests while
// the real thing is broken, which is the failure mode this package exists to
// remove, so testing it that way would be the same mistake one level up.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal"
)

var (
	adminDSN      string
	integrationOn bool
	dbSeq         int
)

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerAvailable() {
		fmt.Fprintln(os.Stderr, "migrate integration tests SKIPPED: Docker unavailable or SKIP_INTEGRATION=1")
		os.Exit(m.Run())
	}
	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("migrate_admin"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate integration tests SKIPPED (container): %v\n", err)
		os.Exit(m.Run())
	}
	adminDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}
	integrationOn = true
	code := m.Run()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// requireIntegration makes a skip say so. A suite that silently reports ok
// because Docker was down looks exactly like a suite that passed.
func requireIntegration(t *testing.T) {
	t.Helper()
	if !integrationOn {
		t.Skip("needs Docker: no Postgres container")
	}
}

// freshDB hands out a brand-new database per test. Tests here deliberately put
// the database into states that are mutually exclusive (empty / unadopted /
// behind), so they cannot share one.
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()

	admin, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	defer admin.Close()

	dbSeq++
	name := fmt.Sprintf("mig_%d_%d", os.Getpid()%100000, dbSeq)
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name))
	require.NoError(t, err)

	// The container DSN ends in /migrate_admin; point it at the new database.
	dsn := strings.Replace(adminDSN, "/migrate_admin?", "/"+name+"?", 1)
	require.NotEqual(t, adminDSN, dsn, "could not derive a DSN for %s", name)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func repoMigrations(t *testing.T) []Migration {
	t.Helper()
	ms, err := Discover(internal.MigrationsFS)
	require.NoError(t, err)
	return ms
}

// ledgerVersions reads the numbers the database claims, straight from SQL
// rather than through Inspect — a bug in Inspect must not be able to make the
// ledger agree with itself.
func ledgerVersions(t *testing.T, pool *pgxpool.Pool) []int {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT version FROM schema_migrations ORDER BY version")
	require.NoError(t, err)
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		 ORDER BY table_name`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	require.NoError(t, rows.Err())
	return out
}

// §驗證 1 — empty database, applied to the end, ledger set == disk set.
func TestUpAppliesEverythingAndLedgerMatchesDisk(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)

	applied, err := Up(t.Context(), pool, ms)
	require.NoError(t, err)
	require.Len(t, applied, len(ms))

	want := make([]int, 0, len(ms))
	for _, m := range ms {
		want = append(want, m.Version)
	}
	sort.Ints(want)
	assert.Equal(t, want, ledgerVersions(t, pool))
	assert.NoError(t, Verify(t.Context(), pool, ms), "a fully applied database must boot")

	// Applying again is a no-op rather than an error: `migrate up` is the
	// command a deploy runs unconditionally.
	again, err := Up(t.Context(), pool, ms)
	require.NoError(t, err)
	assert.Empty(t, again)
}

// §驗證 2 — one behind, refused, and the message names the number.
func TestVerifyRefusesADatabaseThatIsBehind(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)

	_, err := Up(t.Context(), pool, ms[:len(ms)-1])
	require.NoError(t, err)

	missing := ms[len(ms)-1].Version
	err = Verify(t.Context(), pool, ms)
	require.Error(t, err)

	var behind ErrBehind
	require.ErrorAs(t, err, &behind)
	assert.Equal(t, []int{missing}, behind.Missing)
	assert.Contains(t, err.Error(), fmt.Sprintf("%06d", missing),
		"the message has to name the number — otherwise it sends someone to diff directories by hand")
}

// §驗證 3 — an applied file edited afterwards is caught by checksum.
func TestVerifyRefusesAnEditedMigration(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)
	_, err := Up(t.Context(), pool, ms)
	require.NoError(t, err)

	// One character, in the middle of the set — enough that "the database no
	// longer matches the files" is true, and nothing else about it changed.
	edited := append([]Migration(nil), ms...)
	victim := len(edited) / 2
	edited[victim].SQL += "\n-- x"
	edited[victim].Checksum = "0000000000000000000000000000000000000000000000000000000000000000"

	err = Verify(t.Context(), pool, edited)
	require.Error(t, err)
	var mismatch ErrChecksumMismatch
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, []int{edited[victim].Version}, mismatch.Versions)
	assert.Contains(t, err.Error(), fmt.Sprintf("%06d", edited[victim].Version))
}

// §驗證 4 — tables but no ledger: refused; adopt clears it and runs no DDL.
func TestUnadoptedDatabaseIsRefusedAndAdoptExecutesNoDDL(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)

	// Stand in for a database that predates the ledger: it has objects, and
	// nothing anywhere records how they got there.
	_, err := pool.Exec(t.Context(), "CREATE TABLE legacy_thing (id int primary key)")
	require.NoError(t, err)

	err = Verify(t.Context(), pool, ms)
	require.Error(t, err)
	var unadopted ErrUnadopted
	require.ErrorAs(t, err, &unadopted)
	assert.Contains(t, err.Error(), "adopt")

	before := tableNames(t, pool)
	highest := ms[len(ms)-1].Version
	require.NoError(t, Adopt(t.Context(), pool, ms, highest))

	// Proof by object list, not by absence of an error: adopt writing the
	// ledger while quietly also running DDL is precisely the mistake that would
	// destroy an existing database.
	after := tableNames(t, pool)
	var created []string
	for _, n := range after {
		if n != LedgerTable && !slices.Contains(before, n) {
			created = append(created, n)
		}
	}
	assert.Empty(t, created, "adopt executed DDL; it must only write the ledger")
	assert.Equal(t, []string{"legacy_thing"}, before)

	assert.NoError(t, Verify(t.Context(), pool, ms), "an adopted database must boot")
	assert.Len(t, ledgerVersions(t, pool), len(ms))
}

// The applier has to refuse an unadopted database too, not only the boot gate.
// `docker compose up` runs `migrate up`, so a guard that lives only in Verify is
// one nobody on that path ever reaches: up would re-run 000001 against objects
// that already exist and report a raw SQLSTATE instead of naming adopt.
func TestUpRefusesAnUnadoptedDatabase(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)

	_, err := pool.Exec(t.Context(), "CREATE TABLE legacy_thing (id int primary key)")
	require.NoError(t, err)

	applied, err := Up(t.Context(), pool, ms)
	var unadopted ErrUnadopted
	require.ErrorAs(t, err, &unadopted)
	assert.Contains(t, err.Error(), "adopt")
	assert.Empty(t, applied)

	// The refusal must also leave nothing behind. An empty ledger written on the
	// way out erases the only evidence that this database needs adopting — no
	// ledger, but tables present — and every later run then reads as merely
	// "behind", which is a different remedy.
	assert.Equal(t, []string{"legacy_thing"}, tableNames(t, pool),
		"a refused up created the ledger; the unadopted state is now unrecoverable")
}

// Same fact, one step later: tables that no ledger row accounts for. This is
// exactly what the old create-then-inspect ordering left behind, so a database
// already damaged that way must still be told to adopt rather than be walked
// into the same failure again.
func TestUpRefusesTablesNoLedgerRowAccountsFor(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)

	_, err := pool.Exec(t.Context(), "CREATE TABLE legacy_thing (id int primary key)")
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), createLedger)
	require.NoError(t, err)

	applied, err := Up(t.Context(), pool, ms)
	var unadopted ErrUnadopted
	require.ErrorAs(t, err, &unadopted)
	assert.Empty(t, applied)
	assert.Empty(t, ledgerVersions(t, pool))
}

// Adopting a fresh database would claim DDL that never ran — the ledger would
// insist the schema is current while every table is missing.
func TestAdoptRefusesAnEmptyDatabase(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)
	ms := repoMigrations(t)

	err := Adopt(t.Context(), pool, ms, ms[len(ms)-1].Version)
	require.ErrorIs(t, err, ErrNothingToAdopt)
	assert.Empty(t, ledgerVersions(t, pool))
}

// §驗證 5 — a migration that fails partway leaves neither objects nor a row.
func TestFailedMigrationLeavesNoObjectsAndNoLedgerRow(t *testing.T) {
	requireIntegration(t)
	pool := freshDB(t)

	// Two statements: the first would succeed on its own, the second cannot.
	// Without DDL-in-transaction this leaves half a schema and no record of it.
	bad, err := Discover(fstest.MapFS{
		"x/migrations/000001_ok.up.sql": {Data: []byte(`CREATE TABLE first_ok (id int);`)},
		"x/migrations/000002_boom.up.sql": {Data: []byte(
			"CREATE TABLE half_created (id int);\nSELECT 1/0;\n")},
	})
	require.NoError(t, err)

	applied, err := Up(t.Context(), pool, bad)
	require.Error(t, err)
	assert.Equal(t, []int{1}, applied, "the migration before the failure is genuinely applied")
	assert.Contains(t, err.Error(), "000002")

	assert.Equal(t, []int{1}, ledgerVersions(t, pool))
	names := tableNames(t, pool)
	assert.Contains(t, names, "first_ok")
	assert.NotContains(t, names, "half_created",
		"the failing migration's DDL survived its own rollback")
}

// §驗證 6 — the three refusals must not shadow one another.
//
// Each state is put to Verify and the result must match its own error type and
// NEITHER of the others. That is the property "remove one check and only its
// own test goes red" is really asserting: if, say, the unadopted check also
// fired for a database that is merely behind, deleting the behind check would
// leave its test green and the gap invisible.
func TestEachRefusalIsDistinctAndDoesNotShadowTheOthers(t *testing.T) {
	requireIntegration(t)
	ms := repoMigrations(t)

	setups := map[string]struct {
		arrange func(t *testing.T, pool *pgxpool.Pool) []Migration
		want    string
	}{
		"empty": {
			arrange: func(*testing.T, *pgxpool.Pool) []Migration { return ms },
			want:    "uninitialised",
		},
		"unadopted": {
			arrange: func(t *testing.T, pool *pgxpool.Pool) []Migration {
				_, err := pool.Exec(t.Context(), "CREATE TABLE legacy_thing (id int)")
				require.NoError(t, err)
				return ms
			},
			want: "unadopted",
		},
		"behind": {
			arrange: func(t *testing.T, pool *pgxpool.Pool) []Migration {
				_, err := Up(t.Context(), pool, ms[:len(ms)-1])
				require.NoError(t, err)
				return ms
			},
			want: "behind",
		},
		"checksum": {
			arrange: func(t *testing.T, pool *pgxpool.Pool) []Migration {
				_, err := Up(t.Context(), pool, ms)
				require.NoError(t, err)
				edited := append([]Migration(nil), ms...)
				edited[0].Checksum = "deadbeef"
				return edited
			},
			want: "checksum",
		},
	}

	for name, tc := range setups {
		t.Run(name, func(t *testing.T) {
			pool := freshDB(t)
			use := tc.arrange(t, pool)
			err := Verify(t.Context(), pool, use)
			require.Error(t, err)

			var (
				uninit    ErrUninitialised
				unadopted ErrUnadopted
				behind    ErrBehind
				checksum  ErrChecksumMismatch
			)
			got := map[string]bool{
				"uninitialised": errors.As(err, &uninit),
				"unadopted":     errors.As(err, &unadopted),
				"behind":        errors.As(err, &behind),
				"checksum":      errors.As(err, &checksum),
			}
			for kind, matched := range got {
				if kind == tc.want {
					assert.True(t, matched, "expected %s, got %v", kind, err)
					continue
				}
				assert.False(t, matched,
					"%s state also matched %s — the two checks shadow each other, so removing "+
						"one would not turn any test red", name, kind)
			}
		})
	}
}
