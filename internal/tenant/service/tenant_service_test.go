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

// fakeMember is one roster row, tracked by pointer so UpdateMemberRole can
// mutate it in place (mirroring the real repo's UPDATE).
type fakeMember struct {
	userID   uuid.UUID
	role     string
	email    string
	joinedAt time.Time
}

type fakeRepo struct {
	tenants   map[string]domain.Tenant // slug → tenant
	liveRoles map[string]string        // slug → the caller's LIVE membership role (legacy default, pre-2.2c)
	invites   []domain.Invite
	accepted  map[string]domain.AcceptedInvite // tokenHash → result
	members   map[string][]*fakeMember         // slug → roster (2.2c)

	// revoked records every RevokeAllForUserInTenant call, so tests can
	// assert member removal reaches the token layer (best-effort, see
	// tenantService.RemoveMember).
	revoked []struct {
		userID uuid.UUID
		slug   string
	}
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		tenants:   map[string]domain.Tenant{},
		liveRoles: map[string]string{},
		accepted:  map[string]domain.AcceptedInvite{},
		members:   map[string][]*fakeMember{},
	}
}

// addMember seeds a roster row for tenant slug AND keeps MembershipRole
// answering for that specific user — the single source of truth 2.2c tests
// need (who is caller vs. target vs. the tenant's other members).
func (f *fakeRepo) addMember(slug string, userID uuid.UUID, role, email string) {
	f.members[slug] = append(f.members[slug], &fakeMember{userID: userID, role: role, email: email, joinedAt: time.Now().UTC()})
}

func (f *fakeRepo) slugByTenantID(id uuid.UUID) (string, bool) {
	for slug, t := range f.tenants {
		if t.ID == id {
			return slug, true
		}
	}
	return "", false
}

