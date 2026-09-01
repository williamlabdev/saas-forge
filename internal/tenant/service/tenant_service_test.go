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

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

type fakeRepo struct {
	tenants   map[string]domain.Tenant // slug → tenant
	liveRoles map[string]string        // slug → the caller's LIVE membership role
	invites   []domain.Invite
	accepted  map[string]domain.AcceptedInvite // tokenHash → result
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		tenants:   map[string]domain.Tenant{},
		liveRoles: map[string]string{},
		accepted:  map[string]domain.AcceptedInvite{},
	}
}

func (f *fakeRepo) MembershipsForUser(context.Context, uuid.UUID) ([]domain.UserMembership, error) {
	return nil, nil
}
func (f *fakeRepo) MembershipRole(_ context.Context, _ uuid.UUID, slug string) (string, error) {
	role, ok := f.liveRoles[slug]
	if !ok {
		return "", apperrors.ErrNotFound
	}
	return role, nil
}
func (f *fakeRepo) TenantBySlug(_ context.Context, slug string) (domain.Tenant, error) {
	t, ok := f.tenants[slug]
	if !ok {
		return domain.Tenant{}, apperrors.ErrNotFound
	}
	return t, nil
}
func (f *fakeRepo) ProvisionOwnerTx(context.Context, pgx.Tx, uuid.UUID) (string, error) {
	return "", errors.New("unused")
}
func (f *fakeRepo) CreateInvite(_ context.Context, inv domain.Invite) error {
	f.invites = append(f.invites, inv)
	return nil
}
func (f *fakeRepo) AcceptInvite(_ context.Context, tokenHash string, _ uuid.UUID) (domain.AcceptedInvite, error) {
	res, ok := f.accepted[tokenHash]
	if !ok {
		return domain.AcceptedInvite{}, domain.ErrInviteNotFound
	}
	return res, nil
}

func (f *fakeRepo) PlanForTenant(context.Context, string) (domain.Plan, error) {
	return domain.Plan{Name: domain.DefaultPlanName}, nil
}

func (f *fakeRepo) SetTenantPlan(context.Context, string, string) error { return nil }

func testIndexer(t *testing.T) crypto.BlindIndexer {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0xcd
	}
	idx, err := crypto.NewHMACBlindIndexer(b)
	require.NoError(t, err)
	return idx
}

func subjectCtx(role, tenant string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: tenant, TenantRole: role,
	})
}

func newSvc(t *testing.T, repo *fakeRepo) TenantService {
	t.Helper()
	return NewTenantService(repo, testIndexer(t), authz.NewRBACAuthorizer())
}

func TestCreateInvite_OwnerCreates(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x", Name: "X"}
	repo.liveRoles["t_x"] = "owner"
	svc := newSvc(t, repo)

	dto, err := svc.CreateInvite(subjectCtx("owner", "t_x"), CreateInviteInput{Email: "new@example.com", Role: "editor"})
	require.NoError(t, err)
	assert.Equal(t, "t_x", dto.TenantID)
	assert.Equal(t, "editor", dto.Role)
	assert.Len(t, dto.Token, 64, "raw token returned once")
	assert.WithinDuration(t, time.Now().Add(inviteTTL), dto.ExpiresAt, time.Minute)

	require.Len(t, repo.invites, 1)
	stored := repo.invites[0]
	assert.NotEqual(t, dto.Token, stored.TokenHash, "only the hash is stored")
	assert.Equal(t, hashInviteToken(dto.Token), stored.TokenHash)
	assert.NotEmpty(t, stored.EmailLookupHash, "email stored as blind index only")
}

func TestCreateInvite_Permissions(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	repo.liveRoles["t_x"] = "owner"
	svc := newSvc(t, repo)
	in := CreateInviteInput{Email: "new@example.com", Role: "viewer"}

	// editor/viewer cannot manage members (D3) — rejected on the JWT role.
	for _, role := range []string{"editor", "viewer", ""} {
		_, err := svc.CreateInvite(subjectCtx(role, "t_x"), in)
		require.Error(t, err, "role %q must not invite", role)
	}
	// no active tenant → explicit rejection.
	_, err := svc.CreateInvite(subjectCtx("owner", ""), in)
	require.ErrorIs(t, err, errNoActiveTenant)
	// unauthenticated → 401.
	_, err = svc.CreateInvite(context.Background(), in)
	require.ErrorIs(t, err, apperrors.ErrUnauthorized)
}

func TestCreateInvite_StaleJWTRoleRejectedByLiveCheck(t *testing.T) {
	// The JWT still says owner, but the live membership is gone (demoted or
	// removed within the token TTL): the fresh DB check must refuse.
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	svc := newSvc(t, repo)
	in := CreateInviteInput{Email: "new@example.com", Role: "viewer"}

	_, err := svc.CreateInvite(subjectCtx("owner", "t_x"), in)
	require.ErrorIs(t, err, apperrors.ErrForbidden)
	require.Empty(t, repo.invites)

	// Demoted to editor live: also refused despite owner JWT.
	repo.liveRoles["t_x"] = "editor"
	_, err = svc.CreateInvite(subjectCtx("owner", "t_x"), in)
	require.ErrorIs(t, err, apperrors.ErrForbidden)
	require.Empty(t, repo.invites)
}

func TestCreateInvite_RejectsOwnerRoleAndBadInput(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	repo.liveRoles["t_x"] = "owner"
	svc := newSvc(t, repo)

	_, err := svc.CreateInvite(subjectCtx("owner", "t_x"), CreateInviteInput{Email: "a@b.com", Role: "owner"})
	require.Error(t, err, "owner is not invitable (transfer flow only)")

	_, err = svc.CreateInvite(subjectCtx("owner", "t_x"), CreateInviteInput{Email: "not-an-email", Role: "viewer"})
	require.Error(t, err)
}

func TestAcceptInvite_PassesThroughRepoResult(t *testing.T) {
	repo := newFakeRepo()
	raw := "deadbeef"
	repo.accepted[hashInviteToken(raw)] = domain.AcceptedInvite{TenantSlug: "t_x", TenantName: "X", Role: "viewer"}
	svc := newSvc(t, repo)

	dto, err := svc.AcceptInvite(subjectCtx("", ""), raw)
	require.NoError(t, err)
	assert.Equal(t, &AcceptedInviteDTO{TenantID: "t_x", TenantName: "X", Role: "viewer"}, dto)

	_, err = svc.AcceptInvite(subjectCtx("", ""), "wrong-token")
	require.ErrorIs(t, err, domain.ErrInviteNotFound)

	_, err = svc.AcceptInvite(context.Background(), raw)
	require.ErrorIs(t, err, apperrors.ErrUnauthorized)

	_, err = svc.AcceptInvite(subjectCtx("", ""), "")
	require.Error(t, err)
}
