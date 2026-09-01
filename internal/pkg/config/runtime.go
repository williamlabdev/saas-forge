package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// Runtime holds cross-cutting runtime settings (outbox, MCP, authz, JWT).
type Runtime struct {
	// AppEnv declares the deployment environment. It defaults to "production",
	// which arms the hard startup guards (TKT-R2). Other values (development,
	// staging, ...) disarm them.
	//
	// The default is deliberately the safe end, not the convenient one. Every
	// guard in ValidateRuntime is conditioned on this field, so a default of
	// "development" would have meant the protection only reached operators who
	// already knew to declare production — while the operator who copies
	// .env.example, runs `docker compose up` and points a domain at it, the one
	// the guards exist for, never trips them. Both .env.example and
	// docker-compose.yml set APP_ENV=development explicitly, so local
	// development is unchanged; what changed is that running unguarded is now a
	// line someone wrote, not a default they never saw.
	AppEnv             string
	AuthzMode          string
	MCPBaseURL         string
	OutboxPollInterval time.Duration
	OutboxBatchSize    int
	OutboxMaxRetries   int
	// OutboxStaleThreshold is how long a row may sit in 'processing' before the
	// reaper reclaims it to 'pending' (crash/shutdown recovery). Keep it well
	// above the poll interval and expected delivery time.
	OutboxStaleThreshold time.Duration
	JWTSecret            []byte
	DeliveryJWTSecret    []byte
	JWTAccessTTL         time.Duration
	JWTRefreshTTL        time.Duration
	// AgentTokenTTL is how long a minted agent credential lives (ADR-013,
	// ruled 2026-08-06). Separate from JWTAccessTTL because an agent runs
	// unattended: it has no refresh path and nobody to log it back in, so
	// inheriting the 15-minute access TTL meant losing the credential four
	// times an hour. Long TTLs are only tenable because the credential is
	// revocable — see agent_credentials.
	AgentTokenTTL       time.Duration
	BootstrapAdminID    string
	AuthDevHeaders      bool
	AuthLoginRateLimit  int
	AuthLoginRateWindow time.Duration
	MetricsEnabled      bool
	MetricsBearerToken  string
	// TrustProxyHeaders lets requestctx.ClientIP believe X-Forwarded-For.
	// Keep false unless a trusted proxy strips client-supplied values.
	TrustProxyHeaders bool
	// AgentRateLimit caps requests per AgentRateWindow for ONE agent credential
	// (ADR-013 補裁 S-2 題一). Zero — the default — disables the cap.
	//
	// 🔴 DEFAULTING TO OFF IS A DECISION, not an oversight. The ruling that
	// created this layer separated it from the number on purpose: "每支 agent
	// 每分鐘 N 次" is an EXTERNAL PROMISE, and this limiter cannot keep one.
	// `ip_limiter.go:8` says `in-process; Redis in M1` and Redis has never
	// landed, so under N replicas the real ceiling is N × limit. Shipping a
	// default would publish a number the system does not enforce; an operator
	// who sets one is choosing a backstop for their own topology, which is a
	// different and honest thing. LogProductionWarnings says so out loud when
	// it is unset.
	AgentRateLimit  int
	AgentRateWindow time.Duration
	// GatewaySecret, when set, requires every request (except /health) to
	// carry a matching X-Gateway-Secret header (TKT-R7) — a defense against
	// the domain API being reached directly on :8080. Empty = disabled.
	GatewaySecret string
	// Object storage for media (ADR-005). S3-compatible: AWS S3 / Cloudflare R2
	// / Backblaze B2 / MinIO. Empty endpoint = media feature off (the endpoints
	// answer 501) rather than a half-configured store failing at request time.
	MediaEndpoint  string
	MediaRegion    string
	MediaBucket    string
	MediaAccessKey string
	MediaSecretKey string
	MediaUseSSL    bool
}

