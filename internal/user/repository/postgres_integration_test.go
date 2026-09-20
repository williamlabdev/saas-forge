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
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	authrepo "github.com/williamlabdev/saas-forge/internal/auth/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/idempotency"
	"github.com/williamlabdev/saas-forge/internal/pkg/pagination"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

var (
	testPool      *pgxpool.Pool
	testEnc       crypto.FieldEncryptor
	testIdx       crypto.BlindIndexer
	testContainer testcontainers.Container
	integrationOn bool
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
			fmt.Fprintln(os.Stderr, "integration tests skipped: Docker not available")
		}
		os.Exit(m.Run())
	}

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("users_test"),
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
	testContainer = container

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}

	testPool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}

	migrationSQL, err := loadMigrationSQL()
	if err != nil {
		testPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}
	if _, err := testPool.Exec(ctx, migrationSQL); err != nil {
		testPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	encKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i)
	}
	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = byte(i + 32)
	}

	testEnc, err = crypto.NewAESGCMEncryptor(encKey)
	if err != nil {
		testPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}
	testIdx, err = crypto.NewHMACBlindIndexer(pepper)
	if err != nil {
		testPool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	integrationOn = true
	code := m.Run()

	testPool.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func requireIntegration(t *testing.T) {
	t.Helper()
	if !integrationOn {
		t.Skip("integration tests require Docker; set SKIP_INTEGRATION=1 to silence or start Docker")
	}
}

func newTestRepo(t *testing.T) *PostgresUserRepository {
	t.Helper()
	idem := idempotency.NewPostgresRegistrationStore(testPool)
	return NewPostgresUserRepository(testPool, testEnc, nil, authrepo.NewPostgresCredentialRepository(testPool), idem, nil)
}

func loadMigrationSQL() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(filepath.Dir(file), "..", "migrations")
	var combined string
	for _, name := range []string{
		"000001_init_users.up.sql",
		"000002_iam_outbox.up.sql",
		"../../auth/migrations/000003_auth.up.sql",
		"000004_registration_idempotency.up.sql",
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

func truncateUsers(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `
		TRUNCATE TABLE registration_idempotency, integration_outbox, refresh_tokens, credentials, user_roles, users RESTART IDENTITY CASCADE
	`)
	if err != nil {
		t.Fatalf("truncate users: %v", err)
	}
}

func buildUser(t *testing.T, suffix string) *domain.User {
	t.Helper()
	username := "user_" + suffix
	email := username + "@example.com"
	usernameHash, err := testIdx.Index(username)
	if err != nil {
		t.Fatalf("index username: %v", err)
	}
	emailHash, err := testIdx.Index(email)
	if err != nil {
		t.Fatalf("index email: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &domain.User{
		ID:                 uuid.New(),
		Username:           username,
		Email:              email,
		DisplayName:        "Display " + suffix,
		Phone:              "+1555000" + suffix,
		Preferences:        domain.Preferences{"locale": "en-US", "theme": "dark"},
		Status:             domain.StatusActive,
		UsernameLookupHash: usernameHash,
		EmailLookupHash:    emailHash,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

func TestPostgresUserRepository_CreateAndGetByID(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	seed := buildUser(t, "01")

	if err := repo.Create(ctx, seed, "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.ByID(ctx, seed.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Email != seed.Email {
		t.Fatalf("email: got %q want %q", got.Email, seed.Email)
	}
	if got.DisplayName != seed.DisplayName {
		t.Fatalf("display_name: got %q want %q", got.DisplayName, seed.DisplayName)
	}
	if got.Preferences["theme"] != "dark" {
		t.Fatalf("preferences theme: %v", got.Preferences["theme"])
	}
}

func TestPostgresUserRepository_UniqueEmailAndUsername(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	first := buildUser(t, "dup")

	if err := repo.Create(ctx, first, "", ""); err != nil {
		t.Fatalf("create first: %v", err)
	}

	second := buildUser(t, "dup2")
	second.Email = first.Email
	second.EmailLookupHash = first.EmailLookupHash

	err := repo.Create(ctx, second, "", "")
	if err == nil {
		t.Fatal("expected duplicate email error")
	}
	ae, ok := apperrors.As(err)
	if !ok || ae.Code != apperrors.ErrEmailTaken.Code {
		t.Fatalf("got err %v want USER_EMAIL_TAKEN", err)
	}

	third := buildUser(t, "dup3")
	third.Username = first.Username
	third.UsernameLookupHash = first.UsernameLookupHash
	third.Email = "other_" + third.Email
	third.EmailLookupHash, _ = testIdx.Index(third.Email)

	err = repo.Create(ctx, third, "", "")
	if err == nil {
		t.Fatal("expected duplicate username error")
	}
	ae, ok = apperrors.As(err)
	if !ok || ae.Code != apperrors.ErrUsernameTaken.Code {
		t.Fatalf("got err %v want USER_USERNAME_TAKEN", err)
	}
}

func TestPostgresUserRepository_SoftDelete(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	seed := buildUser(t, "del")

	if err := repo.Create(ctx, seed, "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SoftDelete(ctx, seed.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	got, err := repo.ByID(ctx, seed.ID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got.Status != domain.StatusDeleted {
		t.Fatalf("status: got %q want deleted", got.Status)
	}
	if got.DeletedAt == nil {
		t.Fatal("expected deleted_at to be set")
	}
}

// SoftDelete flips users.status, but the sessions already issued against that
// row live in refresh_tokens and outlive the flip on their own expiry. Without
// the revoke, a soft-deleted account keeps minting new tokens until then.
func TestPostgresUserRepository_SoftDeleteRevokesRefreshTokens(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	seed := buildUser(t, "revoke")

	if err := repo.Create(ctx, seed, "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Raw SQL rather than the credential repository's own methods: this
	// harness applies only the user + auth migrations (loadMigrationSQL), so
	// refresh_tokens has no tenant_id column here. Asserting the row state is
	// also the more direct claim for a repository-layer test.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)
	`, seed.ID, "tokhash-revoke", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("insert refresh token: %v", err)
	}

	if err := repo.SoftDelete(ctx, seed.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	var revokedAt *time.Time
	if err := testPool.QueryRow(ctx, `
		SELECT revoked_at FROM refresh_tokens WHERE token_hash = $1
	`, "tokhash-revoke").Scan(&revokedAt); err != nil {
		t.Fatalf("read refresh token: %v", err)
	}
	if revokedAt == nil {
		t.Fatal("refresh token must be revoked after soft delete, but revoked_at is NULL")
	}
}

func TestPostgresUserRepository_UpdatePreferencesMergeAndReplace(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	seed := buildUser(t, "prefs")

	if err := repo.Create(ctx, seed, "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	patch := domain.Preferences{"notifications": map[string]any{"email": false}}
	if err := repo.UpdatePreferences(ctx, seed.ID, patch, true); err != nil {
		t.Fatalf("merge preferences: %v", err)
	}

	got, err := repo.ByID(ctx, seed.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Preferences["locale"] != "en-US" {
		t.Fatalf("merge should keep locale, got %v", got.Preferences)
	}
	if got.Preferences["notifications"] == nil {
		t.Fatal("merge should add notifications")
	}

	replacement := domain.Preferences{"theme": "light"}
	if err := repo.UpdatePreferences(ctx, seed.ID, replacement, false); err != nil {
		t.Fatalf("replace preferences: %v", err)
	}

	got, err = repo.ByID(ctx, seed.ID)
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}
	if _, ok := got.Preferences["locale"]; ok {
		t.Fatalf("replace should drop locale, got %v", got.Preferences)
	}
	if got.Preferences["theme"] != "light" {
		t.Fatalf("theme: got %v", got.Preferences["theme"])
	}
}

func TestPostgresUserRepository_ListPage(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)

	u1 := buildUser(t, "list1")
	u2 := buildUser(t, "list2")
	if err := repo.Create(ctx, u1, "", ""); err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := repo.Create(ctx, u2, "", ""); err != nil {
		t.Fatalf("create u2: %v", err)
	}

	page1, err := repo.ListPage(ctx, "active", nil, 1)
	if err != nil {
		t.Fatalf("list first: %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("want 1 row, got %d", len(page1))
	}

	cursor := &pagination.UserCursor{CreatedAt: page1[0].CreatedAt, ID: page1[0].ID}
	page2, err := repo.ListPage(ctx, "active", cursor, 10)
	if err != nil {
		t.Fatalf("list after cursor: %v", err)
	}
	if len(page2) < 1 {
		t.Fatalf("expected at least one more user")
	}
	for _, u := range page2 {
		if u.ID == page1[0].ID {
			t.Fatalf("cursor page should not repeat first id")
		}
	}
}

func TestPostgresUserRepository_GetByEmailHash(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	seed := buildUser(t, "emailhash")

	if err := repo.Create(ctx, seed, "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.ByEmailHash(ctx, seed.EmailLookupHash)
	if err != nil {
		t.Fatalf("get by email hash: %v", err)
	}
	if got.ID != seed.ID {
		t.Fatalf("id: got %v want %v", got.ID, seed.ID)
	}
}

func TestRegistrationIdempotencyStore(t *testing.T) {
	requireIntegration(t)
	truncateUsers(t)
	ctx := context.Background()
	repo := newTestRepo(t)
	store := idempotency.NewPostgresRegistrationStore(testPool)
	seed := buildUser(t, "idem")

	if err := repo.Create(ctx, seed, "", "test-idem-key-01"); err != nil {
		t.Fatalf("create: %v", err)
	}
	uid, ok, err := store.UserIDByKey(ctx, "test-idem-key-01")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if uid != seed.ID {
		t.Fatalf("user id: got %v want %v", uid, seed.ID)
	}
}
