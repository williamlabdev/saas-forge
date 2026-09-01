package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	authhandler "github.com/williamlabdev/saas-forge/internal/auth/handler"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	authrepo "github.com/williamlabdev/saas-forge/internal/auth/repository"
	authservice "github.com/williamlabdev/saas-forge/internal/auth/service"
	iamhandler "github.com/williamlabdev/saas-forge/internal/iam/handler"
	iamrepo "github.com/williamlabdev/saas-forge/internal/iam/repository"
	iamservice "github.com/williamlabdev/saas-forge/internal/iam/service"
	notificationhandler "github.com/williamlabdev/saas-forge/internal/notification/handler"
	notificationrepo "github.com/williamlabdev/saas-forge/internal/notification/repository"
	notificationservice "github.com/williamlabdev/saas-forge/internal/notification/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	"github.com/williamlabdev/saas-forge/internal/pkg/idempotency"
	"github.com/williamlabdev/saas-forge/internal/pkg/mcp"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
	"github.com/williamlabdev/saas-forge/internal/platform"
	platformopshandler "github.com/williamlabdev/saas-forge/internal/platformops/handler"
	platformopsrepo "github.com/williamlabdev/saas-forge/internal/platformops/repository"
	platformopsservice "github.com/williamlabdev/saas-forge/internal/platformops/service"
	tenanthandler "github.com/williamlabdev/saas-forge/internal/tenant/handler"
	tenantrepo "github.com/williamlabdev/saas-forge/internal/tenant/repository"
	tenantservice "github.com/williamlabdev/saas-forge/internal/tenant/service"
	tickethandler "github.com/williamlabdev/saas-forge/internal/ticket/handler"
	ticketrepo "github.com/williamlabdev/saas-forge/internal/ticket/repository"
	ticketservice "github.com/williamlabdev/saas-forge/internal/ticket/service"
	userrepo "github.com/williamlabdev/saas-forge/internal/user/repository"

	contenthandler "github.com/williamlabdev/saas-forge/internal/cms/content/handler"
	contentrepo "github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	contentservice "github.com/williamlabdev/saas-forge/internal/cms/content/service"

	// new-domain:imports
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func providePool(ctx context.Context, cfg config.User) (*pgxpool.Pool, error) {
	// Not pgxpool.New: the migration check rides along with pool construction so
	// this root and internal/platform's cannot drift apart. See ADR-012 §4.
	return platform.OpenVerifiedPool(ctx, cfg.DatabaseURL)
}

func provideEncryptor(cfg config.User) (crypto.FieldEncryptor, error) {
	return crypto.NewAESGCMEncryptor(cfg.EncryptionKey)
}

func provideIndexer(cfg config.User) (crypto.BlindIndexer, error) {
	return crypto.NewHMACBlindIndexer(cfg.BlindIndexPepper)
}

func provideOutboxRepository(pool *pgxpool.Pool) *outbox.PostgresRepository {
	return outbox.NewPostgresRepository(pool)
}

func provideCredentialRepository(pool *pgxpool.Pool) *authrepo.PostgresCredentialRepository {
	return authrepo.NewPostgresCredentialRepository(pool)
}

func provideAuditRepository(pool *pgxpool.Pool) *authrepo.PostgresAuditRepository {
	return authrepo.NewPostgresAuditRepository(pool)
}

func provideMetricsRegistry() *metrics.Registry {
	return metrics.NewRegistry()
}

func provideLoginLimiter(rt config.Runtime) *ratelimit.IPLimiter {
	return ratelimit.NewIPLimiter(rt.AuthLoginRateLimit, rt.AuthLoginRateWindow)
}

func provideIAMRepository(pool *pgxpool.Pool) *iamrepo.PostgresIAMRepository {
	return iamrepo.NewPostgresIAMRepository(pool)
}

func provideIAMService(repo *iamrepo.PostgresIAMRepository) iamservice.IAMService {
	return iamservice.NewIAMService(repo)
}

func provideIAMAdminService(repo *iamrepo.PostgresIAMRepository, auth authz.Authorizer) iamservice.IAMAdminService {
	return iamservice.NewIAMAdminService(repo, auth)
}

func provideIdempotencyStore(pool *pgxpool.Pool) *idempotency.PostgresRegistrationStore {
	return idempotency.NewPostgresRegistrationStore(pool)
}

func provideTenantRepository(pool *pgxpool.Pool) *tenantrepo.PostgresTenantRepository {
	return tenantrepo.NewPostgresTenantRepository(pool)
}

func provideUserRepository(
	pool *pgxpool.Pool,
	enc crypto.FieldEncryptor,
	ob *outbox.PostgresRepository,
	creds *authrepo.PostgresCredentialRepository,
	idem *idempotency.PostgresRegistrationStore,
	tenants *tenantrepo.PostgresTenantRepository,
) *userrepo.PostgresUserRepository {
	return userrepo.NewPostgresUserRepository(pool, enc, ob, creds, idem, tenants)
}

