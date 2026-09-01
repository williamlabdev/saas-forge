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

type tenantService struct {
	repo  repository.TenantRepository
	idx   crypto.BlindIndexer
	authz authz.Authorizer
}

func NewTenantService(repo repository.TenantRepository, idx crypto.BlindIndexer, authorizer authz.Authorizer) TenantService {
	return &tenantService{repo: repo, idx: idx, authz: authorizer}
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
	tenant, err := s.repo.TenantBySlug(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}

	rawToken, err := newInviteToken()
	if err != nil {
		return nil, err
	}
	inv := domain.Invite{
		ID:              uuid.New(),
		TenantID:        tenant.ID,
		EmailLookupHash: emailHash,
		Role:            in.Role,
		TokenHash:       hashInviteToken(rawToken),
		InvitedBy:       sub.UserID,
		ExpiresAt:       time.Now().UTC().Add(inviteTTL),
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
