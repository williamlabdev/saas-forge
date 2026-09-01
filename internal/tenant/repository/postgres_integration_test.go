package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

var (
	testPool      *pgxpool.Pool
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
		postgres.WithDatabase("tenants_test"),
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

func loadMigrationSQL() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(filepath.Dir(file), "..", "migrations")
	var combined string
	for _, name := range []string{
		"../../user/migrations/000001_init_users.up.sql",
		"../../auth/migrations/000003_auth.up.sql",
		"000012_tenants_memberships.up.sql",
		"000013_tenant_invites.up.sql",
		"000015_plans.up.sql",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		combined += string(b) + "\n"
	}
	return combined, nil
}

func truncateTenants(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `
		TRUNCATE TABLE tenant_invites, memberships, refresh_tokens, credentials, users RESTART IDENTITY CASCADE
	`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Keep seeded demo tenants out of per-test state: remove everything except them.
	_, err = testPool.Exec(context.Background(),
		`DELETE FROM tenants WHERE slug NOT IN ('tenant_acme', 'tenant_beta', 'tenant_legacy')`)
	if err != nil {
		t.Fatalf("prune tenants: %v", err)
	}
}

// seedUser inserts a bare users row (encrypted columns get placeholder bytes —
// this suite only needs the FK target, not user semantics).
func seedUser(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO users (id, username, username_lookup_hash, email_lookup_hash, email_encrypted, email_encrypted_nonce)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "u_"+id.String()[:8], []byte(id.String()), []byte(id.String()+"e"), []byte{0x01}, []byte{0x02})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newRepo() *PostgresTenantRepository { return NewPostgresTenantRepository(testPool) }

func provision(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	slug, err := newRepo().ProvisionOwnerTx(ctx, tx, userID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("provision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return slug
}

var slugPattern = regexp.MustCompile(`^t_[a-z0-9]{32}$`)

func TestProvisionOwnerTx_CreatesTenantAndOwnerMembership(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	userID := seedUser(t)

	slug := provision(t, userID)
	if !slugPattern.MatchString(slug) {
		t.Fatalf("slug %q does not match opaque pattern (D10)", slug)
	}

	tenant, err := newRepo().TenantBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("tenant by slug: %v", err)
	}
	if tenant.Slug != slug {
		t.Fatalf("slug: got %q want %q", tenant.Slug, slug)
	}

	role, err := newRepo().MembershipRole(ctx, userID, slug)
	if err != nil {
		t.Fatalf("membership role: %v", err)
	}
	if role != domain.RoleOwner {
		t.Fatalf("role: got %q want owner", role)
	}
}

func TestProvisionOwnerTx_RollbackLeavesNothing(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	userID := seedUser(t)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	slug, err := newRepo().ProvisionOwnerTx(ctx, tx, userID)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, err := newRepo().TenantBySlug(ctx, slug); err == nil {
		t.Fatal("tenant should not exist after rollback (D8 atomicity)")
	} else if ae, ok := apperrors.As(err); !ok || ae.Code != apperrors.ErrNotFound.Code {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}
	memberships, err := newRepo().MembershipsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("memberships: %v", err)
	}
	if len(memberships) != 0 {
		t.Fatalf("memberships should be empty after rollback, got %d", len(memberships))
	}
}

func TestMembershipsForUser_OrderedAndScoped(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	alice := seedUser(t)
	bob := seedUser(t)

	first := provision(t, alice)
	// Eliminate the same-microsecond tie window (created_at = NOW() of the tx):
	// push the first membership visibly earlier so ordering is deterministic.
	if _, err := testPool.Exec(ctx,
		`UPDATE memberships SET created_at = created_at - INTERVAL '1 second' WHERE user_id = $1`, alice); err != nil {
		t.Fatalf("backdate first membership: %v", err)
	}
	second := provision(t, alice)

	got, err := newRepo().MembershipsForUser(ctx, alice)
	if err != nil {
		t.Fatalf("memberships: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 memberships, got %d", len(got))
	}
	if got[0].Slug != first || got[1].Slug != second {
		t.Fatalf("order should be earliest-first: got %q,%q want %q,%q",
			got[0].Slug, got[1].Slug, first, second)
	}

	// scoping: bob sees none of alice's tenants
	bobs, err := newRepo().MembershipsForUser(ctx, bob)
	if err != nil {
		t.Fatalf("memberships bob: %v", err)
	}
	if len(bobs) != 0 {
		t.Fatalf("bob should have no memberships, got %d", len(bobs))
	}
}

