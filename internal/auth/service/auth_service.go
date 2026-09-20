package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/williamlabdev/saas-forge/internal/auth/audit"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/auth/password"
	"github.com/williamlabdev/saas-forge/internal/auth/repository"
	iamservice "github.com/williamlabdev/saas-forge/internal/iam/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/identity"
	"github.com/williamlabdev/saas-forge/internal/pkg/requestctx"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// CredentialInserter writes credentials inside a user-registration transaction.
type CredentialInserter interface {
	InsertCredentialsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, passwordHash string) error
}

// TenantDirectory resolves tenant memberships at token issuance. Membership is
// looked up fresh on every issue — never trusted from an old token — so role
// changes and revocations take effect at the next refresh (plan §4.1).
type TenantDirectory interface {
	MembershipsForUser(ctx context.Context, userID uuid.UUID) ([]tenantdomain.UserMembership, error)
	MembershipRole(ctx context.Context, userID uuid.UUID, slug string) (string, error)
}

// AuthService handles login, refresh, tenant switching, and password hashing
// for registration.
type AuthService interface {
	HashPassword(plain string) (string, error)
	Login(ctx context.Context, in LoginInput) (*TokenResponse, error)
	Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error)
	// SwitchTenant issues a fresh token pair for the given tenant after
	// verifying the user's membership in it (D5). The previous access token
	// stays valid until its TTL expires; the previous refresh token is
	// untouched (parallel sessions on different tenants are legitimate).
	SwitchTenant(ctx context.Context, userID uuid.UUID, tenantSlug string) (*TokenResponse, error)
	Logout(ctx context.Context, refreshToken string) error
}

