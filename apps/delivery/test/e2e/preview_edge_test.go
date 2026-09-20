// Package e2e_test joins the two halves of a preview link over real HTTP: the
// Domain API that mints and validates the credential, and the delivery edge
// that forwards it.
//
// WHY IT LIVES HERE AND NOT IN test/e2e. The edge's handler and upstream client
// are under apps/delivery/internal, which Go makes importable only from within
// apps/delivery — so the repository's main e2e suite structurally cannot run
// the edge in-process. That suite proves the Domain API half against the same
// real database (test/e2e/preview_test.go); this one proves the join, and the
// response contract that only the edge decides: no-store, no validator, and a
// single-entry scope that the edge refuses to widen.
//
// The pair replaces a probe script that proved all of it against the running
// compose stack and then vanished with the session that wrote it.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	deliveryhandler "github.com/williamlabdev/saas-forge/apps/delivery/internal/handler"
	"github.com/williamlabdev/saas-forge/apps/delivery/internal/upstream"
	"github.com/williamlabdev/saas-forge/internal"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/platform"
	"github.com/williamlabdev/saas-forge/internal/platform/migrate"
)

var (
	domainURL string
	edgeURL   string
	e2eOn     bool
)

// deliveryKey is the edge's key. It is NOT the Domain API's main key — the
// separation is the premise ADR-006 rests on, so the harness reproduces it
// rather than sharing one signer between the two processes.
var (
	mainKey     = bytes.Repeat([]byte{3}, 32)
	deliveryKey = bytes.Repeat([]byte{4}, 32)
)

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	if os.Getenv("SKIP_E2E") == "1" || !dockerAvailable() {
		if !dockerAvailable() {
			fmt.Fprintln(os.Stderr, "delivery e2e skipped: Docker not available")
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
		fmt.Fprintf(os.Stderr, "delivery e2e skipped (postgres): %v\n", err)
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
	// Same applier as production and as the platform e2e suite (ADR-012), so
	// "how a schema gets built" has one implementation rather than three. It
	// also writes the ledger, which BuildApp's boot check requires below.
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

	app, err := platform.BuildApp(ctx,
		config.User{
			DatabaseURL:      connStr,
			HTTPAddr:         ":0",
			EncryptionKey:    bytes.Repeat([]byte{1}, 32),
			BlindIndexPepper: bytes.Repeat([]byte{2}, 32),
		},
		config.Runtime{
			AuthzMode:         "rbac",
			JWTSecret:         mainKey,
			DeliveryJWTSecret: deliveryKey,
			JWTAccessTTL:      15 * time.Minute,
			JWTRefreshTTL:     24 * time.Hour,
			// Every request in this suite arrives from the test process, so a
			// production-sized per-IP login cap would trip on the second test.
			AuthLoginRateLimit:  100,
			AuthLoginRateWindow: time.Minute,
		})
	if err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	domain := httptest.NewServer(app.Handler)
	// The edge holds ONLY the delivery key — jwt.NewSigner(nil, …) is what makes
	// "this process cannot mint a login" a fact about the wiring rather than a
	// convention. Its own reads mint tenant-scoped delivery credentials; the
	// preview path mints nothing and forwards the caller's token.
	edgeSigner := jwt.NewSigner(nil, time.Minute).WithDeliveryKey(deliveryKey)
	h := deliveryhandler.New(upstream.NewClient(domain.URL, "", edgeSigner), nil, 0)
	r := chi.NewRouter()
	h.Routes(r)
	edge := httptest.NewServer(r)

	domainURL, edgeURL, e2eOn = domain.URL, edge.URL, true
	code := m.Run()

	edge.Close()
	domain.Close()
	app.Pool.Close()
	pool.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func requireE2E(t *testing.T) {
	t.Helper()
	if !e2eOn {
		t.Skip("e2e requires Docker; set SKIP_E2E=1 to silence")
	}
}

// A preview link, opened at the edge exactly as a reviewer would open it.
func TestE2E_EdgeServesTheWorkingCopyForAPreviewLink(t *testing.T) {
	requireE2E(t)

	token, tenant := registerAndLogin(t, "edgepvw")
	domainPost(t, "/api/v1/content/types", token,
		`{"name":"story","label":"S","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		http.StatusCreated)

	draft := domainPost(t, "/api/v1/content/entries?type=story", token,
		`{"title":"unreleased"}`, http.StatusCreated)
	draftID := draft["id"].(string)
	// Published on purpose: a second DRAFT would 404 on the published-only gate
	// even if the token's single-entry narrowing were removed, so the scope
	// assertion below would pass for a reason that has nothing to do with scope.
	other := domainPost(t, "/api/v1/content/entries?type=story", token,
		`{"title":"already public, still not this token's"}`, http.StatusCreated)
	otherID := other["id"].(string)
	domainPost(t, "/api/v1/content/entries/"+otherID+"/publish?type=story", token, "", http.StatusOK)

	link := domainPost(t, "/api/v1/content/entries/"+draftID+"/preview-link?type=story",
		token, "", http.StatusCreated)
	previewToken := link["token"].(string)

	// The URL a reviewer is sent: the published shape, plus the credential.
	resp, body := edgeGet(t, fmt.Sprintf("/v1/%s/story/%s?preview_token=%s", tenant, draftID, previewToken))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	// This is the assertion no unit test on either side can make: a token the
	// Domain API MINTED, forwarded verbatim by the real edge, is a token the
	// Domain API ACCEPTS — and what comes back is the unpublished working copy.
	var entry struct {
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
		CreatedBy *string `json:"created_by"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &entry), body)
	require.Equal(t, "unreleased", entry.Data.Title)
	require.Nil(t, entry.CreatedBy, "the preview audience is shown no actors")

	// The edge's own half of the contract. no-store because this body is one
	// tenant's unpublished draft delivered against a bearer token that sits in
	// the URL; no validator because nothing may store it in the first place.
	require.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"))
	require.Empty(t, resp.Header.Get("ETag"), "a preview response must not invite the caching it forbids")

	// The same link, aimed at the tenant's other — published — entry. The edge
	// does not decode the token; upstream is what narrows it, and 404 is what
	// arrives even for a row the public may read.
	resp, body = edgeGet(t, fmt.Sprintf("/v1/%s/story/%s?preview_token=%s", tenant, otherID, previewToken))
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	// Pasted into a collection URL. Refused BY THE EDGE with an actionable code:
	// upstream's 403 would otherwise reach the reviewer as a generic 502, and
	// silently serving the published list would hide the draft they were sent.
	resp, body = edgeGet(t, fmt.Sprintf("/v1/%s/story?preview_token=%s", tenant, previewToken))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, body, "PREVIEW_NOT_SUPPORTED")
}