func TestMembershipRole_NotFound(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	userID := seedUser(t)

	// unknown slug entirely
	if _, err := newRepo().MembershipRole(ctx, userID, "t_nonexistent"); err == nil {
		t.Fatal("want error for unknown membership")
	} else if ae, ok := apperrors.As(err); !ok || ae.Code != apperrors.ErrNotFound.Code {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}

	// existing (seeded) tenant, but no membership for this user
	if _, err := newRepo().MembershipRole(ctx, userID, "tenant_acme"); err == nil {
		t.Fatal("want error: user has no membership in tenant_acme")
	} else if ae, ok := apperrors.As(err); !ok || ae.Code != apperrors.ErrNotFound.Code {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}
}

func TestMembershipRoleCheckConstraint_RejectsUnknownRole(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	userID := seedUser(t)
	slug := provision(t, userID)

	tenant, err := newRepo().TenantBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	other := seedUser(t)
	_, err = testPool.Exec(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, $3)`,
		other, tenant.ID, "banana")
	if err == nil {
		t.Fatal("CHECK constraint should reject unknown role (D3)")
	}
}

func TestSeededDemoTenantsExist(t *testing.T) {
	requireIntegration(t)
	ctx := context.Background()
	want := map[string]string{
		"tenant_acme":   "22222222-2222-2222-2222-222222222201",
		"tenant_beta":   "22222222-2222-2222-2222-222222222202",
		"tenant_legacy": "22222222-2222-2222-2222-222222222203",
	}
	for slug, id := range want {
		tenant, err := newRepo().TenantBySlug(ctx, slug)
		if err != nil {
			t.Fatalf("seeded tenant %s missing: %v (D11)", slug, err)
		}
		if tenant.ID.String() != id {
			t.Fatalf("seeded tenant %s id: got %s want %s (D11 fixed UUID)", slug, tenant.ID, id)
		}
	}
}

func TestProvisionOwnerTx_RetriesOnSlugCollision(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	userID := seedUser(t)

	repo := newRepo()
	// First draw collides with a seeded slug; retry must recover on a fresh
	// savepoint without poisoning the caller's transaction.
	draws := 0
	repo.slugFn = func() (string, error) {
		draws++
		if draws == 1 {
			return "tenant_acme", nil // guaranteed 23505
		}
		return newSlug()
	}

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	slug, err := repo.ProvisionOwnerTx(ctx, tx, userID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("provision after collision should succeed: %v", err)
	}
	if draws < 2 {
		t.Fatalf("expected a retry, got %d draw(s)", draws)
	}
	// outer tx must still be usable after the rolled-back savepoint
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("outer tx poisoned after savepoint retry: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if role, err := newRepo().MembershipRole(ctx, userID, slug); err != nil || role != domain.RoleOwner {
		t.Fatalf("membership after retry: role=%q err=%v", role, err)
	}
}

func TestProvisionOwnerTx_FKViolationLeavesOuterTxUsable(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Non-existent user → memberships FK violation inside the savepoint.
	if _, err := newRepo().ProvisionOwnerTx(ctx, tx, uuid.New()); err == nil {
		t.Fatal("expected FK violation for unknown user")
	}
	// Error path must roll back only the savepoint; outer tx stays usable.
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("outer tx poisoned after failed provision: %v", err)
	}
}

func TestRefreshTokensTenantIDColumn(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	userID := seedUser(t)

	// F2: refresh_tokens.tenant_id exists and accepts a slug; old rows may be NULL.
	_, err := testPool.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at, tenant_id)
		VALUES ($1, $2, NOW() + INTERVAL '1 hour', $3)
	`, userID, "hash-tenant-col-test", "tenant_acme")
	if err != nil {
		t.Fatalf("insert refresh token with tenant_id: %v (F2 ALTER missing?)", err)
	}
	var got *string
	if err := testPool.QueryRow(ctx,
		`SELECT tenant_id FROM refresh_tokens WHERE token_hash = $1`, "hash-tenant-col-test",
	).Scan(&got); err != nil {
		t.Fatalf("read back tenant_id: %v", err)
	}
	if got == nil || *got != "tenant_acme" {
		t.Fatalf("tenant_id round-trip: got %v", got)
	}
}

// --- invite tests (PR-invite) -------------------------------------------------

