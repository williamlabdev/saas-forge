package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/williamlabdev/saas-forge/internal"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/mcp"
	mcpmock "github.com/williamlabdev/saas-forge/internal/pkg/mcp/mock"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/pkg/requestctx"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/platform"
	"github.com/williamlabdev/saas-forge/internal/platform/migrate"
)

var (
	e2eHandler http.Handler
	e2ePool    *pgxpool.Pool
	e2eMCP     *mcpmock.Server
	e2eCleanup func()
	e2eOn      bool
	e2eApp     *platform.App
	e2eSigner  *jwt.Signer
	// e2eOutboxReg is the worker's metrics registry, exposed so a test can
	// assert on delivery outcomes (see outbox_delivery_test.go).
	e2eOutboxReg *metrics.Registry
)

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func TestMain(m *testing.M) {
	// The suite emulates a trusted-proxy deployment: each test carries its own
	// synthetic client IP via X-Forwarded-For (e2eClientIP) so the per-IP login
	// rate limiter is isolated per test. Production defaults to NOT trusting
	// the header (TRUST_PROXY_HEADERS=false; see internal/pkg/requestctx).
	requestctx.SetTrustProxyHeaders(true)

	ctx := context.Background()
	if os.Getenv("SKIP_E2E") == "1" || !dockerAvailable() {
		if !dockerAvailable() {
			fmt.Fprintln(os.Stderr, "e2e tests skipped: Docker not available")
		}
		os.Exit(m.Run())
	}

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("e2e"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e skipped (postgres): %v\n", err)
		os.Exit(m.Run())
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}
	// Build the schema with the SAME applier production uses (ADR-012). This
	// used to concatenate every .up.sql and run it as one statement, which meant
	// "how e2e builds a schema" and "how production applies one" were two
	// separate implementations that had never checked each other — and only the
	// e2e one was ever exercised here. It also leaves the ledger populated,
	// which BuildApp's boot check below now requires.
	ms, err := migrate.Discover(internal.MigrationsFS)
	if err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}
	if _, err := migrate.Up(ctx, pool, ms); err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	cfg := config.User{
		DatabaseURL:      connStr,
		HTTPAddr:         ":8080",
		EncryptionKey:    bytesRepeat(32, 1),
		BlindIndexPepper: bytesRepeat(32, 2),
	}
	e2eMCP = mcpmock.NewServer()
	mcpListener := httptest.NewServer(e2eMCP.Handler())

	rt := config.Runtime{
		AuthzMode:      "rbac",
		AuthDevHeaders: false,
		JWTSecret:      bytesRepeat(32, 3),
		// Separate key so the delivery credential path is exercised end to end
		// (ADR-004): unset would leave the feature off and delivery tokens refused.
		DeliveryJWTSecret:   bytesRepeat(32, 4),
		JWTAccessTTL:        15 * time.Minute,
		JWTRefreshTTL:       24 * time.Hour,
		OutboxBatchSize:     5,
		OutboxMaxRetries:    3,
		OutboxPollInterval:  200 * time.Millisecond,
		MCPBaseURL:          mcpListener.URL,
		AuthLoginRateLimit:  3,
		AuthLoginRateWindow: time.Minute,
		MetricsEnabled:      true,
		// R4b: content limits now come from the tenant's plan (plans table),
		// not env. The quota e2e assigns its tenant a low-limit plan to trip
		// the 429 (see TestE2E_ContentQuotaBackstop).
	}

	app, err := platform.BuildApp(ctx, cfg, rt)
	if err != nil {
		mcpListener.Close()
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	// Build the worker through the SAME provider production uses. Hand-assembling
	// a second copy here is what let e2e drift: when production gained
	// WithContentWebhooks, this copy kept nil webhooks, so every content event
	// failed its delivery and retried to dead-letter — invisibly, because a
	// passing test prints no log. rt.MCPBaseURL already points at the mock MCP
	// server, so the provider resolves the same client the hand-built one used.
	reg := metrics.NewRegistry()
	e2eOutboxReg = reg
	worker := platform.ProvideOutboxWorker(pool, rt, reg)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	go worker.Run(workerCtx, rt.OutboxPollInterval)

	e2eHandler = app.Handler
	e2eApp = app
	e2eSigner = jwt.NewSigner(rt.JWTSecret, rt.JWTAccessTTL).WithDeliveryKey(rt.DeliveryJWTSecret)
	e2ePool = pool
	e2eOn = true
	e2eCleanup = func() {
		cancelWorker()
		mcpListener.Close()
		app.Pool.Close()
		pool.Close()
		_ = container.Terminate(ctx)
	}
	code := m.Run()
	e2eCleanup()
	os.Exit(code)
}

