package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
	platformopshandler "github.com/williamlabdev/saas-forge/internal/platformops/handler"
	platformopsrepo "github.com/williamlabdev/saas-forge/internal/platformops/repository"
	platformopsservice "github.com/williamlabdev/saas-forge/internal/platformops/service"
	tenanthandler "github.com/williamlabdev/saas-forge/internal/tenant/handler"
	tenantrepo "github.com/williamlabdev/saas-forge/internal/tenant/repository"
	tenantservice "github.com/williamlabdev/saas-forge/internal/tenant/service"
	userhandler "github.com/williamlabdev/saas-forge/internal/user/handler"
	userrepo "github.com/williamlabdev/saas-forge/internal/user/repository"
	userservice "github.com/williamlabdev/saas-forge/internal/user/service"

	contenthandler "github.com/williamlabdev/saas-forge/internal/cms/content/handler"
	"github.com/williamlabdev/saas-forge/internal/cms/content/mediatransform"
	contentrepo "github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	contentscheduler "github.com/williamlabdev/saas-forge/internal/cms/content/scheduler"
	contentservice "github.com/williamlabdev/saas-forge/internal/cms/content/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"

	// new-domain:imports
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// App holds wired HTTP handlers and dependencies for tests or custom entrypoints.
type App struct {
	Handler         http.Handler
	Pool            *pgxpool.Pool
	IAM             iamservice.IAMService
	Signer          *jwt.Signer
	AllowDevHeaders bool
	// DeliveryCounter buffers public delivery read volume. Run its flusher
	// (RunFlusher) alongside the server; ContentRepo is the sink.
	DeliveryCounter *contentservice.DeliveryCounter
	ContentRepo     contentrepo.ContentRepository
	// Scheduler executes due publish/unpublish schedules (ADR-017). Like the
	// outbox worker it is BUILT here and RUN by the caller: an e2e test that
	// never starts it gets a system where scheduling still works and nothing
	// ever fires, which is exactly the state most of those tests want.
	Scheduler *contentscheduler.Worker
	// MediaTransform renders image variants (ADR-019); nil when media is off.
	// Built, not run, for Scheduler's reason.
	MediaTransform *mediatransform.Worker
}

// mediaStore builds the object store when configured; nil means media off.
func mediaStore(rt config.Runtime) (objectstore.Store, error) {
	if rt.MediaEndpoint == "" || rt.MediaBucket == "" {
		return nil, nil
	}
	return objectstore.New(objectstore.Config{
		Endpoint: rt.MediaEndpoint, Region: rt.MediaRegion, Bucket: rt.MediaBucket,
		AccessKey: rt.MediaAccessKey, SecretKey: rt.MediaSecretKey, UseSSL: rt.MediaUseSSL,
		PublicEndpoint: rt.MediaPublicEndpoint, PublicUseSSL: rt.MediaPublicUseSSL,
	})
}

