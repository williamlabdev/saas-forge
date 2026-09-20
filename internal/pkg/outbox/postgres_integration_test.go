package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// These tests exercise the DB-level outbox behaviour behind TKT-OBX-1 (claimed_at,
// ReclaimStale, processing stats). They require Docker and are skipped otherwise.

var (
	obPool      *pgxpool.Pool
	obContainer testcontainers.Container
	obOn        bool
)

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerAvailable() {
		if !dockerAvailable() {
			fmt.Fprintln(os.Stderr, "outbox integration tests skipped: Docker not available")
		}
		os.Exit(m.Run())
	}

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("outbox_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "outbox integration tests skipped (postgres container): %v\n", err)
		os.Exit(m.Run())
	}
	obContainer = container

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}
	obPool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}

	migrationSQL, err := loadOutboxMigrations()
	if err != nil {
		obPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}
	if _, err := obPool.Exec(ctx, migrationSQL); err != nil {
		obPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	obOn = true
	code := m.Run()

	obPool.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// loadOutboxMigrations applies the minimal schema the integration_outbox table
// needs: users/roles (000001), the outbox table + enum (000002), and the
// claimed_at column added by TKT-OBX-1 (000011).
func loadOutboxMigrations() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "user", "migrations")
	var combined string
	for _, name := range []string{
		"000001_init_users.up.sql",
		"000002_iam_outbox.up.sql",
		"000011_outbox_claimed_at.up.sql",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		combined += string(b) + "\n"
	}
	return combined, nil
}

func requireOutboxIntegration(t *testing.T) {
	t.Helper()
	if !obOn {
		t.Skip("outbox integration tests require Docker; set SKIP_INTEGRATION=1 to silence or start Docker")
	}
}

func truncateOutbox(t *testing.T) {
	t.Helper()
	_, err := obPool.Exec(context.Background(), `TRUNCATE integration_outbox`)
	require.NoError(t, err)
}

func enqueueOne(t *testing.T, repo *PostgresRepository, eventType string) uuid.UUID {
	t.Helper()
	agg := uuid.New()
	tx, err := obPool.Begin(context.Background())
	require.NoError(t, err)
	require.NoError(t, repo.EnqueueTx(context.Background(), tx, agg, eventType, json.RawMessage(`{}`), uuid.NewString()))
	require.NoError(t, tx.Commit(context.Background()))
	return agg
}

func TestIntegration_ClaimPendingSetsClaimedAt(t *testing.T) {
	requireOutboxIntegration(t)
	truncateOutbox(t)
	repo := NewPostgresRepository(obPool)
	enqueueOne(t, repo, EventUserCreated)

	claimed, err := repo.ClaimPending(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	var claimedAt *time.Time
	err = obPool.QueryRow(context.Background(),
		`SELECT claimed_at FROM integration_outbox WHERE id = $1`, claimed[0].ID).Scan(&claimedAt)
	require.NoError(t, err)
	require.NotNil(t, claimedAt, "ClaimPending must stamp claimed_at so the reaper can detect staleness")
}

func TestIntegration_ReclaimStaleRequeuesOnlyOldProcessing(t *testing.T) {
	requireOutboxIntegration(t)
	truncateOutbox(t)
	repo := NewPostgresRepository(obPool)
	enqueueOne(t, repo, EventUserCreated)

	claimed, err := repo.ClaimPending(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	id := claimed[0].ID

	// Not yet stale: a generous threshold reclaims nothing.
	n, err := repo.ReclaimStale(context.Background(), time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "fresh processing row must not be reclaimed")

	// Backdate the claim so it is stale, then reclaim.
	_, err = obPool.Exec(context.Background(),
		`UPDATE integration_outbox SET claimed_at = now() - interval '10 minutes' WHERE id = $1`, id)
	require.NoError(t, err)

	n, err = repo.ReclaimStale(context.Background(), time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "stale processing row must be reclaimed")

	// It is back to pending, claimed_at cleared, retry_count untouched, and re-claimable.
	var status string
	var retry int
	var claimedAt *time.Time
	err = obPool.QueryRow(context.Background(),
		`SELECT status, retry_count, claimed_at FROM integration_outbox WHERE id = $1`, id).
		Scan(&status, &retry, &claimedAt)
	require.NoError(t, err)
	assert.Equal(t, "pending", status)
	assert.Equal(t, 0, retry, "reaper must not increment retry_count for a shutdown-orphaned row")
	assert.Nil(t, claimedAt)

	reclaimed, err := repo.ClaimPending(context.Background(), 10)
	require.NoError(t, err)
	assert.Len(t, reclaimed, 1, "reclaimed row is deliverable again")
}

func TestIntegration_PendingStatsCountsProcessing(t *testing.T) {
	requireOutboxIntegration(t)
	truncateOutbox(t)
	repo := NewPostgresRepository(obPool)
	enqueueOne(t, repo, EventUserCreated) // stays pending
	enqueueOne(t, repo, EventUserUpdated) // will be claimed → processing

	_, err := repo.ClaimPending(context.Background(), 1)
	require.NoError(t, err)

	st, err := repo.PendingStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.Pending, "one row remains pending")
	assert.Equal(t, int64(1), st.Processing, "claimed row is visible as processing, not hidden")
}
