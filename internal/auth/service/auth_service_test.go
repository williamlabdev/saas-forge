package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// --- fakes -------------------------------------------------------------------

type storedRefresh struct {
	userID  uuid.UUID
	slug    string
	expires time.Time
	revoked bool
}

type fakeCredRepo struct {
	userID    uuid.UUID
	pwHash    string
	status    string
	refreshes map[string]*storedRefresh
}

func newFakeCredRepo(userID uuid.UUID, pwHash string) *fakeCredRepo {
	return &fakeCredRepo{userID: userID, pwHash: pwHash, status: "active", refreshes: map[string]*storedRefresh{}}
}

func (f *fakeCredRepo) InsertCredentialsTx(context.Context, pgx.Tx, uuid.UUID, string) error {
	return nil
}
func (f *fakeCredRepo) UpdateLastLogin(context.Context, uuid.UUID) error { return nil }
func (f *fakeCredRepo) UserIDByEmailLookup(context.Context, []byte) (uuid.UUID, error) {
	return f.userID, nil
}
func (f *fakeCredRepo) GetPasswordHash(context.Context, uuid.UUID) (string, error) {
	return f.pwHash, nil
}
func (f *fakeCredRepo) StoreRefreshToken(_ context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time, tenantSlug string) error {
	f.refreshes[tokenHash] = &storedRefresh{userID: userID, slug: tenantSlug, expires: expiresAt}
	return nil
}
func (f *fakeCredRepo) RevokeRefreshToken(_ context.Context, tokenHash string) error {
	if r, ok := f.refreshes[tokenHash]; ok {
		r.revoked = true
	}
	return nil
}
func (f *fakeCredRepo) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	for _, r := range f.refreshes {
		if r.userID == userID {
			r.revoked = true
		}
	}
	return nil
}
func (f *fakeCredRepo) UserStatusByID(context.Context, uuid.UUID) (string, error) {
	return f.status, nil
}
func (f *fakeCredRepo) FindValidRefresh(_ context.Context, tokenHash string) (uuid.UUID, string, error) {
	r, ok := f.refreshes[tokenHash]
	if !ok || r.revoked || time.Now().After(r.expires) {
		return uuid.Nil, "", pgx.ErrNoRows
	}
	return r.userID, r.slug, nil
}

type fakeIAM struct{ roles []string }

func (f fakeIAM) AssignRoleByName(context.Context, uuid.UUID, string) error { return nil }
func (f fakeIAM) RevokeRoleByName(context.Context, uuid.UUID, string) error { return nil }
func (f fakeIAM) RolesForUser(context.Context, uuid.UUID) ([]string, error) {
	return f.roles, nil
}

type fakeTenants struct {
	memberships []tenantdomain.UserMembership
}

func (f *fakeTenants) MembershipsForUser(context.Context, uuid.UUID) ([]tenantdomain.UserMembership, error) {
	return f.memberships, nil
}
func (f *fakeTenants) MembershipRole(_ context.Context, _ uuid.UUID, slug string) (string, error) {
	for _, m := range f.memberships {
		if m.Slug == slug {
			return m.Role, nil
		}
	}
	return "", apperrors.ErrNotFound
}

// --- harness -----------------------------------------------------------------

const testPassword = "password12"

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func newTestAuth(t *testing.T, platformRoles []string, memberships []tenantdomain.UserMembership) (AuthService, *fakeCredRepo, *fakeTenants, *jwt.Signer, uuid.UUID) {
	t.Helper()
	userID := uuid.New()
	signer := jwt.NewSigner(testSecret, 15*time.Minute)
	idx, err := crypto.NewHMACBlindIndexer(bytesRepeat32())
	require.NoError(t, err)

	svc := NewAuthService(nil, idx, signer, fakeIAM{roles: platformRoles}, &fakeTenants{}, nil, time.Hour)
	pwHash, err := svc.HashPassword(testPassword)
	require.NoError(t, err)

	repo := newFakeCredRepo(userID, pwHash)
	tenants := &fakeTenants{memberships: memberships}
	svc = NewAuthService(repo, idx, signer, fakeIAM{roles: platformRoles}, tenants, nil, time.Hour)
	return svc, repo, tenants, signer, userID
}

func bytesRepeat32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0xab
	}
	return b
}

func membership(slug, name, role string, createdAt time.Time) tenantdomain.UserMembership {
	return tenantdomain.UserMembership{TenantID: uuid.New(), Slug: slug, Name: name, Role: role, CreatedAt: createdAt}
}

