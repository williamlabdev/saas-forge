package service

import apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"

var (
	ErrInvalidCredentials = apperrors.New("AUTH_INVALID_CREDENTIALS", "invalid email or password", 401)
	ErrInvalidToken       = apperrors.New("AUTH_INVALID_TOKEN", "invalid or expired token", 401)
	ErrRefreshRevoked     = apperrors.New("AUTH_REFRESH_REVOKED", "refresh token revoked or expired", 401)
	ErrRateLimited        = apperrors.New("AUTH_RATE_LIMITED", "too many login attempts; try again later", 429)
	// ErrMembershipRevoked fires when the active tenant's membership vanished
	// between issuance and refresh — the refresh revocation checkpoint (plan §4.1).
	ErrMembershipRevoked = apperrors.New("AUTH_TENANT_MEMBERSHIP_REVOKED", "tenant membership no longer valid; sign in again", 401)
	// ErrNotTenantMember rejects a switch-tenant request for a tenant the
	// user has no membership in (D5). 403, not 404: the caller is
	// authenticated, and slugs are opaque random values (D10), so membership
	// denial leaks nothing about tenant existence.
	ErrNotTenantMember = apperrors.New("AUTH_NOT_TENANT_MEMBER", "not a member of the requested tenant", 403)
)
