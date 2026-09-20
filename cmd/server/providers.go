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
	userrepo "github.com/williamlabdev/saas-forge/internal/user/repository"

	contenthandler "github.com/williamlabdev/saas-forge/internal/cms/content/handler"
	"github.com/williamlabdev/saas-forge/internal/cms/content/mediatransform"
	contentrepo "github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	contentscheduler "github.com/williamlabdev/saas-forge/internal/cms/content/scheduler"
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

// provideAuthorizer builds the AUTHZ_MODE-selected engine and wraps it via
// authz.WrapWithDecisionLog so every decision is logged per
// AUTHZ_DECISION_LOG (off|deny|all). internal/platform/app.go's own
// provideAuthorizer mirrors this switch and calls the same shared helper, so
// the decision-log wrapping itself cannot drift between the two composition
// roots even though engine selection is still implemented twice.
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

func provideMCPClient(rt config.Runtime) mcp.Client {
	if rt.MCPBaseURL != "" {
		return mcp.NewHTTPClient(rt.MCPBaseURL)
	}
	return mcp.NoopClient{}
}

func provideOutboxWorker(ob *outbox.PostgresRepository, client mcp.Client, reg *metrics.Registry, rt config.Runtime, pool *pgxpool.Pool, notifier *notificationservice.SystemNotifier) *outbox.Worker {
	// Content events route to tenant-registered webhooks; the content repository
	// is the directory (it owns the registry table and its RLS scoping).
	dir := contentrepo.NewPostgresContentRepository(pool, ob)
	return outbox.NewWorker(ob, client, rt.OutboxBatchSize, rt.OutboxMaxRetries, rt.OutboxStaleThreshold, reg).
		WithContentWebhooks(dir, outbox.NewHTTPWebhookSender()).
		// ADR-023 trigger 4c: a content event's dead-lettered webhook notifies
		// the tenant's owner/admin. Wired unconditionally, same as the content
		// service's notifier below — the notification plane is not optional.
		WithNotifier(notifier)
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

// provideEmailSender is the one EmailSender both composition roots wire
// (2.5a). No real provider is integrated — see NoopEmailSender's doc — but
// the constructor call happens here, at startup, rather than being deferred,
// so the "email not configured" log line fires exactly once per process.
func provideEmailSender() notificationservice.EmailSender {
	return notificationservice.NewNoopEmailSender()
}

// provideSystemNotifier wires the one notifier all three 2.5a trigger points
// share: the content service's pending-review/proposal hook, the scheduler's
// terminal-state hook, and the outbox's dead-letter hook. Built once here
// (not once per consumer) — see internal/platform/app.go's BuildApp for the
// same choice in the other composition root — because the write path
// (notification repository, tenant roster, email sender) is identical for
// all three, and there is no reason for a "which one is this call for"
// switch inside SystemNotifier itself.
func provideSystemNotifier(
	repo *notificationrepo.PostgresNotificationRepository,
	tenants *tenantrepo.PostgresTenantRepository,
	email notificationservice.EmailSender,
) *notificationservice.SystemNotifier {
	return notificationservice.NewSystemNotifier(repo, tenants, email)
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

func provideContentRepository(pool *pgxpool.Pool, ob *outbox.PostgresRepository) *contentrepo.PostgresContentRepository {
	return contentrepo.NewPostgresContentRepository(pool, ob)
}

// provideDeliveryCounter buffers public delivery reads in process; the flusher
// (started in main) folds them into the daily bucket. Counting per request in
// the DB would put a write on the read-optimised public path (ADR-004).
func provideDeliveryCounter() *contentservice.DeliveryCounter {
	return contentservice.NewDeliveryCounter()
}

// provideScheduler builds the content scheduler worker (ADR-017). main runs it
// alongside the outbox worker; the two share a shutdown context.
//
// It takes the content repository rather than the pool because everything it
// does — the cross-tenant due scan, the per-tenant claim transaction, the
// publish itself — is already spelled there under the RLS scoping that owns
// those tables.
func provideScheduler(repo *contentrepo.PostgresContentRepository, notifier *notificationservice.SystemNotifier) *contentscheduler.Worker {
	// ADR-023 trigger 4b: the requester hears about a schedule's terminal
	// state (succeeded/failed/stale) — see worker.go's execute/markFailed.
	return contentscheduler.NewWorker(contentscheduler.NewStore(repo)).WithNotifier(notifier)
}

// provideMediaTransform returns nil when media is not configured: with no
// bucket there are no bytes to render, and the rows the worker would poll
// for can only be created by CompleteMediaUpload, which answers 501 in that
// deployment anyway (ADR-019).
func provideMediaTransform(repo *contentrepo.PostgresContentRepository, store objectstore.Store) *mediatransform.Worker {
	if store == nil {
		return nil
	}
	return mediatransform.NewWorker(mediatransform.NewStore(repo), store)
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
		PublicEndpoint: rt.MediaPublicEndpoint, PublicUseSSL: rt.MediaPublicUseSSL,
	})
}