func login(t *testing.T, svc AuthService) *TokenResponse {
	t.Helper()
	tokens, err := svc.Login(context.Background(), LoginInput{Email: "a@example.com", Password: testPassword})
	require.NoError(t, err)
	return tokens
}

// --- tests -------------------------------------------------------------------

func TestLogin_SingleMembership_TokenCarriesTenantAndRole(t *testing.T) {
	svc, repo, _, signer, _ := newTestAuth(t, []string{"admin"},
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "owner", time.Now())})

	tokens := login(t, svc)
	assert.Equal(t, "t_alpha", tokens.TenantID)
	assert.Empty(t, tokens.AvailableTenants, "single membership needs no tenant picker")

	claims, err := signer.ParseAccessToken(tokens.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, "t_alpha", claims.TenantID)
	assert.Equal(t, "owner", claims.TenantRole)
	// D6 namespace guard: tenant role never leaks into the platform plane.
	assert.Equal(t, []string{"admin"}, claims.Roles)
	assert.NotContains(t, claims.Roles, "owner")

	// F2: the refresh token remembers the active tenant.
	require.Len(t, repo.refreshes, 1)
	for _, r := range repo.refreshes {
		assert.Equal(t, "t_alpha", r.slug)
	}
}

func TestLogin_MultiMembership_DefaultsToEarliestAndListsAll(t *testing.T) {
	now := time.Now()
	svc, _, _, signer, _ := newTestAuth(t, nil, []tenantdomain.UserMembership{
		membership("t_first", "First", "editor", now.Add(-time.Hour)),
		membership("t_second", "Second", "viewer", now),
	})

	tokens := login(t, svc)
	assert.Equal(t, "t_first", tokens.TenantID)
	require.Len(t, tokens.AvailableTenants, 2)
	assert.Equal(t, TenantOption{Slug: "t_first", Name: "First", Role: "editor"}, tokens.AvailableTenants[0])
	assert.Equal(t, TenantOption{Slug: "t_second", Name: "Second", Role: "viewer"}, tokens.AvailableTenants[1])

	claims, err := signer.ParseAccessToken(tokens.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, "editor", claims.TenantRole)
}

func TestLogin_NoMembership_IssuesTenantlessToken(t *testing.T) {
	// Platform operators have no membership but must still sign in (plan §6);
	// content access is rejected downstream by the empty-tenant guard.
	svc, repo, _, signer, _ := newTestAuth(t, []string{"admin"}, nil)

	tokens := login(t, svc)
	assert.Empty(t, tokens.TenantID)

	claims, err := signer.ParseAccessToken(tokens.AccessToken)
	require.NoError(t, err)
	assert.Empty(t, claims.TenantID)
	assert.Empty(t, claims.TenantRole)
	for _, r := range repo.refreshes {
		assert.Empty(t, r.slug)
	}
}

func TestRefresh_ReResolvesRoleFromMembership(t *testing.T) {
	svc, _, tenants, signer, _ := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "editor", time.Now())})

	tokens := login(t, svc)

	// Role change lands at the next refresh — the revocation checkpoint (§4.1).
	tenants.memberships[0].Role = "viewer"
	refreshed, err := svc.Refresh(context.Background(), tokens.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, "t_alpha", refreshed.TenantID)

	claims, err := signer.ParseAccessToken(refreshed.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, "viewer", claims.TenantRole)
}

func TestRefresh_LegacyNullTenant_DegradesToDefaultMembership(t *testing.T) {
	// In-flight refresh tokens from before tenant tracking carry no slug (F6);
	// refresh resolves the default membership instead of forcing re-login.
	svc, repo, _, signer, userID := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "owner", time.Now())})

	raw, err := newRefreshToken()
	require.NoError(t, err)
	require.NoError(t, repo.StoreRefreshToken(context.Background(), userID, hashRefreshToken(raw), time.Now().Add(time.Hour), ""))

	refreshed, err := svc.Refresh(context.Background(), raw)
	require.NoError(t, err)
	assert.Equal(t, "t_alpha", refreshed.TenantID)

	claims, err := signer.ParseAccessToken(refreshed.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, "owner", claims.TenantRole)
}

func TestRefresh_MembershipRevoked_ForcesReauth(t *testing.T) {
	svc, _, tenants, _, _ := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "owner", time.Now())})

	tokens := login(t, svc)

	tenants.memberships = nil // membership revoked between issuance and refresh
	_, err := svc.Refresh(context.Background(), tokens.RefreshToken)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMembershipRevoked), "got %v", err)
}

