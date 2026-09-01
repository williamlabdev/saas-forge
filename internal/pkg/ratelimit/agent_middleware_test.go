package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// agentSubject is what JWTMiddleware puts in the context for an agent token:
// name, principal and jti are all present, because ParseAccessToken validates
// the agent claims as a set and refuses the token otherwise.
func agentSubject(tenant, name string) authn.Subject {
	principal := uuid.New()
	credential := uuid.New()
	return authn.Subject{
		UserID:       uuid.New(),
		TenantID:     tenant,
		TenantRole:   "editor",
		Kind:         authn.ActorKindAgent,
		AgentID:      &name,
		PrincipalID:  &principal,
		CredentialID: &credential,
	}
}

// serve runs one request through the middleware as the given subject (nil = an
// unauthenticated request) and reports the status plus whether it got through.
func serve(t *testing.T, mw func(http.Handler) http.Handler, sub *authn.Subject) (int, bool, string) {
	t.Helper()
	reached := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/entries/article", nil)
	if sub != nil {
		req = req.WithContext(authn.WithSubject(req.Context(), *sub))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, reached, rec.Body.String()
}

func TestAgentMiddleware_CapsOneAgentAndSaysWhy(t *testing.T) {
	refusals := 0
	mw := AgentMiddleware(NewIPLimiter(2, time.Minute), func() { refusals++ })
	sub := agentSubject("tenant-a", "import-bot")

	for i := 1; i <= 2; i++ {
		code, reached, _ := serve(t, mw, &sub)
		require.Equal(t, http.StatusOK, code, "request %d is within the cap", i)
		require.True(t, reached)
	}

	code, reached, body := serve(t, mw, &sub)
	require.Equal(t, http.StatusTooManyRequests, code)
	assert.False(t, reached, "a refused request must not reach the handler")
	assert.Equal(t, 1, refusals, "the refusal is counted exactly once")

	// The body has to name the right component. Reusing auth's error would
	// answer "too many login attempts" to an agent that never logged in.
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.NotNil(t, env.Error)
	assert.Equal(t, "AGENT_RATE_LIMITED", env.Error.Code)
	assert.NotContains(t, env.Error.Message, "login")
}

// The bucket is per agent, not per tenant. Without this, one busy agent
// throttles every other agent its tenant owns — which is the behaviour the
// ruling exists to replace ("算租戶帳" was already the status quo elsewhere).
func TestAgentMiddleware_OneAgentDoesNotSpendAnothersBudget(t *testing.T) {
	mw := AgentMiddleware(NewIPLimiter(1, time.Minute), nil)
	busy := agentSubject("tenant-a", "import-bot")
	quiet := agentSubject("tenant-a", "report-bot")

	code, _, _ := serve(t, mw, &busy)
	require.Equal(t, http.StatusOK, code)
	code, _, _ = serve(t, mw, &busy)
	require.Equal(t, http.StatusTooManyRequests, code, "the busy agent spends its own budget")

	code, reached, _ := serve(t, mw, &quiet)
	assert.Equal(t, http.StatusOK, code, "the quiet agent's budget is untouched")
	assert.True(t, reached)
}

// Two tenants may both call their importer `import-bot` — agent names are
// strings a tenant picks. Sharing a bucket would let either throttle the other
// by looping, which is a cross-tenant effect nobody asked for.
func TestAgentMiddleware_SameAgentNameInTwoTenantsAreSeparateBuckets(t *testing.T) {
	mw := AgentMiddleware(NewIPLimiter(1, time.Minute), nil)
	a := agentSubject("tenant-a", "import-bot")
	b := agentSubject("tenant-b", "import-bot")

	require.Equal(t, http.StatusOK, first(serve(t, mw, &a)))
	require.Equal(t, http.StatusTooManyRequests, first(serve(t, mw, &a)))
	assert.Equal(t, http.StatusOK, first(serve(t, mw, &b)),
		"tenant-b's import-bot must not be paying for tenant-a's")
}

// Rotation in this system is "mint a second credential for the same agent_id,
// switch, revoke the old" — ADR-013 explains why there is no `rotate` verb. If
// the bucket were keyed on the credential (`jti`), that documented procedure
// would double as a documented way to reset the counter.
func TestAgentMiddleware_RotatingTheCredentialDoesNotResetTheBudget(t *testing.T) {
	mw := AgentMiddleware(NewIPLimiter(1, time.Minute), nil)
	before := agentSubject("tenant-a", "import-bot")
	require.Equal(t, http.StatusOK, first(serve(t, mw, &before)))

	after := agentSubject("tenant-a", "import-bot") // same agent, fresh jti and principal
	require.NotEqual(t, *before.CredentialID, *after.CredentialID, "precondition: a different credential")
	assert.Equal(t, http.StatusTooManyRequests, first(serve(t, mw, &after)),
		"the same agent under a new credential keeps spending the same budget")
}

// Humans and the unauthenticated pass untouched. This is the blast radius of
// the whole layer: a cap that also applied to people would be a much larger
// decision, and it is not the one that was ruled.
func TestAgentMiddleware_NonAgentsAreNotLimited(t *testing.T) {
	mw := AgentMiddleware(NewIPLimiter(1, time.Minute), func() {
		t.Fatal("a non-agent request must never reach the limiter")
	})
	human := authn.Subject{UserID: uuid.New(), TenantID: "tenant-a", TenantRole: "owner"}

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusOK, first(serve(t, mw, &human)), "human request %d", i)
		require.Equal(t, http.StatusOK, first(serve(t, mw, nil)), "anonymous request %d", i)
	}
}