func provideJWTSigner(rt config.Runtime) *jwt.Signer {
	// WithDeliveryKey is a no-op when DeliveryJWTSecret is unset (dev single-key
	// mode); production refuses to start without it (see ValidateRuntime).
	// WithAgentTTL must match internal/platform's BuildApp — the second
	// composition root. Diverging here would not fail to start: it would mint
	// agent credentials with the wrong lifetime, which nothing observes until an
	// agent stops working (too short) or a leaked token outlives its welcome
	// (too long).
	return jwt.NewSigner(rt.JWTSecret, rt.JWTAccessTTL).
		WithDeliveryKey(rt.DeliveryJWTSecret).
		WithAgentTTL(rt.AgentTokenTTL)
}

func provideAgentCredentialRepository(pool *pgxpool.Pool) *authrepo.PostgresAgentCredentialRepository {
	return authrepo.NewPostgresAgentCredentialRepository(pool)
}

func provideAgentCredentialService(
	repo *authrepo.PostgresAgentCredentialRepository,
	signer *jwt.Signer,
	authorizer authz.Authorizer,
) authservice.AgentCredentialService {
	return authservice.NewAgentCredentialService(repo, signer, authorizer)
}

func provideAgentCredentialHandler(svc authservice.AgentCredentialService) *authhandler.AgentCredentialHandler {
	return authhandler.NewAgentCredentialHandler(svc)
}

func provideAuthService(
	repo *authrepo.PostgresCredentialRepository,
	idx crypto.BlindIndexer,
	signer *jwt.Signer,
	iam iamservice.IAMService,
	tenants *tenantrepo.PostgresTenantRepository,
	auditRepo *authrepo.PostgresAuditRepository,
	rt config.Runtime,
) authservice.AuthService {
	return authservice.NewAuthService(repo, idx, signer, iam, tenants, auditRepo, rt.JWTRefreshTTL)
}

func provideAuthorizer(rt config.Runtime, iam iamservice.IAMService) (authz.Authorizer, error) {
	switch strings.ToLower(rt.AuthzMode) {
	case "opa":
		return authz.NewOPAAuthorizer(iamservice.FactsLoader{Svc: iam})
	case "rbac":
		return authz.NewRBACAuthorizer(), nil
	default:
		return authz.NewAllowAllAuthorizer(), nil
	}
}

func provideMCPClient(rt config.Runtime) mcp.Client {
	if rt.MCPBaseURL != "" {
		return mcp.NewHTTPClient(rt.MCPBaseURL)
	}
	return mcp.NoopClient{}
}

func provideOutboxWorker(ob *outbox.PostgresRepository, client mcp.Client, reg *metrics.Registry, rt config.Runtime, pool *pgxpool.Pool) *outbox.Worker {
	// Content events route to tenant-registered webhooks; the content repository
	// is the directory (it owns the registry table and its RLS scoping).
	dir := contentrepo.NewPostgresContentRepository(pool, ob)
	return outbox.NewWorker(ob, client, rt.OutboxBatchSize, rt.OutboxMaxRetries, rt.OutboxStaleThreshold, reg).
		WithContentWebhooks(dir, outbox.NewHTTPWebhookSender())
}

func provideAuthHandler(svc authservice.AuthService, limiter *ratelimit.IPLimiter, reg *metrics.Registry) *authhandler.Handler {
	return authhandler.NewHandler(svc, limiter, reg)
}

func provideIAMHandler(svc iamservice.IAMAdminService) *iamhandler.Handler {
	return iamhandler.NewHandler(svc)
}

func provideNotificationRepository(pool *pgxpool.Pool) *notificationrepo.PostgresNotificationRepository {
	return notificationrepo.NewPostgresNotificationRepository(pool)
}

func provideNotificationService(repo *notificationrepo.PostgresNotificationRepository, auth authz.Authorizer) notificationservice.NotificationService {
	return notificationservice.NewNotificationService(repo, auth)
}

func provideNotificationHandler(svc notificationservice.NotificationService) *notificationhandler.Handler {
	return notificationhandler.NewHandler(svc)
}

func providePlatformAppRepository(pool *pgxpool.Pool) *platformopsrepo.PostgresPlatformAppRepository {
	return platformopsrepo.NewPostgresPlatformAppRepository(pool)
}

func providePlatformAppService(
	repo *platformopsrepo.PostgresPlatformAppRepository,
	auth authz.Authorizer,
) platformopsservice.PlatformAppService {
	return platformopsservice.NewPlatformAppService(repo, auth)
}

func providePlatformConsoleRepository(pool *pgxpool.Pool) *platformopsrepo.PostgresPlatformConsoleRepository {
	return platformopsrepo.NewPostgresPlatformConsoleRepository(pool)
}