func requireE2E(t *testing.T) {
	t.Helper()
	if !e2eOn {
		t.Skip("e2e requires Docker; set SKIP_E2E=1 to silence")
	}
}

func TestE2E_OutboxDeliveredToMCP(t *testing.T) {
	requireE2E(t)
	e2eMCP.Reset()

	email := fmt.Sprintf("mcp-%s@example.com", uuid.NewString()[:8])
	rec := doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"mcpuser","email":%q,"password":"password12"}`, email), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code)
	userID := decodeDataMap(t, rec)["id"].(string)

	// Reset above does not isolate this test: the worker keeps delivering events
	// enqueued by earlier tests, and those upserts land in the shared mock after
	// the reset. Wait for THIS user's upsert rather than for "any upsert", or the
	// assertions below read a neighbour's event.
	var got mcp.UpsertRequest
	require.Eventually(t, func() bool {
		for _, u := range e2eMCP.Upserts() {
			if u.UserID.String() == userID {
				got = u
				return true
			}
		}
		return false
	}, 15*time.Second, 200*time.Millisecond, "MCP mock should receive user.created upsert for this user")

	assert.Equal(t, "user.created", got.EventType)
	assert.Equal(t, "active", got.Status)

	// The outbox row is marked done only after the MCP call returns, so a single
	// read right after the upsert arrives is racy.
	require.Eventually(t, func() bool {
		var status string
		err := e2ePool.QueryRow(context.Background(), `
			SELECT status::text FROM integration_outbox WHERE aggregate_id = $1::uuid ORDER BY created_at DESC LIMIT 1
		`, userID).Scan(&status)
		return err == nil && status == "done"
	}, 15*time.Second, 200*time.Millisecond, "outbox row for this user should reach done")
}

func TestE2E_RegisterLoginAndAccessUser(t *testing.T) {
	requireE2E(t)

	email := fmt.Sprintf("e2e-%s@example.com", uuid.NewString()[:8])
	regBody := fmt.Sprintf(`{"username":"e2euser","email":%q,"password":"password12"}`, email)

	rec := doJSON(t, http.MethodPost, "/api/v1/users", regBody, "", "", e2eClientIP(t))
	assert.Equal(t, http.StatusCreated, rec.Code)
	userID := decodeDataMap(t, rec)["id"].(string)

	loginBody := fmt.Sprintf(`{"email":%q,"password":"password12"}`, email)
	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login", loginBody, "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code)
	token := decodeDataMap(t, recLogin)["access_token"].(string)

	assert.Equal(t, http.StatusOK, doJSON(t, http.MethodGet, "/api/v1/users/"+userID, "", "Bearer "+token, "", e2eClientIP(t)).Code)
	assert.Equal(t, http.StatusUnauthorized, doJSON(t, http.MethodGet, "/api/v1/users/"+userID, "", "", "", e2eClientIP(t)).Code)
}

func TestE2E_RegisterIdempotencyKey(t *testing.T) {
	requireE2E(t)

	key := "idemkey-" + uuid.NewString()
	email := fmt.Sprintf("idem-%s@example.com", uuid.NewString()[:8])
	body := fmt.Sprintf(`{"username":"idemuser","email":%q,"password":"password12"}`, email)

	rec1 := doJSON(t, http.MethodPost, "/api/v1/users", body, "", key, e2eClientIP(t))
	assert.Equal(t, http.StatusCreated, rec1.Code)
	id1 := decodeDataMap(t, rec1)["id"]

	rec2 := doJSON(t, http.MethodPost, "/api/v1/users", body, "", key, e2eClientIP(t))
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, id1, decodeDataMap(t, rec2)["id"])
}

func TestE2E_LoginRateLimit(t *testing.T) {
	requireE2E(t)

	email := fmt.Sprintf("ratelimit-%s@example.com", uuid.NewString()[:8])
	doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"rluser","email":%q,"password":"password12"}`, email), "", "", e2eClientIP(t))

	body := fmt.Sprintf(`{"email":%q,"password":"wrongpass12"}`, email)
	rateIP := "203.0.113.50"
	var lastCode int
	for i := 0; i < 4; i++ {
		rec := doJSON(t, http.MethodPost, "/api/v1/auth/login", body, "", "", rateIP)
		lastCode = rec.Code
	}
	assert.Equal(t, http.StatusTooManyRequests, lastCode)

	recMetrics := httptest.NewRecorder()
	e2eHandler.ServeHTTP(recMetrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, recMetrics.Code)
	assert.Contains(t, recMetrics.Body.String(), "auth_login_rate_limited_total")
}