func (f *fakeRepo) MembershipsForUser(context.Context, uuid.UUID) ([]domain.UserMembership, error) {
	return nil, nil
}
func (f *fakeRepo) MembershipRole(_ context.Context, userID uuid.UUID, slug string) (string, error) {
	for _, m := range f.members[slug] {
		if m.userID == userID {
			return m.role, nil
		}
	}
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
// AcceptInvite checks invites created via CreateInvite/seeded into f.invites
// first (so RevokeInvite → AcceptInvite round-trips exercise the real
// revoked/used/expired ordering the Postgres repository implements), then
// falls back to the legacy f.accepted map older tests seed directly.
func (f *fakeRepo) AcceptInvite(_ context.Context, tokenHash string, userID uuid.UUID) (domain.AcceptedInvite, error) {
	for i := range f.invites {
		inv := &f.invites[i]
		if inv.TokenHash != tokenHash {
			continue
		}
		if inv.AcceptedAt != nil {
			return domain.AcceptedInvite{}, domain.ErrInviteUsed
		}
		// Revoked is checked BEFORE expiry — same deliberate ordering as
		// PostgresTenantRepository.AcceptInvite: a deliberate cancellation is
		// a more specific, actionable reason than "time ran out".
		if inv.RevokedAt != nil {
			return domain.AcceptedInvite{}, domain.ErrInviteRevoked
		}
		if inv.ExpiresAt.Before(time.Now().UTC()) {
			return domain.AcceptedInvite{}, domain.ErrInviteExpired
		}
		now := time.Now().UTC()
		inv.AcceptedAt = &now
		inv.AcceptedBy = &userID
		slug, _ := f.slugByTenantID(inv.TenantID)
		tenant := f.tenants[slug]
		f.addMember(slug, userID, inv.Role, "")
		return domain.AcceptedInvite{TenantSlug: slug, TenantName: tenant.Name, Role: inv.Role}, nil
	}
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

// --- 2.2c: member management fake -------------------------------------------

func (f *fakeRepo) Members(_ context.Context, tenantID uuid.UUID) ([]domain.Member, error) {
	slug, ok := f.slugByTenantID(tenantID)
	if !ok {
		return nil, nil
	}
	out := make([]domain.Member, 0, len(f.members[slug]))
	for _, m := range f.members[slug] {
		out = append(out, domain.Member{UserID: m.userID, Role: m.role, JoinedAt: m.joinedAt})
	}
	return out, nil
}

func (f *fakeRepo) UpdateMemberRole(_ context.Context, tenantID, targetUserID uuid.UUID, newRole string) (domain.Member, error) {
	slug, ok := f.slugByTenantID(tenantID)
	if !ok {
		return domain.Member{}, apperrors.ErrNotFound
	}
	roster := f.members[slug]
	var target *fakeMember
	for _, m := range roster {
		if m.userID == targetUserID {
			target = m
			break
		}
	}
	if target == nil {
		return domain.Member{}, apperrors.ErrNotFound
	}
	if target.role == domain.RoleOwner && newRole != domain.RoleOwner && countOwners(roster) <= 1 {
		return domain.Member{}, domain.ErrLastOwner
	}
	target.role = newRole
	return domain.Member{UserID: target.userID, Role: target.role, JoinedAt: target.joinedAt}, nil
}

func (f *fakeRepo) RemoveMember(_ context.Context, tenantID, targetUserID uuid.UUID) error {
	slug, ok := f.slugByTenantID(tenantID)
	if !ok {
		return apperrors.ErrNotFound
	}
	roster := f.members[slug]
	idx := -1
	for i, m := range roster {
		if m.userID == targetUserID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return apperrors.ErrNotFound
	}
	if roster[idx].role == domain.RoleOwner && countOwners(roster) <= 1 {
		return domain.ErrLastOwner
	}
	f.members[slug] = append(roster[:idx], roster[idx+1:]...)
	return nil
}

func countOwners(roster []*fakeMember) int {
	n := 0
	for _, m := range roster {
		if m.role == domain.RoleOwner {
			n++
		}
	}
	return n
}

func (f *fakeRepo) ListInvites(_ context.Context, tenantID uuid.UUID) ([]domain.Invite, error) {
	out := make([]domain.Invite, 0)
	for _, inv := range f.invites {
		if inv.TenantID == tenantID && inv.AcceptedAt == nil && inv.RevokedAt == nil {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (f *fakeRepo) RevokeInvite(_ context.Context, tenantID, inviteID, revokedBy uuid.UUID) error {
	for i := range f.invites {
		inv := &f.invites[i]
		if inv.TenantID != tenantID || inv.ID != inviteID {
			continue
		}
		if inv.AcceptedAt != nil {
			return domain.ErrInviteUsed
		}
		if inv.RevokedAt != nil {
			return nil // idempotent no-op, same as the real repository
		}
		now := time.Now().UTC()
		inv.RevokedAt = &now
		inv.RevokedBy = &revokedBy
		return nil
	}
	return domain.ErrInviteNotFound
}

// EmailByID and RevokeAllForUserInTenant let *fakeRepo double as both
// service.MemberEmailLookup and service.RefreshTokenRevoker — no separate
// fake types needed, since the roster it already tracks holds everything
// both interfaces need.
func (f *fakeRepo) EmailByID(_ context.Context, userID uuid.UUID) (string, error) {
	for _, roster := range f.members {
		for _, m := range roster {
			if m.userID == userID {
				return m.email, nil
			}
		}
	}
	return "", apperrors.ErrNotFound
}

func (f *fakeRepo) RevokeAllForUserInTenant(_ context.Context, userID uuid.UUID, slug string) error {
	f.revoked = append(f.revoked, struct {
		userID uuid.UUID
		slug   string
	}{userID, slug})
	return nil
}

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

func testEncryptor(t *testing.T) crypto.FieldEncryptor {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0xab
	}
	enc, err := crypto.NewAESGCMEncryptor(b)
	require.NoError(t, err)
	return enc
}

func subjectCtx(role, tenant string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: tenant, TenantRole: role,
	})
}

// subjectCtxFor is subjectCtx with a caller-chosen UserID — 2.2c tests need
// deterministic identities to tell caller, target, and "the tenant's other
// owner" apart in a shared fakeRepo roster.
func subjectCtxFor(userID uuid.UUID, role, tenant string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID: userID, TenantID: tenant, TenantRole: role,
	})
}