func LoadRuntimeFromEnv() Runtime {
	rt := Runtime{
		AppEnv:             strings.ToLower(strings.TrimSpace(getenv("APP_ENV", "production"))),
		AuthzMode:          getenv("AUTHZ_MODE", "allow"),
		MCPBaseURL:         os.Getenv("MCP_BASE_URL"),
		OutboxPollInterval: 2 * time.Second,
		OutboxBatchSize:    10,
	}
	if sec, err := strconv.Atoi(getenv("OUTBOX_POLL_SECONDS", "2")); err == nil && sec > 0 {
		rt.OutboxPollInterval = time.Duration(sec) * time.Second
	}
	if n, err := strconv.Atoi(getenv("OUTBOX_BATCH_SIZE", "10")); err == nil && n > 0 {
		rt.OutboxBatchSize = n
	}
	if n, err := strconv.Atoi(getenv("OUTBOX_MAX_RETRIES", "5")); err == nil && n > 0 {
		rt.OutboxMaxRetries = n
	} else {
		rt.OutboxMaxRetries = 5
	}
	rt.OutboxStaleThreshold = 60 * time.Second
	if sec, err := strconv.Atoi(getenv("OUTBOX_STALE_SECONDS", "60")); err == nil && sec > 0 {
		rt.OutboxStaleThreshold = time.Duration(sec) * time.Second
	}
	rt.JWTAccessTTL = 15 * time.Minute
	if m, err := strconv.Atoi(getenv("JWT_ACCESS_TTL_MINUTES", "15")); err == nil && m > 0 {
		rt.JWTAccessTTL = time.Duration(m) * time.Minute
	}
	rt.JWTRefreshTTL = 7 * 24 * time.Hour
	if d, err := strconv.Atoi(getenv("JWT_REFRESH_TTL_DAYS", "7")); err == nil && d > 0 {
		rt.JWTRefreshTTL = time.Duration(d) * 24 * time.Hour
	}
	rt.AgentTokenTTL = 30 * 24 * time.Hour
	if d, err := strconv.Atoi(getenv("AGENT_TOKEN_TTL_DAYS", "30")); err == nil && d > 0 {
		rt.AgentTokenTTL = time.Duration(d) * 24 * time.Hour
	}
	if hexSecret := os.Getenv("JWT_SECRET_HEX"); hexSecret != "" {
		if b, err := hex.DecodeString(hexSecret); err == nil && len(b) >= 32 {
			rt.JWTSecret = b
		}
	}
	// Separate signing key for public delivery credentials (ADR-004). Optional:
	// unset means single-key mode, fine for dev. In production it is required —
	// the public edge holds whichever key it signs with, and that key must not
	// be able to mint an owner token. See ValidateRuntime.
	if hexSecret := os.Getenv("DELIVERY_JWT_SECRET_HEX"); hexSecret != "" {
		if b, err := hex.DecodeString(hexSecret); err == nil && len(b) >= 32 {
			rt.DeliveryJWTSecret = b
		}
	}
	rt.BootstrapAdminID = os.Getenv("BOOTSTRAP_ADMIN_USER_ID")
	rt.AuthDevHeaders = true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TRUST_PROXY_HEADERS"))); v == "true" || v == "1" {
		rt.TrustProxyHeaders = true
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("AUTH_DEV_HEADERS"))); v == "false" || v == "0" {
		rt.AuthDevHeaders = false
	}
	rt.AuthLoginRateLimit = 10
	if n, err := strconv.Atoi(getenv("AUTH_LOGIN_RATE_LIMIT", "10")); err == nil {
		rt.AuthLoginRateLimit = n
	}
	rt.AuthLoginRateWindow = time.Minute
	if sec, err := strconv.Atoi(getenv("AUTH_LOGIN_RATE_WINDOW_SEC", "60")); err == nil && sec > 0 {
		rt.AuthLoginRateWindow = time.Duration(sec) * time.Second
	}
	rt.MetricsEnabled = true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("METRICS_ENABLED"))); v == "false" || v == "0" {
		rt.MetricsEnabled = false
	}
	rt.MetricsBearerToken = strings.TrimSpace(os.Getenv("METRICS_BEARER_TOKEN"))
	// Absent AGENT_RATE_LIMIT leaves the cap at zero, which NewIPLimiter reads
	// as "allow everything" — see the field comment for why that is the default.
	if n, err := strconv.Atoi(getenv("AGENT_RATE_LIMIT", "0")); err == nil && n > 0 {
		rt.AgentRateLimit = n
	}
	rt.AgentRateWindow = time.Minute
	if sec, err := strconv.Atoi(getenv("AGENT_RATE_WINDOW_SEC", "60")); err == nil && sec > 0 {
		rt.AgentRateWindow = time.Duration(sec) * time.Second
	}
	rt.GatewaySecret = strings.TrimSpace(os.Getenv("GATEWAY_SECRET"))
	rt.MediaEndpoint = strings.TrimSpace(os.Getenv("MEDIA_S3_ENDPOINT"))
	rt.MediaRegion = getenv("MEDIA_S3_REGION", "us-east-1")
	rt.MediaBucket = strings.TrimSpace(os.Getenv("MEDIA_S3_BUCKET"))
	rt.MediaAccessKey = strings.TrimSpace(os.Getenv("MEDIA_S3_ACCESS_KEY"))
	rt.MediaSecretKey = strings.TrimSpace(os.Getenv("MEDIA_S3_SECRET_KEY"))
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("MEDIA_S3_USE_SSL"))); v == "true" || v == "1" {
		rt.MediaUseSSL = true
	}
	return rt
}

