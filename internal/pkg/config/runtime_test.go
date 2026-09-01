package config

import (
	"bytes"
	"encoding/hex"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearRuntimeEnv forces every knob to its default by setting empties.
func clearRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AUTHZ_MODE", "MCP_BASE_URL", "OUTBOX_POLL_SECONDS", "OUTBOX_BATCH_SIZE",
		"OUTBOX_MAX_RETRIES", "JWT_ACCESS_TTL_MINUTES", "JWT_REFRESH_TTL_DAYS",
		"JWT_SECRET_HEX", "BOOTSTRAP_ADMIN_USER_ID", "AUTH_DEV_HEADERS",
		"AUTH_LOGIN_RATE_LIMIT", "AUTH_LOGIN_RATE_WINDOW_SEC", "METRICS_ENABLED",
		"METRICS_BEARER_TOKEN", "TRUST_PROXY_HEADERS", "APP_ENV",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadRuntimeFromEnv_Defaults(t *testing.T) {
	clearRuntimeEnv(t)
	rt := LoadRuntimeFromEnv()

	assert.Equal(t, "production", rt.AppEnv,
		"APP_ENV defaults to the safe end: the TKT-R2 guards are armed unless someone opts out")
	assert.True(t, rt.IsProduction())
	assert.Equal(t, "allow", rt.AuthzMode)
	assert.Equal(t, 2*time.Second, rt.OutboxPollInterval)
	assert.Equal(t, 10, rt.OutboxBatchSize)
	assert.Equal(t, 5, rt.OutboxMaxRetries)
	assert.Equal(t, 60*time.Second, rt.OutboxStaleThreshold)
	assert.Equal(t, 15*time.Minute, rt.JWTAccessTTL)
	assert.Equal(t, 7*24*time.Hour, rt.JWTRefreshTTL)
	assert.True(t, rt.AuthDevHeaders,
		"the flag itself still defaults on for the dev template — what stops it reaching production is ValidateRuntime, which now runs by default (see TestValidateRuntime_FailsClosedWithoutAppEnv)")
	assert.Equal(t, 10, rt.AuthLoginRateLimit)
	assert.Equal(t, time.Minute, rt.AuthLoginRateWindow)
	assert.True(t, rt.MetricsEnabled)
}

// TestValidateRuntime_FailsClosedWithoutAppEnv pins the whole point of defaulting
// APP_ENV to "production": an operator who never declares an environment gets the
// guards, not a warning. Before 2026-09-01 this configuration booted and only
// logged WARN, which meant the protection reached exactly the operators who did
// not need it.
func TestValidateRuntime_FailsClosedWithoutAppEnv(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv("JWT_SECRET_HEX", strings.Repeat("ab", 32))

	rt := LoadRuntimeFromEnv()
	err := ValidateRuntime(rt)

	require.Error(t, err, "unset APP_ENV must not silently mean development")
	assert.Contains(t, err.Error(), "AUTH_DEV_HEADERS=true is forbidden")
}

// The escape hatch has to actually work, or every local flow breaks.
func TestValidateRuntime_DevelopmentDisarmsGuards(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("JWT_SECRET_HEX", strings.Repeat("ab", 32))

	require.NoError(t, ValidateRuntime(LoadRuntimeFromEnv()),
		"APP_ENV=development is what .env.example sets; it must keep allow-mode + dev headers bootable")
}

func TestLoadRuntimeFromEnv_Overrides(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv("AUTHZ_MODE", "opa")
	t.Setenv("OUTBOX_BATCH_SIZE", "25")
	t.Setenv("OUTBOX_MAX_RETRIES", "9")
	t.Setenv("JWT_ACCESS_TTL_MINUTES", "30")
	t.Setenv("AUTH_DEV_HEADERS", "false")
	t.Setenv("METRICS_ENABLED", "0")
	t.Setenv("JWT_SECRET_HEX", strings.Repeat("ab", 32)) // 32 bytes

	rt := LoadRuntimeFromEnv()
	assert.Equal(t, "opa", rt.AuthzMode)
	assert.Equal(t, 25, rt.OutboxBatchSize)
	assert.Equal(t, 9, rt.OutboxMaxRetries)
	assert.Equal(t, 30*time.Minute, rt.JWTAccessTTL)
	assert.False(t, rt.AuthDevHeaders)
	assert.False(t, rt.MetricsEnabled)
	assert.Len(t, rt.JWTSecret, 32)
}

func TestLoadRuntimeFromEnv_IgnoresShortJWTSecret(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv("JWT_SECRET_HEX", "abcd") // too short -> ignored
	rt := LoadRuntimeFromEnv()
	assert.Empty(t, rt.JWTSecret)
}

func TestValidateRuntime(t *testing.T) {
	err := ValidateRuntime(Runtime{})
	require.Error(t, err, "missing JWT secret must fail validation")

	err = ValidateRuntime(Runtime{JWTSecret: make([]byte, 32)})
	require.NoError(t, err)
}

// captureLog runs fn with the standard logger redirected to a buffer and
// returns everything it wrote.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func TestLogProductionWarnings_AllowModeWarns(t *testing.T) {
	out := captureLog(func() {
		LogProductionWarnings(Runtime{AuthzMode: "allow", MCPBaseURL: "http://mcp"})
	})
	require.Contains(t, out, "WARN: AUTHZ_MODE")
	require.Contains(t, out, "all authorization checks pass")
}

