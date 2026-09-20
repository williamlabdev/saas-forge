package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/auth/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// AgentCredentialService mints, lists and revokes agent credentials — the
// endpoint half of ADR-013's credential lifecycle (ruled 2026-08-06).
//
// It is a separate service from AuthService rather than four more methods on
// it, for the reason IAMAdminService is separate from IAMService: this is the
// only part of auth that is AUTHORIZED — login and refresh answer to a password
// and a stored token, while these three answer to a tenant role — and folding
// an authorizer into AuthService would put one on the login path, where the
// caller has no subject yet by definition.
type AgentCredentialService interface {
	// Issue mints a credential by downgrading the CALLER'S OWN token, which is
	// why the raw bearer is an argument rather than something recovered from the
	// context. jwt.IssueAgentToken takes the minter's *Claims and refuses
	// anything that is not an ordinary tenant credential — a delivery token, a
	// preview token, another agent. Rebuilding those claims from the Subject
	// would hand that check a reconstruction instead of the token, and the
	// reconstruction is exactly where "an agent may not mint an agent" would go
	// missing.
	Issue(ctx context.Context, bearer string, in IssueAgentCredentialInput) (IssuedAgentCredentialDTO, error)
	List(ctx context.Context) ([]AgentCredentialDTO, error)
	Revoke(ctx context.Context, id uuid.UUID) error
}

// IssueAgentCredentialInput is what the caller chooses. Tenant and principal are
// still copied off the caller's own token and cannot be asked for; the role is
// asked for, but only from the set the caller's own role may grant (補裁 S-1).
type IssueAgentCredentialInput struct {
	AgentID string `json:"agent_id"`
	// TenantRole is the role the credential will carry. It is REQUIRED and it is
	// bounded by the caller's own role (ADR-013 補裁 S-1): owner may grant
	// {admin, editor, viewer}, admin may grant {editor, viewer}, and neither may
	// grant its own role.
	//
	// It is required rather than defaulted for the same reason AllowedTypes is:
	// the only defensible default would be "copy the minter", which is precisely
	// the behaviour this field exists to end — a caller that forgot to choose
	// would get the widest credential on offer instead of the narrowest.
	TenantRole   string   `json:"tenant_role"`
	AllowedTypes []string `json:"allowed_types"`
}

// IssuedAgentCredentialDTO is the ONLY time the token is ever returned. It is
// not stored, so it cannot be shown again — the row keeps the id, not the
// secret.
type IssuedAgentCredentialDTO struct {
	ID           string    `json:"id"`
	Token        string    `json:"token"`
	AgentID      string    `json:"agent_id"`
	AllowedTypes []string  `json:"allowed_types"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// AgentCredentialDTO is one row of the tenant's credential list.
type AgentCredentialDTO struct {
	ID           string     `json:"id"`
	AgentID      string     `json:"agent_id"`
	PrincipalID  string     `json:"principal_id"`
	TenantRole   string     `json:"tenant_role"`
	AllowedTypes []string   `json:"allowed_types"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	// Active is computed here rather than left to the reader, because "not
	// revoked" and "usable" are different questions and the list is read during
	// incidents. A credential that expired last week is not revoked and is not
	// active, and a UI that shows only revoked_at renders it as live.
	Active bool `json:"active"`
}

type agentCredentialService struct {
	repo   repository.AgentCredentialRepository
	signer *jwt.Signer
	authz  authz.Authorizer
	now    func() time.Time
}

func NewAgentCredentialService(
	repo repository.AgentCredentialRepository,
	signer *jwt.Signer,
	authorizer authz.Authorizer,
) AgentCredentialService {
	return &agentCredentialService{repo: repo, signer: signer, authz: authorizer, now: time.Now}
}

