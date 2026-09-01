//go:build wireinject

package main

import (
	"context"

	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/idempotency"
	"github.com/williamlabdev/saas-forge/internal/user/handler"
	"github.com/williamlabdev/saas-forge/internal/user/repository"
	"github.com/williamlabdev/saas-forge/internal/user/service"

	// new-domain:imports
	"github.com/google/wire"
)

func InitializeApp(ctx context.Context, cfg config.User, rt config.Runtime) (*app, error) {
	wire.Build(
		providePool,
		provideEncryptor,
		provideIndexer,
		provideOutboxRepository,
		provideCredentialRepository,
		provideAuditRepository,
		provideMetricsRegistry,
		provideLoginLimiter,
		provideIdempotencyStore,
		provideTenantRepository,
		provideIAMRepository,
		provideIAMService,
		provideIAMAdminService,
		provideUserRepository,
		wire.Bind(new(repository.UserRepository), new(*repository.PostgresUserRepository)),
		wire.Bind(new(idempotency.RegistrationStore), new(*idempotency.PostgresRegistrationStore)),
		provideJWTSigner,
		provideAuthService,
		provideAuthorizer,
		provideMCPClient,
		provideOutboxWorker,
		service.NewUserService,
		handler.NewHandler,
		provideAuthHandler,
		provideIAMHandler,
		provideNotificationRepository,
		provideNotificationService,
		provideNotificationHandler,
		providePlatformAppRepository,
		providePlatformAppService,
		providePlatformConsoleRepository,
		providePlatformConsoleService,
		provideTenantAdminService,
		providePlatformAppHandler,
		provideTicketRepository,
		provideTicketService,
		provideTicketHandler,
		provideContentRepository,
		provideDeliveryCounter,
		provideMediaStore,
		provideContentService,
		provideContentHandler,
		provideTenantService,
		provideTenantHandler,
		provideAgentCredentialRepository,
		provideAgentCredentialService,
		provideAgentCredentialHandler,
		// new-domain:wire
		provideAppRouter,
		provideServer,
		wire.Struct(new(app), "Server", "Pool", "Worker", "Runtime", "IAM"),
	)
	return nil, nil
}
