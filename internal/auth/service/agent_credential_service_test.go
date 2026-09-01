package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/auth/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// fakeAgentCredentialRepo records what the service decided to store. Only
// Insert is exercised here; the other three answer the lifecycle questions
// that have their own tests over real HTTP.
type fakeAgentCredentialRepo struct {
	inserted []repository.AgentCredential
}

func (f *fakeAgentCredentialRepo) Insert(_ context.Context, c repository.AgentCredential) error {
	f.inserted = append(f.inserted, c)
	return nil
}

func (f *fakeAgentCredentialRepo) ListByTenant(context.Context, string) ([]repository.AgentCredential, error) {
	return nil, nil
}
func (f *fakeAgentCredentialRepo) Revoke(context.Context, string, uuid.UUID, uuid.UUID) error {
	return nil
}
func (f *fakeAgentCredentialRepo) IsActive(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

// minterContext returns a service, the caller's raw bearer, and the repo the
// service writes to. The token is REAL (signed and parsed back) because Issue
// downgrades the token rather than the subject — a synthesised Claims value
// would skip the very path under test.
func minterContext(t *testing.T, role string) (context.Context, AgentCredentialService, string, *fakeAgentCredentialRepo) {
	t.Helper()
	signer := jwt.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	principal := uuid.New()
	bearer, _, err := signer.IssueAccessToken(principal, nil, "tenant-a", role, "eu", true)
	require.NoError(t, err)

	repo := &fakeAgentCredentialRepo{}
	svc := NewAgentCredentialService(repo, signer, authz.NewRBACAuthorizer())
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: principal, TenantID: "tenant-a", TenantRole: role,
	})
	return ctx, svc, bearer, repo
}

// ADR-013 補裁 S-1 at the layer that answers the caller. The signer refuses the
// same thing with a sentinel error; what this layer owes is a 403 that names
// both roles, because "403" alone cannot be told from a missing verb.
func TestIssueBoundsTheRoleByTheMinters(t *testing.T) {
	ctx, svc, bearer, repo := minterContext(t, "owner")

	_, err := svc.Issue(ctx, bearer, IssueAgentCredentialInput{
		AgentID: "peer-bot", TenantRole: "owner", AllowedTypes: []string{"memo"},
	})
	require.Error(t, err)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 403, appErr.HTTPStatus)
	assert.Contains(t, appErr.Message, "owner")
	assert.Empty(t, repo.inserted, "a refused mint must not leave a row behind")
}

// Absent is refused, not defaulted. The only available default would be the
// minter's own role — the behaviour this ruling removed — and it would arrive
// on exactly the path where the caller forgot to decide.
func TestIssueRequiresARoleRatherThanCopyingOne(t *testing.T) {
	ctx, svc, bearer, repo := minterContext(t, "owner")

	_, err := svc.Issue(ctx, bearer, IssueAgentCredentialInput{
		AgentID: "peer-bot", AllowedTypes: []string{"memo"},
	})
	require.Error(t, err)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 400, appErr.HTTPStatus)
	assert.Contains(t, appErr.Message, "tenant_role")
	assert.Empty(t, repo.inserted)
}

// The granted case, and the assertion that separates "chosen" from "copied":
// an OWNER mints an EDITOR, which a copy cannot produce. The stored role is
// checked as well as the token's, because the registry row is what an operator
// reads during an incident and it is written from a separate argument.
func TestIssueStoresTheRoleThatWasAskedFor(t *testing.T) {
	ctx, svc, bearer, repo := minterContext(t, "owner")

	dto, err := svc.Issue(ctx, bearer, IssueAgentCredentialInput{
		AgentID: "narrow-bot", TenantRole: "editor", AllowedTypes: []string{"memo"},
	})
	require.NoError(t, err)
	require.Len(t, repo.inserted, 1)
	assert.Equal(t, "editor", repo.inserted[0].TenantRole)

	signer := jwt.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	claims, err := signer.ParseAccessToken(dto.Token)
	require.NoError(t, err)
	assert.Equal(t, "editor", claims.TenantRole,
		"the token and the row must not disagree about what this credential is")
}
