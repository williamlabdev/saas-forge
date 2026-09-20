// Package service implements tenant membership use cases (TKT-R1 PR-invite):
// inviting a user into an existing tenant and accepting such an invite.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/identity"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	"github.com/williamlabdev/saas-forge/internal/tenant/domain"
	"github.com/williamlabdev/saas-forge/internal/tenant/repository"
)

// inviteTTL bounds how long a pending invite stays acceptable. Constant for
// now; promote to config if a product need appears.
const inviteTTL = 7 * 24 * time.Hour

var errNoActiveTenant = apperrors.New("TENANT_NO_ACTIVE", "inviting requires an active tenant", 403)

// TenantService exposes tenant membership use cases.
type TenantService interface {
	CreateInvite(ctx context.Context, in CreateInviteInput) (*InviteDTO, error)
	AcceptInvite(ctx context.Context, token string) (*AcceptedInviteDTO, error)

	// --- 2.2c: member management (ADR-022) ------------------------------

	ListMembers(ctx context.Context) (*MembersListDTO, error)
	UpdateMemberRole(ctx context.Context, targetUserID uuid.UUID, in UpdateMemberRoleInput) (*MemberDTO, error)
	RemoveMember(ctx context.Context, targetUserID uuid.UUID) error
	ListInvites(ctx context.Context) (*PendingInvitesDTO, error)
	RevokeInvite(ctx context.Context, inviteID uuid.UUID) error
}

type CreateInviteInput struct {
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role" validate:"required"`
}

// InviteDTO is returned once at creation; Token is the raw secret and is
// never retrievable again (only its hash is stored).
type InviteDTO struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Role      string    `json:"role"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AcceptedInviteDTO struct {
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"tenant_name"`
	Role       string `json:"role"`
}

// --- 2.2c: member management (ADR-022) --------------------------------------

// MemberDTO is one row of GET /tenants/members.
type MemberDTO struct {
	UserID   string    `json:"user_id"`
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// MembersListDTO wraps the roster in an "items" envelope (no pagination
// convention exists elsewhere in this API yet; the repository caps the
// underlying query at memberListCap — see ADR-022).
type MembersListDTO struct {
	Items []MemberDTO `json:"items"`
}

type UpdateMemberRoleInput struct {
	Role string `json:"role" validate:"required"`
}

// PendingInviteDTO is one row of GET /tenants/invites. The raw token is
// never part of this — or any — read path.
type PendingInviteDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedBy string    `json:"created_by"`
	Status    string    `json:"status"`
}

type PendingInvitesDTO struct {
	Items []PendingInviteDTO `json:"items"`
}

// MemberEmailLookup resolves a member's plaintext email for the roster
// endpoint. Narrow on purpose: the tenant module must not import the user
// module's domain types (module-boundary rule) — this asks only for what
// the roster needs, letting the composition root (app.go) supply an adapter
// over the user repository.
type MemberEmailLookup interface {
	EmailByID(ctx context.Context, userID uuid.UUID) (string, error)
}

// RefreshTokenRevoker lets member removal evict the removed user's
// still-valid refresh tokens FOR THIS TENANT ONLY (2.2c). Best-effort: a
// failure here does not roll back the membership removal — the membership
// row is the authority on access, and a revoke failure only means the
// documented residual-access window (ADR-022) lasts the full access-token
// TTL instead of ending at the next refresh attempt.
type RefreshTokenRevoker interface {
	RevokeAllForUserInTenant(ctx context.Context, userID uuid.UUID, tenantSlug string) error
}

type tenantService struct {
	repo   repository.TenantRepository
	idx    crypto.BlindIndexer
	enc    crypto.FieldEncryptor
	authz  authz.Authorizer
	emails MemberEmailLookup
	tokens RefreshTokenRevoker
}

func NewTenantService(
	repo repository.TenantRepository,
	idx crypto.BlindIndexer,
	enc crypto.FieldEncryptor,
	authorizer authz.Authorizer,
	emails MemberEmailLookup,
	tokens RefreshTokenRevoker,
) TenantService {
	return &tenantService{repo: repo, idx: idx, enc: enc, authz: authorizer, emails: emails, tokens: tokens}
}