// BuildApp wires the modular monolith (same graph as cmd/server).
func BuildApp(ctx context.Context, cfg config.User, rt config.Runtime) (*App, error) {
	if err := config.ValidateRuntime(rt); err != nil {
		return nil, err
	}
	if err := validateAuthzMode(rt.AuthzMode); err != nil {
		return nil, err
	}
	if _, err := authz.ParseDecisionLogPolicy(rt.AuthzDecisionLog); err != nil {
		return nil, err
	}
	if err := config.ValidateUserSecrets(cfg, rt.IsProduction()); err != nil {
		return nil, err
	}

	pool, err := OpenVerifiedPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	enc, err := crypto.NewAESGCMEncryptor(cfg.EncryptionKey)
	if err != nil {
		pool.Close()
		return nil, err
	}
	idx, err := crypto.NewHMACBlindIndexer(cfg.BlindIndexPepper)
	if err != nil {
		pool.Close()
		return nil, err
	}

	obRepo := outbox.NewPostgresRepository(pool)
	credRepo := authrepo.NewPostgresCredentialRepository(pool)
	idemStore := idempotency.NewPostgresRegistrationStore(pool)
	tenantRepo := tenantrepo.NewPostgresTenantRepository(pool)
	userRepo := userrepo.NewPostgresUserRepository(pool, enc, obRepo, credRepo, idemStore, tenantRepo)

	iamRepo := iamrepo.NewPostgresIAMRepository(pool)
	iamSvc := iamservice.NewIAMService(iamRepo)

	reg := metrics.NewRegistry()
	auditRepo := authrepo.NewPostgresAuditRepository(pool)
	// WithDeliveryKey must match cmd/server's provideJWTSigner — this is the
	// second composition root, and a signer without the delivery key silently
	// refuses every delivery credential (401).
	signer := jwt.NewSigner(rt.JWTSecret, rt.JWTAccessTTL).
		WithDeliveryKey(rt.DeliveryJWTSecret).
		WithAgentTTL(rt.AgentTokenTTL)
	authSvc := authservice.NewAuthService(credRepo, idx, signer, iamSvc, tenantRepo, auditRepo, rt.JWTRefreshTTL)
	loginLimiter := ratelimit.NewIPLimiter(rt.AuthLoginRateLimit, rt.AuthLoginRateWindow)

	authorizer, err := provideAuthorizer(rt, iamSvc)
	if err != nil {
		pool.Close()
		return nil, err
	}

	userSvc := userservice.NewUserService(userRepo, idx, authorizer, authSvc, idemStore)
	userH := userhandler.NewHandler(userSvc)
	authH := authhandler.NewHandler(authSvc, loginLimiter, reg)
	iamAdmin := iamservice.NewIAMAdminService(iamRepo, authorizer)
	iamH := iamhandler.NewHandler(iamAdmin)

	notifRepo := notificationrepo.NewPostgresNotificationRepository(pool)
	notifSvc := notificationservice.NewNotificationService(notifRepo, authorizer)
	notifH := notificationhandler.NewHandler(notifSvc)
	// The one EmailSender/SystemNotifier both composition roots wire (2.5a) —
	// see cmd/server/providers.go's provideEmailSender/provideSystemNotifier
	// for why this is built once and shared across all three trigger points
	// (proposal/pending-review, schedule outcomes, webhook dead-letter) rather
	// than once per consumer.
	systemNotifier := notificationservice.NewSystemNotifier(notifRepo, tenantRepo, notificationservice.NewNoopEmailSender())

	platformRepo := platformopsrepo.NewPostgresPlatformAppRepository(pool)
	platformSvc := platformopsservice.NewPlatformAppService(platformRepo, authorizer)
	consoleRepo := platformopsrepo.NewPostgresPlatformConsoleRepository(pool)
	consoleSvc := platformopsservice.NewPlatformConsoleService(consoleRepo, authorizer)
	tenantAdminSvc := platformopsservice.NewTenantAdminService(tenantRepo, authorizer)
	platformH := platformopshandler.NewHandler(platformSvc, consoleSvc, tenantAdminSvc)

	contentRepo := contentrepo.NewPostgresContentRepository(pool, obRepo)
	// Public delivery reads are counted in process and flushed periodically —
	// never written per request (see contentservice.DeliveryCounter). The caller
	// owns the flusher's lifetime via App.DeliveryCounter.
	deliveryCounter := contentservice.NewDeliveryCounter()
	contentSvc := contentservice.NewContentServiceWithDelivery(contentRepo, authorizer, contentPlanResolver(tenantRepo), deliveryCounter)
	// Media is optional: without an endpoint the media endpoints answer 501
	// rather than the deployment failing to start (ADR-005).
	var mediaTransform *mediatransform.Worker
	if store, err := mediaStore(rt); err != nil {
		return nil, err
	} else if store != nil {
		contentSvc = contentservice.WithMediaStore(contentSvc, store)
		mediaTransform = mediatransform.NewWorker(mediatransform.NewStore(contentRepo), store)
	}
	// Preview links are optional in exactly the same way, and gated on the same
	// fact that gates the delivery edge itself: without a delivery key there is
	// no key that may sign a delivery-shaped credential, so there is no preview
	// to enable. Conditioned here rather than inside the service so the "off"
	// state is a nil dependency the endpoint can report as 501, not an error
	// thrown from the signer after the caller was told the feature exists.
	if len(rt.DeliveryJWTSecret) > 0 {
		contentSvc = contentservice.WithPreviewLinks(contentSvc, signer)
	}
	// A filed schema proposal has a seven-day TTL and a queue only an approver
	// who opens the page can see, so without this the normal outcome is expiry
	// (ADR-013 §3 step 8). A published entry gaining an unpublished draft is
	// the same shape (ADR-023). Wired unconditionally: the notification plane
	// is already required above, so unlike media and previews there is no
	// "off" deployment to represent.
	contentSvc = contentservice.WithNotifier(contentSvc, systemNotifier)
	contentH := contenthandler.NewHandler(contentSvc)
	tenantSvc := tenantservice.NewTenantService(tenantRepo, idx, enc, authorizer, userEmailLookup{userRepo}, credRepo)
	tenantH := tenanthandler.NewHandler(tenantSvc)
	// The repository is passed to the router TWICE over, in two roles: as the
	// service's store, and as the middleware's revocation lookup. They are the
	// same rows read for different reasons — one writes the registry, the other
	// asks it whether a bearer is still welcome — and keeping one source is
	// what makes revocation take effect on the next request rather than
	// eventually.
	agentCredRepo := authrepo.NewPostgresAgentCredentialRepository(pool)
	agentCredH := authhandler.NewAgentCredentialHandler(
		authservice.NewAgentCredentialService(agentCredRepo, signer, authorizer),
	)
	// new-domain:buildapp-construct
	handler := NewRouter(
		userH,
		authH,
		iamH,
		notifH,
		platformH,
		contentH,
		tenantH,
		agentCredH,
		// new-domain:buildapp-args
		signer,
		rt.AuthDevHeaders,
		agentCredRepo,
		reg,
		rt.MetricsEnabled,
		rt.MetricsBearerToken,
		rt.GatewaySecret,
		ProvideAgentRateLimiter(rt),
		pool,
	)

	return &App{
		Handler:         handler,
		Pool:            pool,
		IAM:             iamSvc,
		Signer:          signer,
		AllowDevHeaders: rt.AuthDevHeaders,
		DeliveryCounter: deliveryCounter,
		ContentRepo:     contentRepo,
		Scheduler:       contentscheduler.NewWorker(contentscheduler.NewStore(contentRepo)).WithNotifier(systemNotifier),
		MediaTransform:  mediaTransform,
	}, nil
}