// A forged or expired token must not be distinguishable at the edge — it holds
// no key that can validate one, and upstream's refusal is what the caller gets.
func TestE2E_EdgeCannotHonourATokenItWasNotGiven(t *testing.T) {
	requireE2E(t)

	token, tenant := registerAndLogin(t, "edgeforge")
	domainPost(t, "/api/v1/content/types", token,
		`{"name":"note","label":"N","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		http.StatusCreated)
	entry := domainPost(t, "/api/v1/content/entries?type=note", token,
		`{"title":"secret"}`, http.StatusCreated)
	id := entry["id"].(string)

	// Signed with the wrong key — the shape is right, the signature is not.
	forged, _, err := jwt.NewSigner(nil, time.Minute).
		WithDeliveryKey(bytes.Repeat([]byte{9}, 32)).
		IssuePreviewToken(uuid.New(), tenant, uuid.MustParse(id))
	require.NoError(t, err)

	resp, body := edgeGet(t, fmt.Sprintf("/v1/%s/note/%s?preview_token=%s", tenant, id, forged))
	require.NotEqual(t, http.StatusOK, resp.StatusCode, body)
	require.NotContains(t, body, "secret", "an unverifiable credential must return no content at all")
}

// --- harness -----------------------------------------------------------------

func registerAndLogin(t *testing.T, prefix string) (accessToken, tenantSlug string) {
	t.Helper()
	email := fmt.Sprintf("%s-%s@example.com", prefix, uuid.NewString()[:8])
	domainPost(t, "/api/v1/users", "",
		fmt.Sprintf(`{"username":%q,"email":%q,"password":"password12"}`, prefix, email),
		http.StatusCreated)
	login := domainPost(t, "/api/v1/auth/login", "",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, email), http.StatusOK)
	return login["access_token"].(string), login["tenant_id"].(string)
}

// domainPost calls the Domain API over real HTTP and returns its data object.
func domainPost(t *testing.T, path, bearer, body string, want int) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, domainURL+path, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, resp.StatusCode, "%s: %s", path, raw)

	var env struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	return env.Data
}

func edgeGet(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(edgeURL + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(raw)
}