func TestLogProductionWarnings_EmptyModeWarns(t *testing.T) {
	// Empty AUTHZ_MODE falls back to allow-all and must warn too.
	out := captureLog(func() {
		LogProductionWarnings(Runtime{AuthzMode: "", MCPBaseURL: "http://mcp"})
	})
	require.Contains(t, out, "WARN: AUTHZ_MODE")
}

func TestLogProductionWarnings_OpaModeSilent(t *testing.T) {
	out := captureLog(func() {
		LogProductionWarnings(Runtime{AuthzMode: "opa", MCPBaseURL: "http://mcp"})
	})
	require.NotContains(t, out, "AUTHZ_MODE")
}

func TestLogProductionWarnings_DevHeadersWarn(t *testing.T) {
	out := captureLog(func() {
		LogProductionWarnings(Runtime{AuthzMode: "opa", AuthDevHeaders: true, MCPBaseURL: "http://mcp"})
	})
	require.Contains(t, out, "AUTH_DEV_HEADERS")
}

func TestLogProductionWarnings_EmptyMCPBaseURLWarns(t *testing.T) {
	out := captureLog(func() {
		LogProductionWarnings(Runtime{AuthzMode: "opa"})
	})
	require.Contains(t, out, "MCP_BASE_URL")
}

func TestLogProductionWarnings_TrustProxyWarns(t *testing.T) {
	out := captureLog(func() {
		LogProductionWarnings(Runtime{AuthzMode: "opa", MCPBaseURL: "http://mcp", TrustProxyHeaders: true})
	})
	require.Contains(t, out, "TRUST_PROXY_HEADERS")
}

func TestLoadRuntime_TrustProxyHeaders(t *testing.T) {
	clearRuntimeEnv(t)
	require.False(t, LoadRuntimeFromEnv().TrustProxyHeaders, "must default to false")
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	require.True(t, LoadRuntimeFromEnv().TrustProxyHeaders)
}

func TestValidateRuntime_ProductionGuards(t *testing.T) {
	secret := make([]byte, 32)

	// TKT-R2: dev headers refuse to start in production.
	err := ValidateRuntime(Runtime{
		AppEnv: "production", AuthzMode: "opa", JWTSecret: secret, AuthDevHeaders: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTH_DEV_HEADERS")

	// Allow-mode refuses to start in production.
	err = ValidateRuntime(Runtime{
		AppEnv: "production", AuthzMode: "allow", JWTSecret: secret,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTHZ_MODE")

	// Sane production config passes.
	require.NoError(t, ValidateRuntime(Runtime{
		AppEnv: "production", AuthzMode: "rbac", JWTSecret: secret,
	}))
	require.NoError(t, ValidateRuntime(Runtime{
		AppEnv: "Production ", AuthzMode: "opa", JWTSecret: secret,
	}), "APP_ENV matching is case/space-insensitive")

	// Development keeps the dev ergonomics (warn-only).
	require.NoError(t, ValidateRuntime(Runtime{
		AppEnv: "development", AuthzMode: "allow", JWTSecret: secret, AuthDevHeaders: true,
	}))
	require.NoError(t, ValidateRuntime(Runtime{
		AuthzMode: "allow", JWTSecret: secret, AuthDevHeaders: true,
	}), "empty AppEnv behaves as development")
}

func TestValidateRuntime_RejectsKnownDevJWTSecretInProd(t *testing.T) {
	known, _ := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	err := ValidateRuntime(Runtime{AppEnv: "production", AuthzMode: "opa", JWTSecret: known})
	require.Error(t, err)
	require.Contains(t, err.Error(), "JWT_SECRET_HEX")

	// The same throwaway is fine in development.
	require.NoError(t, ValidateRuntime(Runtime{AuthzMode: "opa", JWTSecret: known}))

	// A real secret passes in production.
	real := make([]byte, 32)
	real[0] = 0x99
	require.NoError(t, ValidateRuntime(Runtime{AppEnv: "production", AuthzMode: "opa", JWTSecret: real}))
}

func TestValidateUserSecrets_RejectsKnownDevValuesInProd(t *testing.T) {
	encKnown, _ := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	pepKnown, _ := hex.DecodeString("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	real := make([]byte, 32)
	real[0] = 0x99

	err := ValidateUserSecrets(User{EncryptionKey: encKnown, BlindIndexPepper: real}, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ENCRYPTION_KEY_HEX")

	err = ValidateUserSecrets(User{EncryptionKey: real, BlindIndexPepper: pepKnown}, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "BLIND_INDEX_PEPPER_HEX")

	// Development: throwaways allowed.
	require.NoError(t, ValidateUserSecrets(User{EncryptionKey: encKnown, BlindIndexPepper: pepKnown}, false))
	// Production with real secrets.
	real2 := make([]byte, 32)
	real2[1] = 0x88
	require.NoError(t, ValidateUserSecrets(User{EncryptionKey: real, BlindIndexPepper: real2}, true))
}