// userEmailLookup adapts the user module's repository to
// tenantservice.MemberEmailLookup (2.2c member roster), so the tenant module
// stays free of a compile-time dependency on user/domain — narrow interfaces
// defined by the consumer, wired only here at the composition root.
type userEmailLookup struct {
	repo userrepo.UserRepository
}

func (u userEmailLookup) EmailByID(ctx context.Context, userID uuid.UUID) (string, error) {
	usr, err := u.repo.ByID(ctx, userID)
	if err != nil {
		return "", err
	}
	return usr.Email, nil
}

// provideAuthorizer mirrors cmd/server/providers.go's function of the same
// name — the two composition roots each pick the AUTHZ_MODE-selected engine
// independently, then both call the single shared authz.WrapWithDecisionLog
// so the AUTHZ_DECISION_LOG behaviour (off|deny|all) cannot drift between
// them even though engine selection is still implemented twice.
func provideAuthorizer(rt config.Runtime, iam iamservice.IAMService) (authz.Authorizer, error) {
	var (
		inner authz.Authorizer
		mode  string
		err   error
	)
	switch strings.ToLower(rt.AuthzMode) {
	case "opa":
		mode = authz.ModeOPA
		inner, err = authz.NewOPAAuthorizer(iamservice.FactsLoader{Svc: iam})
	case "rbac":
		mode = authz.ModeRBAC
		inner = authz.NewRBACAuthorizer()
	default:
		mode = authz.ModeAllow
		inner = authz.NewAllowAllAuthorizer()
	}
	if err != nil {
		return nil, err
	}

	return authz.WrapWithDecisionLog(inner, mode, rt.AuthzDecisionLog)
}

func validateAuthzMode(mode string) error {
	switch strings.ToLower(mode) {
	case "allow", "rbac", "opa":
		return nil
	default:
		return fmt.Errorf("config: unknown AUTHZ_MODE %q", mode)
	}
}

// ProvideMCPClient builds MCP client from runtime config.
func ProvideMCPClient(rt config.Runtime) mcp.Client {
	if rt.MCPBaseURL != "" {
		return mcp.NewHTTPClient(rt.MCPBaseURL)
	}
	return mcp.NoopClient{}
}

// ProvideOutboxWorker builds the outbox worker sharing the HTTP metrics registry.
// Content events route to tenant-registered webhooks; the content repository is
// the directory (it owns the registry table and its RLS scoping).
// ProvideAgentRateLimiter builds the per-agent limiter from config (ADR-013
// 補裁 S-2 題一). A zero AGENT_RATE_LIMIT yields a limiter that allows
// everything rather than a nil one, so callers never have to decide what a
// missing limiter means — and so an interface holding a typed nil, which is not
// nil, cannot become the way this layer silently switches itself off.
func ProvideAgentRateLimiter(rt config.Runtime) *ratelimit.IPLimiter {
	return ratelimit.NewIPLimiter(rt.AgentRateLimit, rt.AgentRateWindow)
}

func ProvideOutboxWorker(pool *pgxpool.Pool, rt config.Runtime, reg *metrics.Registry) *outbox.Worker {
	ob := outbox.NewPostgresRepository(pool)
	dir := contentrepo.NewPostgresContentRepository(pool, ob)
	// This function builds its own notification/tenant repositories rather than
	// taking a shared *notificationservice.SystemNotifier: it is a standalone
	// entry point BuildApp does not call (see test/e2e/platform_test.go), so
	// there is no already-built notifier to share, only the pool.
	notifRepo := notificationrepo.NewPostgresNotificationRepository(pool)
	tenantRepo := tenantrepo.NewPostgresTenantRepository(pool)
	notifier := notificationservice.NewSystemNotifier(notifRepo, tenantRepo, notificationservice.NewNoopEmailSender())
	return outbox.NewWorker(ob, ProvideMCPClient(rt), rt.OutboxBatchSize, rt.OutboxMaxRetries, rt.OutboxStaleThreshold, reg).
		WithContentWebhooks(dir, outbox.NewHTTPWebhookSender()).
		WithNotifier(notifier)
}