func (s *agentCredentialService) Issue(ctx context.Context, bearer string, in IssueAgentCredentialInput) (IssuedAgentCredentialDTO, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return IssuedAgentCredentialDTO{}, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionAgentCredentialIssue,
		Resource: authz.Resource{Type: "agent_credential", ID: ""},
	}); err != nil {
		return IssuedAgentCredentialDTO{}, err
	}
	if in.AgentID == "" {
		return IssuedAgentCredentialDTO{}, apperrors.New("VALIDATION_FAILED", "agent_id is required", 400)
	}
	if in.TenantRole == "" {
		return IssuedAgentCredentialDTO{}, apperrors.New("VALIDATION_FAILED", "tenant_role is required", 400)
	}
	if len(in.AllowedTypes) == 0 {
		// Refused here as well as in the signer so the caller gets a 400 that
		// names the field, instead of the signer's error arriving as a 500. The
		// signer's copy is the one that is load-bearing — it guards every future
		// call site — and this one is the message.
		return IssuedAgentCredentialDTO{}, apperrors.New("VALIDATION_FAILED", "allowed_types must name at least one content type", 400)
	}
	if bearer == "" {
		// Dev-header mode: there is a subject but no token underneath it, so
		// there is nothing to downgrade. Refusing is the only honest answer —
		// synthesising claims here would mint a credential that no login backs.
		return IssuedAgentCredentialDTO{}, apperrors.New("VALIDATION_FAILED", "minting an agent credential requires a bearer token to downgrade", 400)
	}
	minter, err := s.signer.ParseAccessToken(bearer)
	if err != nil {
		return IssuedAgentCredentialDTO{}, apperrors.ErrUnauthorized
	}

	// Checked here as well as in the signer, and against the TOKEN's role rather
	// than the Subject's, so that both layers answer the same question about the
	// same fact. This copy exists for the message: the signer's refusal is a
	// sentinel error with no room for the two role names, and "403" alone does
	// not tell an operator whether the role was invalid or merely above them.
	if !tenantdomain.CanMintAgentRole(minter.TenantRole, in.TenantRole) {
		return IssuedAgentCredentialDTO{}, apperrors.New(
			"FORBIDDEN",
			fmt.Sprintf("a caller holding the %s role may not mint an agent credential carrying the %s role", minter.TenantRole, in.TenantRole),
			403,
		)
	}

	credentialID := uuid.New()
	token, expiresAt, err := s.signer.IssueAgentToken(minter, credentialID, in.AgentID, in.TenantRole, in.AllowedTypes)
	if err != nil {
		// ErrRoleNotMintable is listed even though the check above already
		// refused that case: if this call ever stops being preceded by it, the
		// answer must still be a 403 rather than the 500 an unmapped sentinel
		// produces. The message is the poorer one — that is what the check
		// above is for — but the STATUS is not something to lose.
		if errors.Is(err, jwt.ErrNotMintable) || errors.Is(err, jwt.ErrAgentScopeUnset) ||
			errors.Is(err, jwt.ErrRoleNotMintable) {
			return IssuedAgentCredentialDTO{}, apperrors.ErrForbidden
		}
		return IssuedAgentCredentialDTO{}, err
	}

	// Signed first, recorded second, and the token is returned only if the
	// record lands. The other order leaves a row that looks like a live
	// credential when signing failed; this order can only lose a token that
	// never left this function — and a token with no row is refused at the
	// middleware, so even that failure is inert rather than dangerous.
	if err := s.repo.Insert(ctx, repository.AgentCredential{
		ID:           credentialID,
		TenantID:     minter.TenantID,
		AgentID:      in.AgentID,
		PrincipalID:  sub.UserID,
		TenantRole:   in.TenantRole,
		AllowedTypes: in.AllowedTypes,
		ExpiresAt:    expiresAt,
	}); err != nil {
		return IssuedAgentCredentialDTO{}, err
	}

	return IssuedAgentCredentialDTO{
		ID:           credentialID.String(),
		Token:        token,
		AgentID:      in.AgentID,
		AllowedTypes: in.AllowedTypes,
		ExpiresAt:    expiresAt,
	}, nil
}

func (s *agentCredentialService) List(ctx context.Context) ([]AgentCredentialDTO, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return nil, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionAgentCredentialList,
		Resource: authz.Resource{Type: "agent_credential", ID: ""},
	}); err != nil {
		return nil, err
	}
	creds, err := s.repo.ListByTenant(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]AgentCredentialDTO, 0, len(creds))
	for _, c := range creds {
		out = append(out, AgentCredentialDTO{
			ID:           c.ID.String(),
			AgentID:      c.AgentID,
			PrincipalID:  c.PrincipalID.String(),
			TenantRole:   c.TenantRole,
			AllowedTypes: c.AllowedTypes,
			ExpiresAt:    c.ExpiresAt,
			RevokedAt:    c.RevokedAt,
			CreatedAt:    c.CreatedAt,
			Active:       c.RevokedAt == nil && c.ExpiresAt.After(now),
		})
	}
	return out, nil
}

func (s *agentCredentialService) Revoke(ctx context.Context, id uuid.UUID) error {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionAgentCredentialRevoke,
		Resource: authz.Resource{Type: "agent_credential", ID: id.String()},
	}); err != nil {
		return err
	}
	// sub.TenantID, not a tenant from the request: the caller names a credential
	// id and nothing else, so a stolen id from another tenant finds no row.
	err := s.repo.Revoke(ctx, sub.TenantID, id, sub.UserID)
	if errors.Is(err, repository.ErrAgentCredentialNotFound) {
		return apperrors.ErrNotFound
	}
	return err
}
