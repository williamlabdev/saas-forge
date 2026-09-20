package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// stubChecker is the revocation lookup as the middleware sees it. It records
// WHICH id it was asked about, because "the middleware checked something" and
// "the middleware checked this token's credential" are different claims and
// only the second one is worth anything.
type stubChecker struct {
	active bool
	err    error
	asked  []uuid.UUID
}

func (c *stubChecker) IsActive(_ context.Context, id uuid.UUID) (bool, error) {
	c.asked = append(c.asked, id)
	return c.active, c.err
}

func subjectOf(t *testing.T, signer *jwt.Signer, token string, devHeaders bool, checker AgentCredentialChecker) (Subject, bool) {
	t.Helper()
	var sub Subject
	var ok bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sub, ok = SubjectFromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	JWTMiddleware(signer, devHeaders, checker)(next).ServeHTTP(httptest.NewRecorder(), req)
	return sub, ok
}

// A token this deployment MINTS must be a token this deployment ACCEPTS, with
// the agent fields intact — the round trip no unit test of either half proves.
func TestAgentTokenBecomesAnAgentSubject(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)

	principal := uuid.New()
	raw, _, err := signer.IssueAccessToken(principal, []string{"admin"}, "tenant-a", "admin", "eu", true)
	require.NoError(t, err)
	minter, err := signer.ParseAccessToken(raw)
	require.NoError(t, err)

	credID := uuid.New()
	agentRaw, _, err := signer.IssueAgentToken(minter, credID, "content-bot", "editor", []string{"post", "faq"})
	require.NoError(t, err)

	live := &stubChecker{active: true}
	sub, ok := subjectOf(t, signer, agentRaw, false, live)
	require.True(t, ok)
	require.True(t, sub.IsAgent())
	require.NotNil(t, sub.AgentID)
	assert.Equal(t, "content-bot", *sub.AgentID)
	require.NotNil(t, sub.PrincipalID)
	assert.Equal(t, principal, *sub.PrincipalID)
	assert.Equal(t, []string{"post", "faq"}, sub.AllowedTypes)
	assert.Equal(t, "editor", sub.TenantRole)
	assert.Nil(t, sub.Roles, "the platform plane must not survive the downgrade")

	// WHERE THE PROVENANCE GUARANTEE ACTUALLY COMES FROM TODAY. The minted
	// token's subject IS the principal, so UserID and PrincipalID agree, and
	// nothing anywhere holds an unresolvable agent uuid. actor()'s agent branch
	// is therefore belt-and-braces on this path rather than the thing doing the
	// work — which is why it has its own whitebox test in the CMS service
	// (TestActorRecordsThePrincipalEvenIfTheSubjectDiffers) instead of relying
	// on a token to tell the two apart.
	assert.Equal(t, *sub.PrincipalID, sub.UserID,
		"a minted agent credential speaks as its principal — if this ever stops being true, actor() and confinedAuthor() are what keep created_by nameable")

	require.NotNil(t, sub.CredentialID)
	assert.Equal(t, credID, *sub.CredentialID)
	assert.Equal(t, []uuid.UUID{credID}, live.asked,
		"the revocation lookup must be asked about THIS token's credential, not merely called")
}

// The agent fields are credential properties, exactly like PublicDelivery: a
// dev header must never be able to assert one. Otherwise a developer's browser
// mints the credential ADR-013 §1 exists to bound.
func TestDevHeadersCannotAssertAnActorKind(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", uuid.NewString())
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Tenant-Role", "owner")
	// Every spelling a hopeful client might try.
	req.Header.Set("X-Actor-Kind", "agent")
	req.Header.Set("X-Agent-Id", "content-bot")
	req.Header.Set("X-Allowed-Types", "post")
	req.Header.Set("X-Principal-Id", uuid.NewString())

	var sub Subject
	var ok bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sub, ok = SubjectFromContext(r.Context())
		next.ServeHTTP(nil, r)
	})
	JWTMiddleware(signer, true, &stubChecker{active: true})(inner).ServeHTTP(httptest.NewRecorder(), req)

	require.True(t, ok, "the dev-header path must still produce a subject, or this proves nothing")
	assert.False(t, sub.IsAgent(), "an actor kind may only come from a signed claim")
	assert.Nil(t, sub.AgentID)
	assert.Nil(t, sub.PrincipalID)
	assert.Nil(t, sub.AllowedTypes)
}

// The revocation half (ruled 2026-08-06). A long-lived agent credential is only
// issuable because it can be turned off, and "turned off" has to mean the very
// next request — not the next restart, and not when the token expires.
func TestRevokedAgentCredentialIsNotHonoured(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)

	principal := uuid.New()
	raw, _, err := signer.IssueAccessToken(principal, nil, "tenant-a", "admin", "eu", true)
	require.NoError(t, err)
	minter, err := signer.ParseAccessToken(raw)
	require.NoError(t, err)
	agentRaw, _, err := signer.IssueAgentToken(minter, uuid.New(), "content-bot", "editor", []string{"post"})
	require.NoError(t, err)

	// The control first: the SAME token, with a checker that says the row is
	// live, authenticates. Without it every case below is satisfied by a
	// middleware that refuses agent tokens outright.
	_, ok := subjectOf(t, signer, agentRaw, false, &stubChecker{active: true})
	require.True(t, ok)

	for name, checker := range map[string]AgentCredentialChecker{
		// Revoked, expired and deleted all arrive here as `false` — the bearer
		// is told nothing about which.
		"revoked":               &stubChecker{active: false},
		"database unreachable":  &stubChecker{active: true, err: errRegistryUnreachable},
		"no checker configured": nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := subjectOf(t, signer, agentRaw, false, checker)
			assert.False(t, ok, "a credential that cannot be confirmed live must not authenticate")
		})
	}
}

var errRegistryUnreachable = errors.New("the revocation registry is unreachable")

// The nil checker refuses AGENTS and nothing else. An app with no agent surface
// passes nil and behaves exactly as it did before this parameter existed — which
// is what makes fail-closed affordable rather than a migration.
func TestNilCheckerLeavesHumanCredentialsAlone(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)

	human, _, err := signer.IssueAccessToken(uuid.New(), []string{"admin"}, "tenant-a", "owner", "eu", true)
	require.NoError(t, err)

	sub, ok := subjectOf(t, signer, human, false, nil)
	require.True(t, ok, "a human token needs no revocation record — it has a 15-minute TTL instead")
	assert.False(t, sub.IsAgent())
	assert.Nil(t, sub.CredentialID, "only an agent credential carries one")
}