func providePlatformConsoleService(
	repo *platformopsrepo.PostgresPlatformConsoleRepository,
	auth authz.Authorizer,
) platformopsservice.PlatformConsoleService {
	return platformopsservice.NewPlatformConsoleService(repo, auth)
}

func provideTenantAdminService(
	tenants *tenantrepo.PostgresTenantRepository,
	auth authz.Authorizer,
) platformopsservice.TenantAdminService {
	return platformopsservice.NewTenantAdminService(tenants, auth)
}

func providePlatformAppHandler(
	svc platformopsservice.PlatformAppService,
	console platformopsservice.PlatformConsoleService,
	tenantAdmin platformopsservice.TenantAdminService,
) *platformopshandler.Handler {
	return platformopshandler.NewHandler(svc, console, tenantAdmin)
}

func provideTicketRepository(pool *pgxpool.Pool, ob *outbox.PostgresRepository) *ticketrepo.PostgresTicketRepository {
	return ticketrepo.NewPostgresTicketRepository(pool, ob)
}

func provideTicketService(repo *ticketrepo.PostgresTicketRepository, auth authz.Authorizer) ticketservice.TicketService {
	return ticketservice.NewTicketService(repo, auth)
}

func provideTicketHandler(svc ticketservice.TicketService) *tickethandler.Handler {
	return tickethandler.NewHandler(svc)
}

func provideContentRepository(pool *pgxpool.Pool, ob *outbox.PostgresRepository) *contentrepo.PostgresContentRepository {
	return contentrepo.NewPostgresContentRepository(pool, ob)
}

// provideDeliveryCounter buffers public delivery reads in process; the flusher
// (started in main) folds them into the daily bucket. Counting per request in
// the DB would put a write on the read-optimised public path (ADR-004).
func provideDeliveryCounter() *contentservice.DeliveryCounter {
	return contentservice.NewDeliveryCounter()
}

// provideMediaStore returns nil when media is not configured — the media
// endpoints then answer 501 instead of the process refusing to start (ADR-005).
func provideMediaStore(rt config.Runtime) (objectstore.Store, error) {
	if rt.MediaEndpoint == "" || rt.MediaBucket == "" {
		return nil, nil
	}
	return objectstore.New(objectstore.Config{
		Endpoint: rt.MediaEndpoint, Region: rt.MediaRegion, Bucket: rt.MediaBucket,
		AccessKey: rt.MediaAccessKey, SecretKey: rt.MediaSecretKey, UseSSL: rt.MediaUseSSL,
	})
}

func provideContentService(rt config.Runtime, repo *contentrepo.PostgresContentRepository, auth authz.Authorizer, tenants *tenantrepo.PostgresTenantRepository, counter *contentservice.DeliveryCounter, store objectstore.Store, signer *jwt.Signer) contentservice.ContentService {
	svc := contentservice.NewContentServiceWithDelivery(repo, auth, platform.ContentPlanResolver(tenants), counter)
	if store != nil {
		svc = contentservice.WithMediaStore(svc, store)
	}
	// Same condition as platform.BuildApp: no delivery key, no preview links.
	if len(rt.DeliveryJWTSecret) > 0 {
		svc = contentservice.WithPreviewLinks(svc, signer)
	}
	return svc
}

func provideContentHandler(svc contentservice.ContentService) *contenthandler.Handler {
	return contenthandler.NewHandler(svc)
}

func provideTenantService(
	repo *tenantrepo.PostgresTenantRepository,
	idx crypto.BlindIndexer,
	auth authz.Authorizer,
) tenantservice.TenantService {
	return tenantservice.NewTenantService(repo, idx, auth)
}

func provideTenantHandler(svc tenantservice.TenantService) *tenanthandler.Handler {
	return tenanthandler.NewHandler(svc)
}

// new-domain:providers
func provideServer(cfg config.User, router http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func validateRuntime(rt config.Runtime) error {
	if err := config.ValidateRuntime(rt); err != nil {
		return err
	}
	switch strings.ToLower(rt.AuthzMode) {
	case "allow", "rbac", "opa":
		return nil
	default:
		return fmt.Errorf("config: unknown AUTHZ_MODE %q (allow|rbac|opa)", rt.AuthzMode)
	}
}

func bootstrapAdmin(ctx context.Context, iam iamservice.IAMService, rt config.Runtime) {
	if rt.BootstrapAdminID == "" {
		return
	}
	id, err := uuid.Parse(rt.BootstrapAdminID)
	if err != nil {
		fmt.Printf("bootstrap admin: invalid BOOTSTRAP_ADMIN_USER_ID: %v\n", err)
		return
	}
	if err := iam.AssignRoleByName(ctx, id, "admin"); err != nil {
		fmt.Printf("bootstrap admin: %v\n", err)
		return
	}
	fmt.Printf("bootstrap admin: assigned admin to %s\n", id)
}