// The cap is off by default (config keeps AGENT_RATE_LIMIT at zero) and the
// middleware is still mounted. This pins that the disabled shape is a
// pass-through and not an accidental refusal — and that the chain is the same
// object in dev as in production, which is the property the mounting decision
// buys.
func TestAgentMiddleware_DisabledLimitAllowsEverything(t *testing.T) {
	mw := AgentMiddleware(NewIPLimiter(0, time.Minute), func() {
		t.Fatal("a disabled limiter must refuse nothing")
	})
	sub := agentSubject("tenant-a", "import-bot")
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusOK, first(serve(t, mw, &sub)))
	}
}

// 🔴 An empty key means UNLIMITED to the primitive (`Allow` returns true on
// `key == ""`). So a nameless agent subject — which JWT parsing makes
// unreachable today — must not be allowed to produce one, or the day that
// guarantee develops a hole, this layer switches itself off for exactly the
// callers it exists to bound, silently.
func TestAgentMiddleware_ANamelessAgentIsStillBounded(t *testing.T) {
	mw := AgentMiddleware(NewIPLimiter(1, time.Minute), nil)
	empty := ""
	credential := uuid.New()
	nameless := authn.Subject{
		UserID:       uuid.New(),
		TenantID:     "tenant-a",
		Kind:         authn.ActorKindAgent,
		AgentID:      &empty,
		CredentialID: &credential,
	}

	require.Equal(t, http.StatusOK, first(serve(t, mw, &nameless)))
	assert.Equal(t, http.StatusTooManyRequests, first(serve(t, mw, &nameless)),
		"an unnameable agent falls back to its credential, it does not become exempt")

	// And with nothing at all to name it, one shared bucket rather than none.
	bare := authn.Subject{UserID: uuid.New(), Kind: authn.ActorKindAgent}
	mw2 := AgentMiddleware(NewIPLimiter(1, time.Minute), nil)
	require.Equal(t, http.StatusOK, first(serve(t, mw2, &bare)))
	assert.Equal(t, http.StatusTooManyRequests, first(serve(t, mw2, &bare)))
}

func first(code int, _ bool, _ string) int { return code }