// IsProduction reports whether the process declared itself production (APP_ENV).
func (rt Runtime) IsProduction() bool {
	return strings.ToLower(strings.TrimSpace(rt.AppEnv)) == "production"
}

// ValidateRuntime enforces startup invariants. In production (APP_ENV) the
// dev-only escape hatches hard-fail instead of warning (TKT-R2): a process
// that would trust forgeable identity headers refuses to start.
// envHint is appended to the production-guard errors. Since APP_ENV defaults to
// production, the operator who trips these may never have set it — saying only
// "with APP_ENV=production" would read as a message about someone else's config.
const envHint = " (APP_ENV defaults to production; declare APP_ENV=development for local work)"

func ValidateRuntime(rt Runtime) error {
	if len(rt.JWTSecret) < 32 {
		return errors.New("config: JWT_SECRET_HEX must be at least 64 hex chars (32 bytes)")
	}
	if rt.IsProduction() {
		if rt.AuthDevHeaders {
			return errors.New("config: AUTH_DEV_HEADERS=true is forbidden with APP_ENV=production — X-User-*/X-Tenant-* headers grant forgeable identities; set AUTH_DEV_HEADERS=false" + envHint)
		}
		if mode := strings.ToLower(strings.TrimSpace(rt.AuthzMode)); mode == "allow" || mode == "" {
			return errors.New("config: AUTHZ_MODE=allow is forbidden with APP_ENV=production — every authorization check would pass; use rbac or opa" + envHint)
		}
		if isKnownDevSecret(rt.JWTSecret) {
			return errors.New("config: JWT_SECRET_HEX is the publicly-known dev throwaway from .env.example — generate a real secret: openssl rand -hex 32")
		}
		// DELIVERY_JWT_SECRET_HEX is NOT required: unset simply means the public
		// delivery feature is off (the signer refuses to issue or honour delivery
		// credentials without its own key — see jwt.Signer). Requiring it would
		// break existing deployments to protect a feature they do not run. But if
		// it IS set, it must actually provide separation.
		if len(rt.DeliveryJWTSecret) > 0 && bytes.Equal(rt.DeliveryJWTSecret, rt.JWTSecret) {
			return errors.New("config: DELIVERY_JWT_SECRET_HEX must differ from JWT_SECRET_HEX — an identical key defeats the separation entirely")
		}
		if len(rt.DeliveryJWTSecret) > 0 && isKnownDevSecret(rt.DeliveryJWTSecret) {
			return errors.New("config: DELIVERY_JWT_SECRET_HEX is the publicly-known dev throwaway — generate a real secret: openssl rand -hex 32")
		}
	}
	return nil
}

// Publicly-known dev throwaways from .env.example — anyone can sign tokens or
// decrypt PII with them, so production refuses to start on a match (TKT-R3
// residual). Length checks alone would accept them.
var knownDevSecretsHex = []string{
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
}

// IsKnownDevSecret reports whether b equals one of the well-known dev
// throwaway secrets shipped in .env.example.
func IsKnownDevSecret(b []byte) bool { return isKnownDevSecret(b) }

func isKnownDevSecret(b []byte) bool {
	got := hex.EncodeToString(b)
	for _, known := range knownDevSecretsHex {
		if got == known {
			return true
		}
	}
	return false
}