// seedInvite creates an invite from owner's tenant to the given email hash.
func seedInvite(t *testing.T, tenantSlug string, emailHash []byte, role, tokenHash string, expiresAt time.Time, invitedBy uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, err := newRepo().TenantBySlug(ctx, tenantSlug)
	if err != nil {
		t.Fatalf("tenant by slug: %v", err)
	}
	err = newRepo().CreateInvite(ctx, domain.Invite{
		ID: uuid.New(), TenantID: tenant.ID, EmailLookupHash: emailHash,
		Role: role, TokenHash: tokenHash, InvitedBy: invitedBy,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
}

// Note: seedUser stores email_lookup_hash = []byte(id.String()+"e").
func emailHashFor(id uuid.UUID) []byte { return []byte(id.String() + "e") }

func TestIntegration_AcceptInvite_HappyPath(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()

	owner := seedUser(t)
	slug := provision(t, owner)
	invitee := seedUser(t)

	seedInvite(t, slug, emailHashFor(invitee), domain.RoleEditor, "tok1", time.Now().Add(time.Hour), owner)

	got, err := newRepo().AcceptInvite(ctx, "tok1", invitee)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got.TenantSlug != slug || got.Role != domain.RoleEditor {
		t.Fatalf("got %+v", got)
	}
	role, err := newRepo().MembershipRole(ctx, invitee, slug)
	if err != nil || role != domain.RoleEditor {
		t.Fatalf("membership role=%q err=%v", role, err)
	}

	// Single-use: a second accept (even by the same user) reports used.
	if _, err := newRepo().AcceptInvite(ctx, "tok1", invitee); err != domain.ErrInviteUsed {
		t.Fatalf("second accept err=%v, want ErrInviteUsed", err)
	}
}

func TestIntegration_AcceptInvite_Rejections(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()

	owner := seedUser(t)
	slug := provision(t, owner)
	invitee := seedUser(t)
	stranger := seedUser(t)

	// Unknown token.
	if _, err := newRepo().AcceptInvite(ctx, "nope", invitee); err != domain.ErrInviteNotFound {
		t.Fatalf("unknown token err=%v", err)
	}

	// Expired.
	seedInvite(t, slug, emailHashFor(invitee), domain.RoleViewer, "tok-exp", time.Now().Add(-time.Minute), owner)
	if _, err := newRepo().AcceptInvite(ctx, "tok-exp", invitee); err != domain.ErrInviteExpired {
		t.Fatalf("expired err=%v", err)
	}

	// Email mismatch: stranger holds the token but it names invitee's email.
	seedInvite(t, slug, emailHashFor(invitee), domain.RoleViewer, "tok-mm", time.Now().Add(time.Hour), owner)
	if _, err := newRepo().AcceptInvite(ctx, "tok-mm", stranger); err != domain.ErrInviteEmailMismatch {
		t.Fatalf("mismatch err=%v", err)
	}

	// Already a member: the owner inviting themselves.
	seedInvite(t, slug, emailHashFor(owner), domain.RoleEditor, "tok-own", time.Now().Add(time.Hour), owner)
	if _, err := newRepo().AcceptInvite(ctx, "tok-own", owner); err != domain.ErrAlreadyMember {
		t.Fatalf("already-member err=%v", err)
	}

	// Rejections consume nothing: invitee can still accept tok-mm.
	if _, err := newRepo().AcceptInvite(ctx, "tok-mm", invitee); err != nil {
		t.Fatalf("valid accept after rejections: %v", err)
	}
}

func TestIntegration_AcceptInvite_InviterRevoked(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()

	owner := seedUser(t)
	slug := provision(t, owner)
	invitee := seedUser(t)
	seedInvite(t, slug, emailHashFor(invitee), domain.RoleEditor, "tok-rev", time.Now().Add(time.Hour), owner)

	// The inviter loses their membership after minting the invite.
	if _, err := testPool.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1`, owner); err != nil {
		t.Fatalf("revoke inviter: %v", err)
	}
	if _, err := newRepo().AcceptInvite(ctx, "tok-rev", invitee); err != domain.ErrInviteInviterRevoked {
		t.Fatalf("err=%v, want ErrInviteInviterRevoked", err)
	}

	// Demotion below admin voids it too.
	owner2 := seedUser(t)
	slug2 := provision(t, owner2)
	seedInvite(t, slug2, emailHashFor(invitee), domain.RoleViewer, "tok-dem", time.Now().Add(time.Hour), owner2)
	if _, err := testPool.Exec(ctx, `UPDATE memberships SET role = 'editor' WHERE user_id = $1`, owner2); err != nil {
		t.Fatalf("demote inviter: %v", err)
	}
	if _, err := newRepo().AcceptInvite(ctx, "tok-dem", invitee); err != domain.ErrInviteInviterRevoked {
		t.Fatalf("err=%v, want ErrInviteInviterRevoked", err)
	}
}

func TestIntegration_AcceptInvite_ConcurrentSingleUse(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()

	owner := seedUser(t)
	slug := provision(t, owner)
	invitee := seedUser(t)
	seedInvite(t, slug, emailHashFor(invitee), domain.RoleEditor, "tok-race", time.Now().Add(time.Hour), owner)

	const racers = 8
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			_, err := newRepo().AcceptInvite(ctx, "tok-race", invitee)
			errs <- err
		}()
	}
	var okCount, usedCount int
	for i := 0; i < racers; i++ {
		switch err := <-errs; err {
		case nil:
			okCount++
		case domain.ErrInviteUsed, domain.ErrAlreadyMember:
			// Loser saw the consumed invite, or (losing after the winner's
			// commit) the fresh membership row.
			usedCount++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if okCount != 1 || usedCount != racers-1 {
		t.Fatalf("ok=%d used=%d, want exactly one winner", okCount, usedCount)
	}
	var memberships int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memberships WHERE user_id = $1`, invitee).Scan(&memberships); err != nil {
		t.Fatalf("count: %v", err)
	}
	if memberships != 1 {
		t.Fatalf("memberships=%d, want 1", memberships)
	}
}

// --- plans (TKT-R4b PR1) ------------------------------------------------------

func TestPlanForTenant(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	repo := newRepo()

	owner := seedUser(t)
	slug := provision(t, owner) // new tenants default to 'free'

	// Default plan is free with the seeded free limits.
	p, err := repo.PlanForTenant(ctx, slug)
	if err != nil {
		t.Fatalf("plan for tenant: %v", err)
	}
	if p.Name != "free" || p.MaxTypes != 10 || p.MaxEntries != 1000 ||
		p.MaxFieldsPerType != 50 || p.MaxEntryBytes != 262144 || p.SoftThresholdPct != 80 {
		t.Fatalf("free plan = %+v", p)
	}

	// Move the tenant to pro → pro limits (mirror R4a defaults).
	if _, err := testPool.Exec(ctx, `UPDATE tenants SET plan = 'pro' WHERE slug = $1`, slug); err != nil {
		t.Fatalf("set pro: %v", err)
	}
	p, err = repo.PlanForTenant(ctx, slug)
	if err != nil {
		t.Fatalf("plan for tenant (pro): %v", err)
	}
	if p.Name != "pro" || p.MaxEntries != 100000 || p.MaxEntryBytes != 1048576 || p.SoftThresholdPct != 80 {
		t.Fatalf("pro plan = %+v", p)
	}

	// enterprise = all-zero = unlimited.
	if _, err := testPool.Exec(ctx, `UPDATE tenants SET plan = 'enterprise' WHERE slug = $1`, slug); err != nil {
		t.Fatalf("set enterprise: %v", err)
	}
	p, _ = repo.PlanForTenant(ctx, slug)
	if p.Name != "enterprise" || p.MaxTypes != 0 || p.MaxEntries != 0 {
		t.Fatalf("enterprise plan = %+v", p)
	}

	// Unknown slug → free fallback (fail-safe to the tightest tier).
	p, err = repo.PlanForTenant(ctx, "t_does_not_exist")
	if err != nil {
		t.Fatalf("unknown slug should fall back, not error: %v", err)
	}
	if p.Name != "free" {
		t.Fatalf("unknown slug should resolve to free, got %q", p.Name)
	}
}

func TestProvisionedTenantDefaultsToFreePlan(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()

	owner := seedUser(t)
	slug := provision(t, owner)

	var plan string
	if err := testPool.QueryRow(ctx, `SELECT plan FROM tenants WHERE slug = $1`, slug).Scan(&plan); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if plan != "free" {
		t.Fatalf("provisioned tenant plan = %q, want free", plan)
	}
}

func TestSetTenantPlan(t *testing.T) {
	requireIntegration(t)
	truncateTenants(t)
	ctx := context.Background()
	repo := newRepo()

	owner := seedUser(t)
	slug := provision(t, owner) // defaults to free

	// Happy path: move to pro.
	if err := repo.SetTenantPlan(ctx, slug, "pro"); err != nil {
		t.Fatalf("set pro: %v", err)
	}
	if p, _ := repo.PlanForTenant(ctx, slug); p.Name != "pro" {
		t.Fatalf("plan after set = %q, want pro", p.Name)
	}

	// Unknown plan → ErrPlanUnknown, tenant unchanged.
	if err := repo.SetTenantPlan(ctx, slug, "does_not_exist"); err != domain.ErrPlanUnknown {
		t.Fatalf("unknown plan err = %v, want ErrPlanUnknown", err)
	}
	if p, _ := repo.PlanForTenant(ctx, slug); p.Name != "pro" {
		t.Fatalf("failed set must not change plan, got %q", p.Name)
	}

	// Unknown tenant → NotFound.
	if err := repo.SetTenantPlan(ctx, "t_missing", "pro"); err == nil {
		t.Fatal("unknown tenant should error")
	} else if ae, ok := apperrors.As(err); !ok || ae.Code != apperrors.ErrNotFound.Code {
		t.Fatalf("unknown tenant: want NOT_FOUND, got %v", err)
	}
}
