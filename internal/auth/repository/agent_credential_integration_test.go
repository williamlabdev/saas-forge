package repository

import (
	"context"
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

// The agent-credential registry against real Postgres.
//
// It is here and not only in the e2e suite because the properties below are
// SQL properties: a CASCADE, a COALESCE that must not move a timestamp, a
// tenant predicate inside a WHERE clause, and a CHECK constraint. A fake
// repository written in Go agrees with whatever the Go code believes and can
// show none of them (the memRepo lesson, 0801).

var (
	acPool      *pgxpool.Pool
	acContainer testcontainers.Container
	acOn        bool
)

func acDockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	if os.Getenv("SKIP_INTEGRATION") == "1" || !acDockerAvailable() {
		if !acDockerAvailable() {
			fmt.Fprintln(os.Stderr, "integration tests skipped: Docker not available")
		}
		os.Exit(m.Run())
	}

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("auth_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration tests skipped (postgres container): %v\n", err)
		os.Exit(m.Run())
	}
	acContainer = container

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}
	acPool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}

	sql, err := acMigrationSQL()
	if err != nil {
		acPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}
	if _, err := acPool.Exec(ctx, sql); err != nil {
		acPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	acOn = true
	code := m.Run()

	acPool.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func acMigrationSQL() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(filepath.Dir(file), "..", "migrations")
	var combined string
	for _, name := range []string{
		"../../user/migrations/000001_init_users.up.sql",
		"000003_auth.up.sql",
		"000035_agent_credentials.up.sql",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		combined += string(b) + "\n"
	}
	return combined, nil
}

func requireAC(t *testing.T) {
	t.Helper()
	if !acOn {
		t.Skip("integration tests require Docker; set SKIP_INTEGRATION=1 to silence or start Docker")
	}
}

// acUser inserts a person for the credential to belong to. The columns are the
// minimum 000001 demands; nothing here reads them back.
func acUser(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	b := []byte(id.String())
	_, err := acPool.Exec(context.Background(), `
		INSERT INTO users
			(id, username, username_lookup_hash, email_lookup_hash,
			 email_encrypted, email_encrypted_nonce, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'active')
	`, id, "u"+id.String()[:8], b, b, b, b)
	require.NoError(t, err)
	return id
}

func acCred(t *testing.T, tenant string, principal uuid.UUID, expires time.Time) AgentCredential {
	t.Helper()
	return AgentCredential{
		ID:           uuid.New(),
		TenantID:     tenant,
		AgentID:      "content-bot",
		PrincipalID:  principal,
		TenantRole:   "admin",
		AllowedTypes: []string{"post"},
		ExpiresAt:    expires,
	}
}

func TestAgentCredentialIsActiveDistinguishesTheThreeWaysOfBeingDead(t *testing.T) {
	requireAC(t)
	ctx := context.Background()
	repo := NewPostgresAgentCredentialRepository(acPool)
	user := acUser(t)

	live := acCred(t, "t_a", user, time.Now().Add(time.Hour))
	require.NoError(t, repo.Insert(ctx, live))
	active, err := repo.IsActive(ctx, live.ID)
	require.NoError(t, err)
	require.True(t, active, "a freshly minted credential must work, or nothing below means anything")

	expired := acCred(t, "t_a", user, time.Now().Add(-time.Minute))
	require.NoError(t, repo.Insert(ctx, expired))
	active, err = repo.IsActive(ctx, expired.ID)
	require.NoError(t, err)
	assert.False(t, active, "expiry is checked against the DATABASE clock, not the token's")

	revoked := acCred(t, "t_a", user, time.Now().Add(time.Hour))
	require.NoError(t, repo.Insert(ctx, revoked))
	require.NoError(t, repo.Revoke(ctx, "t_a", revoked.ID, user))
	active, err = repo.IsActive(ctx, revoked.ID)
	require.NoError(t, err)
	assert.False(t, active)

	// A credential id nobody minted. This is the CASCADE case too, and the one
	// where a naive implementation returns an error the caller might treat as
	// "unknown, carry on" — it must be a plain, quiet false.
	active, err = repo.IsActive(ctx, uuid.New())
	require.NoError(t, err, "an unknown id is an answer, not a failure")
	assert.False(t, active)
}

