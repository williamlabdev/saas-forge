package main

import (
	"net/http"

	authhandler "github.com/williamlabdev/saas-forge/internal/auth/handler"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	authrepo "github.com/williamlabdev/saas-forge/internal/auth/repository"
	contenthandler "github.com/williamlabdev/saas-forge/internal/cms/content/handler"
	iamhandler "github.com/williamlabdev/saas-forge/internal/iam/handler"
	notificationhandler "github.com/williamlabdev/saas-forge/internal/notification/handler"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/platform"
	platformopshandler "github.com/williamlabdev/saas-forge/internal/platformops/handler"
	tenanthandler "github.com/williamlabdev/saas-forge/internal/tenant/handler"
	tickethandler "github.com/williamlabdev/saas-forge/internal/ticket/handler"
	userhandler "github.com/williamlabdev/saas-forge/internal/user/handler"
	// new-domain:imports
)

func provideAppRouter(
	userH *userhandler.Handler,
	authH *authhandler.Handler,
	iamH *iamhandler.Handler,
	notificationH *notificationhandler.Handler,
	platformopsH *platformopshandler.Handler,
	ticketH *tickethandler.Handler,
	contentH *contenthandler.Handler,
	tenantH *tenanthandler.Handler,
	agentCredH *authhandler.AgentCredentialHandler,
	// new-domain:approuter-params
	signer *jwt.Signer,
	rt config.Runtime,
	agentCredentials *authrepo.PostgresAgentCredentialRepository,
	reg *metrics.Registry,
) http.Handler {
	return platform.NewRouter(
		userH,
		authH,
		iamH,
		notificationH,
		platformopsH,
		ticketH,
		contentH,
		tenantH,
		agentCredH,
		// new-domain:approuter-args
		signer,
		rt.AuthDevHeaders,
		agentCredentials,
		reg,
		rt.MetricsEnabled,
		rt.MetricsBearerToken,
		rt.GatewaySecret,
		platform.ProvideAgentRateLimiter(rt),
	)
}