func (s *tenantService) CreateInvite(ctx context.Context, in CreateInviteInput) (*InviteDTO, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return nil, apperrors.ErrUnauthorized
	}
	// The invite is always for the caller's ACTIVE tenant — the API takes no
	// tenant parameter, same trust posture as content (subject.TenantID only).
	if sub.TenantID == "" {
		return nil, errNoActiveTenant
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantInviteCreate,
		Resource: authz.Resource{Type: "tenant", ID: sub.TenantID},
	}); err != nil {
		return nil, err
	}
	// Member management mints durable state (a 7-day invite), so unlike
	// content verbs it does NOT trust the ≤15-min JWT role alone: re-check the
	// membership fresh from the DB. A demoted admin's residual token cannot
	// mint invites. (Accept-time re-checks the inviter again — see repo.)
	liveRole, err := s.repo.MembershipRole(ctx, sub.UserID, sub.TenantID)
	if err != nil {
		if apperrors.Is(err, apperrors.ErrNotFound) {
			return nil, apperrors.ErrForbidden
		}
		return nil, err
	}
	if liveRole != domain.RoleOwner && liveRole != domain.RoleAdmin {
		return nil, apperrors.ErrForbidden
	}
	if err := validate.Struct(in); err != nil {
		return nil, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	if !domain.InvitableRole(in.Role) {
		return nil, apperrors.New("INVITE_ROLE_INVALID", "role must be admin, editor, or viewer", 422)
	}

	email := identity.NormalizeEmail(in.Email)
	emailHash, err := s.idx.Index(email)
	if err != nil {
		return nil, err
	}
	// 2.2c/ADR-022: also keep a reversibly encrypted copy of the address.
	// The blind index alone can test equality but never be decrypted, and
	// GET /tenants/invites needs to SHOW what address a pending invite was
	// sent to — this closes that schema gap (migration 000048).
	emailEnc, emailNonce, err := s.enc.Encrypt([]byte(email))
	if err != nil {
		return nil, err
	}
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}

	rawToken, err := newInviteToken()
	if err != nil {
		return nil, err
	}
	inv := domain.Invite{
		ID:                  uuid.New(),
		TenantID:            tenant.ID,
		EmailLookupHash:     emailHash,
		EmailEncrypted:      emailEnc,
		EmailEncryptedNonce: emailNonce,
		Role:                in.Role,
		TokenHash:           hashInviteToken(rawToken),
		InvitedBy:           sub.UserID,
		ExpiresAt:           time.Now().UTC().Add(inviteTTL),
	}
	if err := s.repo.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	return &InviteDTO{
		ID:        inv.ID.String(),
		TenantID:  tenant.Slug,
		Role:      inv.Role,
		Token:     rawToken,
		ExpiresAt: inv.ExpiresAt,
	}, nil
}

func (s *tenantService) AcceptInvite(ctx context.Context, token string) (*AcceptedInviteDTO, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return nil, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantInviteAccept,
		Resource: authz.Resource{Type: "tenant", ID: "invite"},
	}); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, apperrors.New("VALIDATION_FAILED", "token is required", 400)
	}
	accepted, err := s.repo.AcceptInvite(ctx, hashInviteToken(token), sub.UserID)
	if err != nil {
		return nil, err
	}
	return &AcceptedInviteDTO{
		TenantID:   accepted.TenantSlug,
		TenantName: accepted.TenantName,
		Role:       accepted.Role,
	}, nil
}

// --- 2.2c: member management (ADR-022) --------------------------------------

// activeTenant resolves the caller's subject + active tenant, in the shape
// every member-management method needs before it can do anything else.
func (s *tenantService) activeTenant(ctx context.Context) (authn.Subject, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return authn.Subject{}, apperrors.ErrUnauthorized
	}
	if sub.TenantID == "" {
		return authn.Subject{}, errNoActiveTenant
	}
	return sub, nil
}

// requireLiveOwnerOrAdmin re-checks the caller's CURRENT membership role
// against the DB rather than trusting the (≤15-min) JWT role alone. Member
// management mints/revokes durable privilege, so — same reasoning as
// CreateInvite — a recently demoted owner/admin's still-valid access token
// must not be able to act here during its remaining TTL.
func (s *tenantService) requireLiveOwnerOrAdmin(ctx context.Context, sub authn.Subject) (string, error) {
	liveRole, err := s.repo.MembershipRole(ctx, sub.UserID, sub.TenantID)
	if err != nil {
		if apperrors.Is(err, apperrors.ErrNotFound) {
			return "", apperrors.ErrForbidden
		}
		return "", err
	}
	if liveRole != domain.RoleOwner && liveRole != domain.RoleAdmin {
		return "", apperrors.ErrForbidden
	}
	return liveRole, nil
}

func (s *tenantService) ListMembers(ctx context.Context) (*MembersListDTO, error) {
	sub, err := s.activeTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantMemberList,
		Resource: authz.Resource{Type: "tenant", ID: sub.TenantID},
	}); err != nil {
		return nil, err
	}
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	members, err := s.repo.Members(ctx, tenant.ID)
	if err != nil {
		return nil, err
	}
	items := make([]MemberDTO, 0, len(members))
	for _, m := range members {
		email, err := s.emails.EmailByID(ctx, m.UserID)
		if err != nil {
			return nil, err
		}
		items = append(items, MemberDTO{
			UserID:   m.UserID.String(),
			Email:    email,
			Role:     m.Role,
			JoinedAt: m.JoinedAt,
		})
	}
	return &MembersListDTO{Items: items}, nil
}

