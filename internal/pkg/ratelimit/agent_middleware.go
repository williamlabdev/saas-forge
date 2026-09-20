package ratelimit

import (
	"net/http"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
)

// Limiter is the seam the agent middleware needs, and the whole of it.
//
// The same one-method shape `apps/delivery` already uses, restated here rather
// than imported because that package is a separate binary: a shared interface
// would make two deployables depend on each other to say "something that can
// say no", which is not a dependency either of them has.
type Limiter interface {
	Allow(key string) bool
}

// ErrAgentRateLimited is what an agent over its budget receives.
//
// It is deliberately NOT `auth.ErrRateLimited`. That one says "too many login
// attempts", which is a lie here and — worse — points whoever reads it at the
// login limiter, an unrelated component keyed by client IP. A refusal that
// misnames its own cause costs more than one that says nothing.
var ErrAgentRateLimited = apperrors.New(
	"AGENT_RATE_LIMITED",
	"too many requests for this agent credential; slow down",
	http.StatusTooManyRequests,
)

// AgentMiddleware caps how fast ONE agent credential may call the domain API
// (ADR-013 補裁 S-2 題一, ruled 2026-08-20).
//
// WHY A LAYER AND NOT A KEY CHANGE. The ruling was first costed as "change the
// limiter key from tenant to tenant+agent", which is true of `apps/delivery`
// and false of the domain API — the surface the ruling actually names. The
// content REST router has no limiter to re-key (41 endpoints, zero calls:
// `grep -cE '\.(Get|Post|Put|Patch|Delete)\(' internal/cms/content/handler/router.go`
// — the grep is the criterion, the number goes stale) and neither does the
// global chain. So there was nothing to re-key and this is a new layer, which
// william approved on the corrected cost.
//
// WHY IT IS MOUNTED EVEN WHEN THE LIMIT IS OFF. `NewIPLimiter(0, …)` allows
// everything, so a disabled limit could equally be expressed by not mounting
// this at all — and that is the worse option, because it makes the middleware
// chain itself differ between a developer's machine and production. The class
// of bug that hides in exactly that gap is the one ADR-015 just spent a landing
// on (`GatewayGuard` is a no-op locally, so the shape that fails in production
// is structurally unreachable in dev). Mounting unconditionally costs one map
// lookup that never happens — `Allow` returns on `max <= 0` before touching the
// mutex — and keeps the chain honest.
//
// WHY ONLY AGENTS. Humans are already bounded by something this cannot see: a
// person's access token lives 15 minutes and a person clicks. An agent holds a
// long-lived credential and loops. Limiting everyone would be a bigger,
// different decision, and nobody has made it.
func AgentMiddleware(limiter Limiter, onRefused func()) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub, ok := authn.SubjectFromContext(r.Context())
			if !ok || !sub.IsAgent() {
				next.ServeHTTP(w, r)
				return
			}
			if limiter != nil && !limiter.Allow(agentKey(sub)) {
				if onRefused != nil {
					onRefused()
				}
				response.Error(w, ErrAgentRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// agentKey names the bucket one agent credential draws from.
//
// TENANT + AGENT NAME, and each half earns its place:
//
//   - The agent NAME rather than the credential id, so that rotating a
//     credential does not hand the same agent a fresh budget. Rotation in this
//     system is "mint a second credential for the same agent_id, switch, revoke
//     the old" (ADR-013 explains why there is no `rotate` verb) — keying on
//     `jti` would make the documented rotation procedure a documented way to
//     reset the counter.
//   - The TENANT because an agent name is a string a tenant chooses. Two
//     tenants both naming their importer `import-bot` must not share a bucket,
//     or either can throttle the other by looping.
func agentKey(sub authn.Subject) string {
	// Unreachable today and still not a fallthrough. `ParseAccessToken`
	// validates the agent claims as a SET and `subjectFromBearer` refuses a
	// token whose agent id or jti is missing, so an agent subject always has a
	// name here. But the primitive treats an EMPTY key as unlimited
	// (`Allow` returns true on `key == ""`), which turns any future hole in
	// that guarantee into silent exemption for the exact callers this layer
	// exists to bound. One shared bucket for the unnameable is the wrong answer
	// too — but it is wrong in the direction that shows up as a 429 someone
	// investigates, not as a limit that quietly stopped applying.
	name := ""
	if sub.AgentID != nil {
		name = *sub.AgentID
	}
	if name == "" && sub.CredentialID != nil {
		name = "jti:" + sub.CredentialID.String()
	}
	if name == "" {
		return "agent:unidentified"
	}
	return "agent:" + sub.TenantID + ":" + name
}