func newSvc(t *testing.T, repo *fakeRepo) TenantService {
	t.Helper()
	return NewTenantService(repo, testIndexer(t), testEncryptor(t), authz.NewRBACAuthorizer(), repo, repo)
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

// --- 2.2c: member management (ADR-022) --------------------------------------

func TestListMembers_IncludesEmail(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	owner := uuid.New()
	other := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	repo.addMember("t_x", other, domain.RoleEditor, "editor@example.com")
	svc := newSvc(t, repo)

	dto, err := svc.ListMembers(subjectCtxFor(owner, "owner", "t_x"))
	require.NoError(t, err)
	require.Len(t, dto.Items, 2)
	byUser := map[string]MemberDTO{}
	for _, m := range dto.Items {
		byUser[m.UserID] = m
	}
	assert.Equal(t, "owner@example.com", byUser[owner.String()].Email)
	assert.Equal(t, "editor@example.com", byUser[other.String()].Email)
}

func TestListMembers_ViewerAllowedEditorRoleRulesOutOthers(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	viewer := uuid.New()
	repo.addMember("t_x", viewer, domain.RoleViewer, "viewer@example.com")
	svc := newSvc(t, repo)

	// All four D3 roles may list members (unlike invite/manage endpoints).
	_, err := svc.ListMembers(subjectCtxFor(viewer, "viewer", "t_x"))
	require.NoError(t, err)

	_, err = svc.ListMembers(subjectCtx("owner", ""))
	require.ErrorIs(t, err, errNoActiveTenant)
	_, err = svc.ListMembers(context.Background())
	require.ErrorIs(t, err, apperrors.ErrUnauthorized)
}

func TestUpdateMemberRole_AdminChangesEditorToViewer_Succeeds(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	admin := uuid.New()
	target := uuid.New()
	repo.addMember("t_x", admin, domain.RoleAdmin, "admin@example.com")
	repo.addMember("t_x", target, domain.RoleEditor, "editor@example.com")
	svc := newSvc(t, repo)

	dto, err := svc.UpdateMemberRole(subjectCtxFor(admin, "admin", "t_x"), target, UpdateMemberRoleInput{Role: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, "viewer", dto.Role)
	assert.Equal(t, "editor@example.com", dto.Email)
}

func TestUpdateMemberRole_AdminCannotAssignOwner(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	admin := uuid.New()
	target := uuid.New()
	repo.addMember("t_x", admin, domain.RoleAdmin, "admin@example.com")
	repo.addMember("t_x", target, domain.RoleEditor, "editor@example.com")
	svc := newSvc(t, repo)

	_, err := svc.UpdateMemberRole(subjectCtxFor(admin, "admin", "t_x"), target, UpdateMemberRoleInput{Role: "owner"})
	require.ErrorIs(t, err, domain.ErrCannotAssignOwner)
	appErr, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, 403, appErr.HTTPStatus)
}

func TestUpdateMemberRole_AdminCannotDemoteOwner(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	admin := uuid.New()
	owner := uuid.New()
	repo.addMember("t_x", admin, domain.RoleAdmin, "admin@example.com")
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	svc := newSvc(t, repo)

	_, err := svc.UpdateMemberRole(subjectCtxFor(admin, "admin", "t_x"), owner, UpdateMemberRoleInput{Role: "editor"})
	require.ErrorIs(t, err, domain.ErrCannotAssignOwner)
}

func TestUpdateMemberRole_LastOwnerSelfDemote_Conflict(t *testing.T) {
	// The sole owner tries to demote themself. Refused 409 — but via
	// ErrSelfRoleChange, not ErrLastOwner: self-operation is refused
	// categorically by this endpoint before ownership count is even
	// consulted (see tenantService.UpdateMemberRole and ADR-022). Both
	// sentinels carry HTTP 409, so either would satisfy "self-demoting the
	// last owner must fail with a conflict" — this test asserts which ONE
	// actually fires, since that is the more specific, actionable code.
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	owner := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	svc := newSvc(t, repo)

	_, err := svc.UpdateMemberRole(subjectCtxFor(owner, "owner", "t_x"), owner, UpdateMemberRoleInput{Role: "admin"})
	require.ErrorIs(t, err, domain.ErrSelfRoleChange)
	appErr, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, 409, appErr.HTTPStatus)
}