// Deleting the person deletes their agents' credentials, and because an absent
// row is a refusal, that IS revocation. A departed employee leaves no agent
// running behind them.
func TestDeletingThePrincipalKillsItsAgentCredentials(t *testing.T) {
	requireAC(t)
	ctx := context.Background()
	repo := NewPostgresAgentCredentialRepository(acPool)

	user := acUser(t)
	cred := acCred(t, "t_cascade", user, time.Now().Add(time.Hour))
	require.NoError(t, repo.Insert(ctx, cred))
	active, err := repo.IsActive(ctx, cred.ID)
	require.NoError(t, err)
	require.True(t, active)

	_, err = acPool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user)
	require.NoError(t, err)

	active, err = repo.IsActive(ctx, cred.ID)
	require.NoError(t, err)
	assert.False(t, active, "the credential outliving its principal would be an unattributable writer")
}

// The kill switch is pressed twice under incident conditions. The second press
// must not be an error and must not rewrite WHEN the credential stopped.
func TestRevokeIsIdempotentAndKeepsTheOriginalTimestamp(t *testing.T) {
	requireAC(t)
	ctx := context.Background()
	repo := NewPostgresAgentCredentialRepository(acPool)

	user := acUser(t)
	other := acUser(t)
	cred := acCred(t, "t_twice", user, time.Now().Add(time.Hour))
	require.NoError(t, repo.Insert(ctx, cred))

	require.NoError(t, repo.Revoke(ctx, "t_twice", cred.ID, user))
	first := acRevokedAt(t, cred.ID)

	require.NoError(t, repo.Revoke(ctx, "t_twice", cred.ID, other),
		"pressing an off switch that is already off is not an error")
	assert.Equal(t, first, acRevokedAt(t, cred.ID),
		"the second press must not move the moment the credential actually stopped")
	assert.Equal(t, user, acRevokedBy(t, cred.ID),
		"nor rewrite who stopped it")
}

// Tenant scoping lives in the WHERE clause, which is the only thing between an
// owner of one tenant and a credential id belonging to another.
func TestRevokeCannotReachAnotherTenantsCredential(t *testing.T) {
	requireAC(t)
	ctx := context.Background()
	repo := NewPostgresAgentCredentialRepository(acPool)

	user := acUser(t)
	cred := acCred(t, "t_owner", user, time.Now().Add(time.Hour))
	require.NoError(t, repo.Insert(ctx, cred))

	err := repo.Revoke(ctx, "t_intruder", cred.ID, acUser(t))
	require.ErrorIs(t, err, ErrAgentCredentialNotFound,
		"and it is NOT FOUND, not FORBIDDEN — the caller learns nothing about another tenant's ids")

	active, err := repo.IsActive(ctx, cred.ID)
	require.NoError(t, err)
	assert.True(t, active, "the credential must still be live: a refused revoke that revoked anyway is worse than either")

	creds, err := repo.ListByTenant(ctx, "t_intruder")
	require.NoError(t, err)
	assert.Empty(t, creds)
}

// ErrAgentScopeUnset's copy in the schema. It guards the INSERTs that do not go
// through the signer — a backfill, a fixture, a future admin path.
func TestTheDatabaseRefusesACredentialScopedToNothing(t *testing.T) {
	requireAC(t)
	ctx := context.Background()
	repo := NewPostgresAgentCredentialRepository(acPool)

	cred := acCred(t, "t_scope", acUser(t), time.Now().Add(time.Hour))
	cred.AllowedTypes = []string{}
	require.Error(t, repo.Insert(ctx, cred), "an empty whitelist must not read as 'everything'")
}

func acRevokedAt(t *testing.T, id uuid.UUID) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, acPool.QueryRow(context.Background(),
		`SELECT revoked_at FROM agent_credentials WHERE id = $1`, id).Scan(&at))
	return at
}

func acRevokedBy(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	var by uuid.UUID
	require.NoError(t, acPool.QueryRow(context.Background(),
		`SELECT revoked_by FROM agent_credentials WHERE id = $1`, id).Scan(&by))
	return by
}