func TestIssueTokens_RejectsRoleOutsideAllowedSet(t *testing.T) {
	svc, _, _, _, _ := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "superuser", time.Now())})

	_, err := svc.Login(context.Background(), LoginInput{Email: "a@example.com", Password: testPassword})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside allowed set")
}

func TestSwitchTenant_IssuesTokensForTargetMembership(t *testing.T) {
	now := time.Now()
	svc, repo, _, signer, userID := newTestAuth(t, nil, []tenantdomain.UserMembership{
		membership("t_first", "First", "owner", now.Add(-time.Hour)),
		membership("t_second", "Second", "editor", now),
	})

	// Establish a pre-switch session so the parallel-session claim below is
	// actually exercised, not vacuous.
	preSwitch := login(t, svc)
	preSwitchHash := hashRefreshToken(preSwitch.RefreshToken)
	require.False(t, repo.refreshes[preSwitchHash].revoked)

	tokens, err := svc.SwitchTenant(context.Background(), userID, "t_second")
	require.NoError(t, err)
	assert.Equal(t, "t_second", tokens.TenantID)

	claims, err := signer.ParseAccessToken(tokens.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, "t_second", claims.TenantID)
	assert.Equal(t, "editor", claims.TenantRole)

	// The new refresh token is bound to the switched tenant (F2), so a later
	// refresh keeps the user on t_second.
	refreshed, err := svc.Refresh(context.Background(), tokens.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, "t_second", refreshed.TenantID)

	// Switching must not kill parallel sessions: the pre-switch refresh token
	// is untouched and still refreshes into its own tenant (t_first).
	assert.False(t, repo.refreshes[preSwitchHash].revoked, "switch must not revoke other sessions' refresh tokens")
	preRefreshed, err := svc.Refresh(context.Background(), preSwitch.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, "t_first", preRefreshed.TenantID)
}

func TestSwitchTenant_NotAMember(t *testing.T) {
	svc, _, _, _, userID := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_mine", "Mine", "owner", time.Now())})

	_, err := svc.SwitchTenant(context.Background(), userID, "t_other")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotTenantMember), "got %v", err)
}

func TestSwitchTenant_EmptySlugRejected(t *testing.T) {
	svc, _, _, _, userID := newTestAuth(t, nil, nil)
	_, err := svc.SwitchTenant(context.Background(), userID, "")
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, 400, ae.HTTPStatus)
}

// --- account status gates every minting path ---------------------------------
//
// 'suspended' and 'deleted' are values the users table has always been able to
// hold; until these three paths read them, they were a setting that looked like
// a control. Each test drives one of the three entry points into issueTokens.

func TestLogin_SuspendedAccountRejected(t *testing.T) {
	svc, repo, _, _, _ := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "owner", time.Now())})

	repo.status = "suspended"
	_, err := svc.Login(context.Background(), LoginInput{Email: "a@example.com", Password: testPassword})
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperrors.ErrSuspended), "got %v", err)
	// The correct password was supplied: the refusal is the status check, not
	// credential verification — and no session was left behind.
	assert.Empty(t, repo.refreshes)
}

func TestRefresh_SuspensionAfterIssuanceEndsTheSession(t *testing.T) {
	svc, repo, _, _, _ := newTestAuth(t, nil,
		[]tenantdomain.UserMembership{membership("t_alpha", "Alpha", "owner", time.Now())})

	tokens := login(t, svc)
	repo.status = "suspended" // suspended while holding a valid refresh token

	_, err := svc.Refresh(context.Background(), tokens.RefreshToken)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperrors.ErrSuspended), "got %v", err)

	// The presented token is spent either way, so a retry cannot re-enter.
	_, err = svc.Refresh(context.Background(), tokens.RefreshToken)
	assert.True(t, errors.Is(err, ErrRefreshRevoked), "got %v", err)
}

func TestSwitchTenant_SuspendedAccountRejected(t *testing.T) {
	// SwitchTenant mints a fresh pair for an already-authenticated caller, so an
	// unexpired access token would otherwise extend a suspended user's session
	// indefinitely. This path does not exist upstream of the funnel check.
	svc, repo, _, _, userID := newTestAuth(t, nil, []tenantdomain.UserMembership{
		membership("t_alpha", "Alpha", "owner", time.Now().Add(-time.Hour)),
		membership("t_beta", "Beta", "editor", time.Now()),
	})

	repo.status = "suspended"
	_, err := svc.SwitchTenant(context.Background(), userID, "t_beta")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperrors.ErrSuspended), "got %v", err)
}