func TestUpdateMemberRole_RepositoryLastOwnerGuard(t *testing.T) {
	// Exercise the repository's last-owner guard directly, with a single
	// owner acting on themself (bypassing the service's self-check). This
	// is NOT the only HTTP-reachable path to ErrLastOwner — see ADR-022
	// §3/§3b: two owners concurrently demoting/removing EACH OTHER also
	// reach it, since that path isn't intercepted by either the self-check
	// or the admin-cannot-touch-owner check (see
	// TestIntegration_UpdateMemberRole_ConcurrentOwnersDemoteEachOther for
	// that path, and PATCH…/members/{user_id} in the handbook for the
	// user-facing description). This test just isolates the guard's own
	// correctness from the concurrency and self-check machinery around it.
	repo := newFakeRepo()
	tenantID := uuid.New()
	repo.tenants["t_x"] = domain.Tenant{ID: tenantID, Slug: "t_x"}
	owner := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")

	_, err := repo.UpdateMemberRole(context.Background(), tenantID, owner, domain.RoleAdmin)
	require.ErrorIs(t, err, domain.ErrLastOwner)

	err = repo.RemoveMember(context.Background(), tenantID, owner)
	require.ErrorIs(t, err, domain.ErrLastOwner)
}

func TestUpdateMemberRole_EditorForbidden(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	editor := uuid.New()
	target := uuid.New()
	repo.addMember("t_x", editor, domain.RoleEditor, "editor@example.com")
	repo.addMember("t_x", target, domain.RoleViewer, "viewer@example.com")
	svc := newSvc(t, repo)

	_, err := svc.UpdateMemberRole(subjectCtxFor(editor, "editor", "t_x"), target, UpdateMemberRoleInput{Role: "editor"})
	require.ErrorIs(t, err, apperrors.ErrForbidden)
}

func TestRemoveMember_AdminRemovesEditor_Succeeds(t *testing.T) {
	repo := newFakeRepo()
	tenantID := uuid.New()
	repo.tenants["t_x"] = domain.Tenant{ID: tenantID, Slug: "t_x"}
	admin := uuid.New()
	target := uuid.New()
	repo.addMember("t_x", admin, domain.RoleAdmin, "admin@example.com")
	repo.addMember("t_x", target, domain.RoleEditor, "editor@example.com")
	svc := newSvc(t, repo)

	err := svc.RemoveMember(subjectCtxFor(admin, "admin", "t_x"), target)
	require.NoError(t, err)
	assert.Len(t, repo.members["t_x"], 1, "target removed from roster")
	require.Len(t, repo.revoked, 1, "best-effort token revoke reached the token layer")
	assert.Equal(t, target, repo.revoked[0].userID)
	assert.Equal(t, "t_x", repo.revoked[0].slug)
}

func TestRemoveMember_AdminCannotRemoveOwner(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	admin := uuid.New()
	owner := uuid.New()
	repo.addMember("t_x", admin, domain.RoleAdmin, "admin@example.com")
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	svc := newSvc(t, repo)

	err := svc.RemoveMember(subjectCtxFor(admin, "admin", "t_x"), owner)
	require.ErrorIs(t, err, domain.ErrCannotAssignOwner)
}