func (s *tenantService) UpdateMemberRole(ctx context.Context, targetUserID uuid.UUID, in UpdateMemberRoleInput) (*MemberDTO, error) {
	sub, err := s.activeTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantMemberUpdate,
		Resource: authz.Resource{Type: "tenant", ID: sub.TenantID},
	}); err != nil {
		return nil, err
	}
	liveRole, err := s.requireLiveOwnerOrAdmin(ctx, sub)
	if err != nil {
		return nil, err
	}
	if err := validate.Struct(in); err != nil {
		return nil, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	if !domain.ValidRole(in.Role) {
		return nil, domain.ErrRoleInvalid
	}
	// Self-operation is refused before we even look at the target's current
	// role — 409, not 403 (see domain.ErrSelfRoleChange doc comment): the
	// caller IS generally authorized to change member roles, just not their
	// own, through this endpoint.
	if targetUserID == sub.UserID {
		return nil, domain.ErrSelfRoleChange
	}
	// Only an owner may move the owner role onto or off of anyone — an admin
	// assigning "owner", or an admin touching a member who currently IS
	// owner, are both refused here (see domain.ErrCannotAssignOwner).
	if liveRole == domain.RoleAdmin {
		targetCurrentRole, err := s.repo.MembershipRole(ctx, targetUserID, sub.TenantID)
		if err != nil {
			return nil, err
		}
		if in.Role == domain.RoleOwner || targetCurrentRole == domain.RoleOwner {
			return nil, domain.ErrCannotAssignOwner
		}
	}
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	// Last-owner protection is enforced atomically inside the repository
	// (row-locked COUNT) — see PostgresTenantRepository.UpdateMemberRole —
	// rather than re-derived here, so a race between two demotions cannot
	// both "succeed" in the gap between a read and a write.
	m, err := s.repo.UpdateMemberRole(ctx, tenant.ID, targetUserID, in.Role)
	if err != nil {
		return nil, err
	}
	email, err := s.emails.EmailByID(ctx, m.UserID)
	if err != nil {
		return nil, err
	}
	return &MemberDTO{
		UserID:   m.UserID.String(),
		Email:    email,
		Role:     m.Role,
		JoinedAt: m.JoinedAt,
	}, nil
}

func (s *tenantService) RemoveMember(ctx context.Context, targetUserID uuid.UUID) error {
	sub, err := s.activeTenant(ctx)
	if err != nil {
		return err
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantMemberRemove,
		Resource: authz.Resource{Type: "tenant", ID: sub.TenantID},
	}); err != nil {
		return err
	}
	liveRole, err := s.requireLiveOwnerOrAdmin(ctx, sub)
	if err != nil {
		return err
	}
	// 409, not 403 — see domain.ErrSelfRemove: leaving a tenant is a
	// distinct, out-of-scope feature, not a special case of this endpoint.
	if targetUserID == sub.UserID {
		return domain.ErrSelfRemove
	}
	if liveRole == domain.RoleAdmin {
		targetCurrentRole, err := s.repo.MembershipRole(ctx, targetUserID, sub.TenantID)
		if err != nil {
			return err
		}
		if targetCurrentRole == domain.RoleOwner {
			return domain.ErrCannotAssignOwner
		}
	}
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return err
	}
	if err := s.repo.RemoveMember(ctx, tenant.ID, targetUserID); err != nil {
		return err
	}
	// Best-effort: evict the removed member's refresh tokens for THIS
	// tenant only, so their next token refresh (not just their already-
	// issued access token) is rejected immediately rather than at its
	// natural TTL. See RefreshTokenRevoker doc comment and ADR-022 for the
	// residual-access-token limitation this does NOT close.
	_ = s.tokens.RevokeAllForUserInTenant(ctx, targetUserID, sub.TenantID)
	return nil
}

func (s *tenantService) ListInvites(ctx context.Context) (*PendingInvitesDTO, error) {
	sub, err := s.activeTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantInviteList,
		Resource: authz.Resource{Type: "tenant", ID: sub.TenantID},
	}); err != nil {
		return nil, err
	}
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	invites, err := s.repo.ListInvites(ctx, tenant.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	items := make([]PendingInviteDTO, 0, len(invites))
	for _, inv := range invites {
		emailPlain, err := s.enc.Decrypt(inv.EmailEncrypted, inv.EmailEncryptedNonce)
		if err != nil {
			return nil, err
		}
		status := domain.InviteStatusPending
		if inv.ExpiresAt.Before(now) {
			status = domain.InviteStatusExpired
		}
		items = append(items, PendingInviteDTO{
			ID:        inv.ID.String(),
			Email:     string(emailPlain),
			Role:      inv.Role,
			ExpiresAt: inv.ExpiresAt,
			CreatedBy: inv.InvitedBy.String(),
			Status:    status,
		})
	}
	return &PendingInvitesDTO{Items: items}, nil
}

func (s *tenantService) RevokeInvite(ctx context.Context, inviteID uuid.UUID) error {
	sub, err := s.activeTenant(ctx)
	if err != nil {
		return err
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionTenantInviteRevoke,
		Resource: authz.Resource{Type: "tenant", ID: sub.TenantID},
	}); err != nil {
		return err
	}
	if _, err := s.requireLiveOwnerOrAdmin(ctx, sub); err != nil {
		return err
	}
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return err
	}
	return s.repo.RevokeInvite(ctx, tenant.ID, inviteID, sub.UserID)
}

// newInviteToken returns 32 bytes of crypto/rand as hex — same strength and
// shape as refresh tokens.
func newInviteToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashInviteToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
