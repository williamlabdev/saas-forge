package platform

import (
	"net/http"

	authhandler "github.com/williamlabdev/saas-forge/internal/auth/handler"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	iamhandler "github.com/williamlabdev/saas-forge/internal/iam/handler"
	notificationhandler "github.com/williamlabdev/saas-forge/internal/notification/handler"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
	platformopshandler "github.com/williamlabdev/saas-forge/internal/platformops/handler"
	tenanthandler "github.com/williamlabdev/saas-forge/internal/tenant/handler"
	tickethandler "github.com/williamlabdev/saas-forge/internal/ticket/handler"
	userhandler "github.com/williamlabdev/saas-forge/internal/user/handler"

	contenthandler "github.com/williamlabdev/saas-forge/internal/cms/content/handler"
	// new-domain:imports
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds the HTTP router with optional dev header fallback.
func NewRouter(
	userH *userhandler.Handler,
	authH *authhandler.Handler,
	iamH *iamhandler.Handler,
	notificationH *notificationhandler.Handler,
	platformopsH *platformopshandler.Handler,
	ticketH *tickethandler.Handler,
	contentH *contenthandler.Handler,
	tenantH *tenanthandler.Handler,
	agentCredH *authhandler.AgentCredentialHandler,
	// new-domain:router-params
	signer *jwt.Signer,
	allowDevHeaders bool,
	// agentCredentials is the revocation lookup for agent tokens. Passing nil
	// does not disable the check — it refuses every agent credential. See
	// authn.JWTMiddleware.
	agentCredentials authn.AgentCredentialChecker,
	registry *metrics.Registry,
	metricsEnabled bool,
	metricsBearerToken string,
	gatewaySecret string,
	// agentLimiter caps one agent credential's request rate. Passed in rather
	// than built here so both composition roots get it from the same provider
	// (ProvideAgentRateLimiter) — the failure this avoids has a precedent in
	// this repo: e2e hand-assembled a second outbox worker, it did not follow
	// production when production gained webhooks, and every delivery failed
	// invisibly because a passing test prints no log.
	agentLimiter ratelimit.Limiter,
) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	// Reject requests that didn't come through the trusted gateway before any
	// auth work (TKT-R7); no-op when unset. /health stays open for probes.
	r.Use(authn.GatewayGuard(gatewaySecret, func(req *http.Request) bool {
		return req.URL.Path == "/health"
	}))
	r.Use(authn.JWTMiddleware(signer, allowDevHeaders, agentCredentials))
	// Per-agent rate limit (ADR-013 補裁 S-2 題一). AFTER auth because it needs
	// the subject, BEFORE the routes because the ruling is about the domain API
	// as a whole and not about the content surface it was noticed on. Mounted
	// unconditionally even when the cap is off — see AgentMiddleware for why a
	// chain that differs between dev and production is the thing to avoid.
	var onAgentRefused func()
	if registry != nil {
		onAgentRefused = func() { registry.AgentRateLimited.Add(1) }
	}
	r.Use(ratelimit.AgentMiddleware(agentLimiter, onAgentRefused))

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if metricsEnabled && registry != nil {
		r.With(metrics.BearerGuard(metricsBearerToken)).Get("/metrics", registry.Handler().ServeHTTP)
	}

	userH.Routes(r)
	authH.Routes(r)
	iamH.Routes(r)
	ticketH.Routes(r)
	contentH.Routes(r)
	tenantH.Routes(r)
	if agentCredH != nil {
		agentCredH.Routes(r)
	}
	// new-domain:router-mount
	if notificationH != nil {
		notificationH.Routes(r)
	}
	if platformopsH != nil {
		platformopsH.Routes(r)
	}
	return r
}