func TestRemoveMember_CannotRemoveSelf(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	owner := uuid.New()
	other := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	repo.addMember("t_x", other, domain.RoleAdmin, "admin@example.com")
	svc := newSvc(t, repo)

	err := svc.RemoveMember(subjectCtxFor(owner, "owner", "t_x"), owner)
	require.ErrorIs(t, err, domain.ErrSelfRemove)
	appErr, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, 409, appErr.HTTPStatus)
}

func TestRemoveMember_EditorForbidden(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	editor := uuid.New()
	target := uuid.New()
	repo.addMember("t_x", editor, domain.RoleEditor, "editor@example.com")
	repo.addMember("t_x", target, domain.RoleViewer, "viewer@example.com")
	svc := newSvc(t, repo)

	err := svc.RemoveMember(subjectCtxFor(editor, "editor", "t_x"), target)
	require.ErrorIs(t, err, apperrors.ErrForbidden)
}

func TestListInvites_OwnerSeesEmailNeverToken(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	owner := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	svc := newSvc(t, repo)

	created, err := svc.CreateInvite(subjectCtxFor(owner, "owner", "t_x"), CreateInviteInput{Email: "invitee@example.com", Role: "viewer"})
	require.NoError(t, err)
	require.NotEmpty(t, created.Token)

	list, err := svc.ListInvites(subjectCtxFor(owner, "owner", "t_x"))
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	item := list.Items[0]
	assert.Equal(t, "invitee@example.com", item.Email, "email is decrypted for display")
	assert.Equal(t, domain.InviteStatusPending, item.Status)
	// PendingInviteDTO has no Token field at all — the raw token is
	// structurally impossible to leak through this read path (see the type
	// definition), not merely omitted by convention.
}

func TestListInvites_EditorForbidden(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	editor := uuid.New()
	repo.addMember("t_x", editor, domain.RoleEditor, "editor@example.com")
	svc := newSvc(t, repo)

	_, err := svc.ListInvites(subjectCtxFor(editor, "editor", "t_x"))
	require.ErrorIs(t, err, apperrors.ErrForbidden)
}

func TestRevokeInvite_ThenAccept_Returns410(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x", Name: "X"}
	owner := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	svc := newSvc(t, repo)

	created, err := svc.CreateInvite(subjectCtxFor(owner, "owner", "t_x"), CreateInviteInput{Email: "invitee@example.com", Role: "viewer"})
	require.NoError(t, err)
	inviteID := repo.invites[0].ID

	err = svc.RevokeInvite(subjectCtxFor(owner, "owner", "t_x"), inviteID)
	require.NoError(t, err)

	// Idempotent: revoking again is still a success, not an error.
	err = svc.RevokeInvite(subjectCtxFor(owner, "owner", "t_x"), inviteID)
	require.NoError(t, err)

	// A revoked invite no longer appears in the pending list.
	list, err := svc.ListInvites(subjectCtxFor(owner, "owner", "t_x"))
	require.NoError(t, err)
	assert.Empty(t, list.Items)

	_, err = svc.AcceptInvite(subjectCtx("", ""), created.Token)
	require.ErrorIs(t, err, domain.ErrInviteRevoked)
	appErr, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, 410, appErr.HTTPStatus)
}

func TestRevokeInvite_EditorForbidden(t *testing.T) {
	repo := newFakeRepo()
	repo.tenants["t_x"] = domain.Tenant{ID: uuid.New(), Slug: "t_x"}
	owner := uuid.New()
	editor := uuid.New()
	repo.addMember("t_x", owner, domain.RoleOwner, "owner@example.com")
	repo.addMember("t_x", editor, domain.RoleEditor, "editor@example.com")
	svc := newSvc(t, repo)

	created, err := svc.CreateInvite(subjectCtxFor(owner, "owner", "t_x"), CreateInviteInput{Email: "invitee@example.com", Role: "viewer"})
	require.NoError(t, err)
	inviteID := repo.invites[0].ID

	err = svc.RevokeInvite(subjectCtxFor(editor, "editor", "t_x"), inviteID)
	require.ErrorIs(t, err, apperrors.ErrForbidden)
	_ = created
}