type LoginInput struct {
	Email    string `validate:"required,email"`
	Password string `validate:"required,min=8"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	UserID       string `json:"user_id"`
	// TenantID is the active tenant slug the access token was issued for.
	// Empty for users with no membership (platform operators): they keep
	// platform:* capabilities but content explicitly rejects them (plan §6).
	TenantID string `json:"tenant_id"`
	// AvailableTenants lists all memberships when the user has more than one,
	// so clients can offer a switch (plan §5; switch endpoint lands in PR3).
	AvailableTenants []TenantOption `json:"available_tenants,omitempty"`
}

// TenantOption is one selectable tenant in a login response.
type TenantOption struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type authService struct {
	repo       repository.CredentialRepository
	idx        crypto.BlindIndexer
	hasher     *password.Hasher
	signer     *jwt.Signer
	iam        iamservice.IAMService
	tenants    TenantDirectory
	audit      audit.Recorder
	refreshTTL time.Duration
}

func NewAuthService(
	repo repository.CredentialRepository,
	idx crypto.BlindIndexer,
	signer *jwt.Signer,
	iam iamservice.IAMService,
	tenants TenantDirectory,
	rec audit.Recorder,
	refreshTTL time.Duration,
) AuthService {
	if rec == nil {
		rec = audit.NoopRecorder{}
	}
	return &authService{
		repo:       repo,
		idx:        idx,
		hasher:     password.NewHasher(),
		signer:     signer,
		iam:        iam,
		tenants:    tenants,
		audit:      rec,
		refreshTTL: refreshTTL,
	}
}

func (s *authService) HashPassword(plain string) (string, error) {
	return s.hasher.Hash(plain)
}

func (s *authService) Login(ctx context.Context, in LoginInput) (*TokenResponse, error) {
	if err := validate.Struct(in); err != nil {
		return nil, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	email := identity.NormalizeEmail(in.Email)
	emailHash, err := s.idx.Index(email)
	if err != nil {
		return nil, err
	}
	userID, err := s.repo.UserIDByEmailLookup(ctx, emailHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.record(ctx, audit.Entry{
				EventType: audit.EventLogin, Outcome: audit.OutcomeFailure,
				ErrorCode: "AUTH_INVALID_CREDENTIALS",
			})
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	stored, err := s.repo.GetPasswordHash(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.record(ctx, audit.Entry{
				EventType: audit.EventLogin, Outcome: audit.OutcomeFailure,
				ErrorCode: "AUTH_INVALID_CREDENTIALS",
			})
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if err := s.hasher.Verify(stored, in.Password); err != nil {
		s.record(ctx, audit.Entry{
			EventType: audit.EventLogin, Outcome: audit.OutcomeFailure,
			ErrorCode: "AUTH_INVALID_CREDENTIALS",
		})
		return nil, ErrInvalidCredentials
	}
	_ = s.repo.UpdateLastLogin(ctx, userID)
	memberships, err := s.tenants.MembershipsForUser(ctx, userID)
	if err != nil {
		uid := userID
		s.record(ctx, audit.Entry{
			EventType: audit.EventLogin, Outcome: audit.OutcomeFailure,
			UserID: &uid, ErrorCode: "INTERNAL_ERROR",
		})
		return nil, err
	}
	// Default active tenant = earliest membership (plan §5/§6). Zero
	// memberships (platform operators) issue a tenant-less token; content
	// rejects it explicitly rather than pooling users in an empty bucket.
	slug := ""
	if len(memberships) > 0 {
		slug = memberships[0].Slug
	}
	tokens, err := s.issueTokens(ctx, userID, slug)
	if err != nil {
		uid := userID
		code := "INTERNAL_ERROR"
		if errors.Is(err, apperrors.ErrSuspended) {
			code = apperrors.ErrSuspended.Code
		}
		s.record(ctx, audit.Entry{
			EventType: audit.EventLogin, Outcome: audit.OutcomeFailure,
			UserID: &uid, ErrorCode: code,
		})
		return nil, err
	}
	if len(memberships) > 1 {
		tokens.AvailableTenants = make([]TenantOption, 0, len(memberships))
		for _, m := range memberships {
			tokens.AvailableTenants = append(tokens.AvailableTenants, TenantOption{
				Slug: m.Slug, Name: m.Name, Role: m.Role,
			})
		}
	}
	uid := userID
	s.record(ctx, audit.Entry{
		EventType: audit.EventLogin, Outcome: audit.OutcomeSuccess, UserID: &uid,
	})
	return tokens, nil
}

func (s *authService) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	if refreshToken == "" {
		s.record(ctx, audit.Entry{
			EventType: audit.EventRefresh, Outcome: audit.OutcomeFailure,
			ErrorCode: "AUTH_INVALID_TOKEN",
		})
		return nil, ErrInvalidToken
	}
	hash := hashRefreshToken(refreshToken)
	userID, slug, err := s.repo.FindValidRefresh(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.record(ctx, audit.Entry{
				EventType: audit.EventRefresh, Outcome: audit.OutcomeFailure,
				ErrorCode: "AUTH_REFRESH_REVOKED",
			})
			return nil, ErrRefreshRevoked
		}
		return nil, err
	}
	if slug == "" {
		// In-flight refresh token from before tenant tracking (F6 rollout):
		// degrade to the user's default membership instead of forcing re-login.
		memberships, err := s.tenants.MembershipsForUser(ctx, userID)
		if err != nil {
			uid := userID
			s.record(ctx, audit.Entry{
				EventType: audit.EventRefresh, Outcome: audit.OutcomeFailure,
				UserID: &uid, ErrorCode: "INTERNAL_ERROR",
			})
			return nil, err
		}
		if len(memberships) > 0 {
			slug = memberships[0].Slug
		}
	}
	_ = s.repo.RevokeRefreshToken(ctx, hash)
	tokens, err := s.issueTokens(ctx, userID, slug)
	if err != nil {
		uid := userID
		code := "INTERNAL_ERROR"
		switch {
		case errors.Is(err, apperrors.ErrSuspended):
			code = apperrors.ErrSuspended.Code
		case errors.Is(err, ErrMembershipRevoked):
			code = ErrMembershipRevoked.Code
		}
		s.record(ctx, audit.Entry{
			EventType: audit.EventRefresh, Outcome: audit.OutcomeFailure,
			UserID: &uid, ErrorCode: code,
		})
		return nil, err
	}
	uid := userID
	s.record(ctx, audit.Entry{
		EventType: audit.EventRefresh, Outcome: audit.OutcomeSuccess, UserID: &uid,
	})
	return tokens, nil
}

func (s *authService) SwitchTenant(ctx context.Context, userID uuid.UUID, tenantSlug string) (*TokenResponse, error) {
	if tenantSlug == "" {
		return nil, apperrors.New("VALIDATION_FAILED", "tenant is required", 400)
	}
	// issueTokens re-checks the membership (§4.1); a missing one surfaces as
	// ErrMembershipRevoked, which for an explicit switch means "not a member".
	tokens, err := s.issueTokens(ctx, userID, tenantSlug)
	if err != nil {
		uid := userID
		code := "INTERNAL_ERROR"
		switch {
		case errors.Is(err, apperrors.ErrSuspended):
			code = apperrors.ErrSuspended.Code
		case errors.Is(err, ErrMembershipRevoked):
			err = ErrNotTenantMember
			code = ErrNotTenantMember.Code
		}
		s.record(ctx, audit.Entry{
			EventType: audit.EventTenantSwitch, Outcome: audit.OutcomeFailure,
			UserID: &uid, ErrorCode: code,
		})
		return nil, err
	}
	uid := userID
	s.record(ctx, audit.Entry{
		EventType: audit.EventTenantSwitch, Outcome: audit.OutcomeSuccess, UserID: &uid,
	})
	return tokens, nil
}

func (s *authService) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	err := s.repo.RevokeRefreshToken(ctx, hashRefreshToken(refreshToken))
	outcome := audit.OutcomeSuccess
	code := ""
	if err != nil {
		outcome = audit.OutcomeFailure
		code = "INTERNAL_ERROR"
	}
	s.record(ctx, audit.Entry{
		EventType: audit.EventLogout, Outcome: outcome, ErrorCode: code,
	})
	return err
}

func (s *authService) record(ctx context.Context, e audit.Entry) {
	if m, ok := requestctx.MetaFrom(ctx); ok {
		e.ClientIP = m.ClientIP
		e.UserAgent = m.UserAgent
	}
	if err := s.audit.Record(ctx, e); err != nil {
		// Best-effort audit; never fail the auth flow.
		_ = err
	}
}

// issueTokens is the single confluence point for login and refresh (and
// switch-tenant in PR3). The membership role is resolved fresh from the DB on
// every call — a revoked membership fails the refresh here, making refresh the
// revocation checkpoint (plan §4.1). tenantSlug may be empty (no membership):
// the token then carries no tenant and content access is rejected downstream.
// statusActive is the only account status allowed to hold a session. Compared
// as a bare string rather than importing internal/user/domain: auth must not
// depend on the user module (the dependency runs the other way, at
// registration time).
const statusActive = "active"

// issueTokens is the single funnel every minting path goes through — Login,
// Refresh and SwitchTenant. The account-status check therefore sits here and
// not at the three call sites: a fourth path added later cannot quietly skip
// it. Login reaches this point only after password verification, so status is
// never observable to an unauthenticated caller probing for account state.
func (s *authService) issueTokens(ctx context.Context, userID uuid.UUID, tenantSlug string) (*TokenResponse, error) {
	// A status column that nothing in the auth path reads is a setting that
	// looks like a control and is not one. Non-active covers 'suspended' and
	// 'deleted'; both get ErrSuspended (403) — deleted normally never reaches
	// here, being filtered at lookup and having its tokens revoked on delete.
	status, err := s.repo.UserStatusByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if status != statusActive {
		return nil, apperrors.ErrSuspended
	}
	roles, err := s.iam.RolesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	tenantRole := ""
	if tenantSlug != "" {
		role, err := s.tenants.MembershipRole(ctx, userID, tenantSlug)
		if err != nil {
			if errors.Is(err, apperrors.ErrNotFound) {
				return nil, ErrMembershipRevoked
			}
			return nil, err
		}
		if !tenantdomain.ValidRole(role) {
			// DB CHECK enforces the set; reaching this means schema drift.
			return nil, fmt.Errorf("auth: membership role %q outside allowed set", role)
		}
		tenantRole = role
	}
	access, exp, err := s.signer.IssueAccessToken(userID, roles, tenantSlug, tenantRole, "", false)
	if err != nil {
		return nil, err
	}
	refresh, err := newRefreshToken()
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().UTC().Add(s.refreshTTL)
	if err := s.repo.StoreRefreshToken(ctx, userID, hashRefreshToken(refresh), expiresAt, tenantSlug); err != nil {
		return nil, err
	}
	return &TokenResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(time.Until(exp).Seconds()),
		TokenType:    "Bearer",
		UserID:       userID.String(),
		TenantID:     tenantSlug,
	}, nil
}

func newRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