func provideContentService(rt config.Runtime, repo *contentrepo.PostgresContentRepository, auth authz.Authorizer, tenants *tenantrepo.PostgresTenantRepository, counter *contentservice.DeliveryCounter, store objectstore.Store, signer *jwt.Signer, notifier *notificationservice.SystemNotifier) contentservice.ContentService {
	svc := contentservice.NewContentServiceWithDelivery(repo, auth, platform.ContentPlanResolver(tenants), counter)
	if store != nil {
		svc = contentservice.WithMediaStore(svc, store)
	}
	// Same condition as platform.BuildApp: no delivery key, no preview links.
	if len(rt.DeliveryJWTSecret) > 0 {
		svc = contentservice.WithPreviewLinks(svc, signer)
	}
	// A filed schema proposal or a published entry gaining an unpublished draft
	// each has a queue only someone who opens the page would otherwise see
	// (ADR-013 §3 step 8, ADR-023). Wired unconditionally, same as
	// internal/platform's BuildApp: the notification plane is always present,
	// unlike media/previews which have a real "off" deployment state.
	svc = contentservice.WithNotifier(svc, notifier)
	return svc
}

func provideContentHandler(svc contentservice.ContentService) *contenthandler.Handler {
	return contenthandler.NewHandler(svc)
}

// userEmailLookup adapts the user repository to tenantservice.MemberEmailLookup
// (2.2c member roster) — narrow interface defined by the consumer, wired only
// at a composition root. Duplicated (not shared) with the identical adapter
// in internal/platform/app.go: this file is wire's OTHER composition root,
// and this codebase's convention (see provideJWTSigner's comment above) is to
// keep such roots independently correct rather than reach across them.
type userEmailLookup struct {
	repo *userrepo.PostgresUserRepository
}

func (u userEmailLookup) EmailByID(ctx context.Context, userID uuid.UUID) (string, error) {
	usr, err := u.repo.ByID(ctx, userID)
	if err != nil {
		return "", err
	}
	return usr.Email, nil
}

func provideUserEmailLookup(repo *userrepo.PostgresUserRepository) tenantservice.MemberEmailLookup {
	return userEmailLookup{repo: repo}
}

func provideTenantService(
	repo *tenantrepo.PostgresTenantRepository,
	idx crypto.BlindIndexer,
	enc crypto.FieldEncryptor,
	auth authz.Authorizer,
	emails tenantservice.MemberEmailLookup,
	creds *authrepo.PostgresCredentialRepository,
) tenantservice.TenantService {
	return tenantservice.NewTenantService(repo, idx, enc, auth, emails, creds)
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
	default:
		return fmt.Errorf("config: unknown AUTHZ_MODE %q (allow|rbac|opa)", rt.AuthzMode)
	}
	if _, err := authz.ParseDecisionLogPolicy(rt.AuthzDecisionLog); err != nil {
		return err
	}
	return nil
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
