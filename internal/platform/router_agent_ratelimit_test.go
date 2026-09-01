package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
)

// 🔴 WHY THIS TEST EXISTS SEPARATELY FROM THE MIDDLEWARE'S OWN TESTS.
//
// `ratelimit/agent_middleware_test.go` proves the middleware refuses correctly.
// It says nothing about whether anything MOUNTS it — delete the `r.Use` line in
// NewRouter and every one of those tests stays green while the domain API has
// no agent rate limit at all. That failure mode has bitten this repo before
// (moving the Publish button back onto the list row removed a gate with the
// entire backend suite still green), so the wiring gets its own assertion,
// through the real NewRouter.
//
// Handlers are nil and the probe path matches no route on purpose. chi runs the
// middleware chain BEFORE routing, so the limiter answers whether or not a path
// resolves — which lets this assert the chain without standing up a database,
// and makes the assertion sharper: 404 then 429 means the refusal came from the
// chain, since nothing downstream of it ever ran.

type activeChecker struct{}

func (activeChecker) IsActive(context.Context, uuid.UUID) (bool, error) { return true, nil }

// agentBearer mints a real agent token the way the platform does — by
// downgrading a minter's claims — so the router's own JWT middleware is what
// puts the agent subject in the context, not the test.
func agentBearer(t *testing.T, signer *jwt.Signer, agentID string) string {
	t.Helper()
	minter := &jwt.Claims{
		TenantID:   "tenant-a",
		TenantRole: "admin",
		Roles:      []string{"member"},
	}
	minter.Subject = uuid.NewString()
	token, _, err := signer.IssueAgentToken(minter, uuid.New(), agentID, "editor", []string{"article"})
	require.NoError(t, err)
	return token
}

func agentRouter(t *testing.T, limit int) (http.Handler, *jwt.Signer, *metrics.Registry) {
	t.Helper()
	signer := jwt.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	reg := metrics.NewRegistry()
	rt := config.Runtime{AgentRateLimit: limit, AgentRateWindow: time.Minute}
	h := NewRouter(
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
		signer,
		false, // dev headers off: the bearer is the only way in
		activeChecker{},
		reg,
		false, "", "",
		ProvideAgentRateLimiter(rt),
	)
	return h, signer, reg
}

func hit(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/__ratelimit_probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestRouter_MountsThePerAgentRateLimit(t *testing.T) {
	h, signer, reg := agentRouter(t, 1)
	token := agentBearer(t, signer, "import-bot")

	require.Equal(t, http.StatusNotFound, hit(t, h, token),
		"the first request reaches routing, which is as far as an unmatched path goes")
	assert.Equal(t, http.StatusTooManyRequests, hit(t, h, token),
		"the second request must be refused BY THE ROUTER, not just by a middleware nobody mounted")
	assert.EqualValues(t, 1, reg.AgentRateLimited.Load(), "the router wires the counter too")
}

// The default configuration (AGENT_RATE_LIMIT unset ⇒ 0) mounts the same chain
// and refuses nothing. Pinning it because the alternative design — mount only
// when configured — would make the dev chain a different object from the
// production one, and this repo has just spent a landing on what hides in that
// gap (ADR-015's GatewayGuard is a no-op locally).
func TestRouter_DefaultConfigMountsTheLayerAndCapsNothing(t *testing.T) {
	t.Setenv("AGENT_RATE_LIMIT", "")
	rt := config.LoadRuntimeFromEnv()
	require.Equal(t, 0, rt.AgentRateLimit, "the default is off — the number is an operator's decision (ADR-013 補裁 S-2)")

	h, signer, reg := agentRouter(t, rt.AgentRateLimit)
	token := agentBearer(t, signer, "import-bot")
	for i := 0; i < 10; i++ {
		require.Equal(t, http.StatusNotFound, hit(t, h, token), "request %d", i)
	}
	assert.EqualValues(t, 0, reg.AgentRateLimited.Load())
}

// A human carrying a perfectly ordinary access token is not affected, through
// the real chain — the middleware's own test asserts this against a subject it
// constructs, this one asserts it against a subject the router derived.
func TestRouter_HumansAreNotCaughtByTheAgentLimit(t *testing.T) {
	h, signer, _ := agentRouter(t, 1)
	human, _, err := signer.IssueAccessToken(uuid.New(), []string{"member"}, "tenant-a", "owner", "", false)
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusNotFound, hit(t, h, human), "human request %d", i)
	}
}

var _ ratelimit.Limiter = (*ratelimit.IPLimiter)(nil)