func TestE2E_LoginAuditEvent(t *testing.T) {
	requireE2E(t)

	email := fmt.Sprintf("audit-%s@example.com", uuid.NewString()[:8])
	ip := e2eClientIP(t)
	rec := doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"audituser","email":%q,"password":"password12"}`, email), "", "", ip)
	require.Equal(t, http.StatusCreated, rec.Code)

	_ = doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"wrongpass12"}`, email), "", "", ip)

	var count int
	err := e2ePool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM auth_audit_events WHERE event_type = 'login' AND outcome = 'failure'
	`).Scan(&count)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 1)
}

func TestE2E_AdminListUsersCursor(t *testing.T) {
	requireE2E(t)

	adminEmail := fmt.Sprintf("listadmin-%s@example.com", uuid.NewString()[:8])
	ip := e2eClientIP(t)
	recA := doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"listadmin","email":%q,"password":"password12"}`, adminEmail), "", "", ip)
	require.Equal(t, http.StatusCreated, recA.Code)
	adminID := decodeDataMap(t, recA)["id"].(string)
	require.NoError(t, assignRoleDirect(context.Background(), adminID, "admin"))

	login := decodeDataMap(t, doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, adminEmail), "", "", ip))
	adminToken := login["access_token"].(string)

	memberEmail := fmt.Sprintf("listmem-%s@example.com", uuid.NewString()[:8])
	require.Equal(t, http.StatusCreated, doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"listmem","email":%q,"password":"password12"}`, memberEmail), "", "", ip).Code)

	recList := doJSON(t, http.MethodGet, "/api/v1/users?limit=10", "", "Bearer "+adminToken, "", ip)
	require.Equal(t, http.StatusOK, recList.Code)
	env := decodeEnvelope(t, recList)
	items, ok := env.Data.([]any)
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(items), 2)
	page, ok := env.Meta.Page.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(10), page["limit"])

	recMember := doJSON(t, http.MethodGet, "/api/v1/users", "", "Bearer "+decodeMemberToken(t, memberEmail, ip), "", ip)
	assert.Equal(t, http.StatusForbidden, recMember.Code)
}

func decodeMemberToken(t *testing.T, email, clientIP string) string {
	t.Helper()
	login := decodeDataMap(t, doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, email), "", "", clientIP))
	return login["access_token"].(string)
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) response.Envelope {
	t.Helper()
	var env response.Envelope
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&env))
	require.Nil(t, env.Error)
	return env
}

func TestE2E_IAMAdminAssignRole(t *testing.T) {
	requireE2E(t)

	adminEmail := fmt.Sprintf("admin-%s@example.com", uuid.NewString()[:8])
	ip := e2eClientIP(t)
	recA := doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"admin1","email":%q,"password":"password12"}`, adminEmail), "", "", ip)
	require.Equal(t, http.StatusCreated, recA.Code)
	adminID := decodeDataMap(t, recA)["id"].(string)

	require.NoError(t, assignRoleDirect(context.Background(), adminID, "admin"))

	login := decodeDataMap(t, doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, adminEmail), "", "", ip))
	adminToken := login["access_token"].(string)

	memberEmail := fmt.Sprintf("member-%s@example.com", uuid.NewString()[:8])
	recM := doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":"mem1","email":%q,"password":"password12"}`, memberEmail), "", "", ip)
	require.Equal(t, http.StatusCreated, recM.Code)
	memberID := decodeDataMap(t, recM)["id"].(string)

	recAssign := doJSON(t, http.MethodPut, "/api/v1/users/"+memberID+"/roles",
		`{"role":"admin"}`, "Bearer "+adminToken, "", ip)
	assert.Equal(t, http.StatusOK, recAssign.Code)

	recRoles := doJSON(t, http.MethodGet, "/api/v1/users/"+memberID+"/roles", "", "Bearer "+adminToken, "", ip)
	require.Equal(t, http.StatusOK, recRoles.Code)
	roles := decodeDataMap(t, recRoles)["roles"].([]any)
	assert.Contains(t, roles, "admin")
}

func assignRoleDirect(ctx context.Context, userID, roleName string) error {
	_, err := e2ePool.Exec(ctx, `
		INSERT INTO user_roles (user_id, role_id)
		SELECT $1::uuid, id FROM roles WHERE name = $2
		ON CONFLICT DO NOTHING
	`, userID, roleName)
	return err
}

func e2eClientIP(t *testing.T) string {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	n := h.Sum32()
	return fmt.Sprintf("10.%d.%d.%d", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

func doJSON(t *testing.T, method, path, body, authHeader, idemKey, clientIP string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	rec := httptest.NewRecorder()
	e2eHandler.ServeHTTP(rec, req)
	return rec
}

func decodeDataMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env response.Envelope
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&env))
	require.Nil(t, env.Error)
	m, ok := env.Data.(map[string]any)
	require.True(t, ok)
	return m
}

func bytesRepeat(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
