package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

func gwHandler(secret string) http.Handler {
	exempt := func(r *http.Request) bool { return r.URL.Path == "/health" }
	return GatewayGuard(secret, exempt)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestGatewayGuard_Disabled(t *testing.T) {
	h := gwHandler("") // empty secret = no-op
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled guard must pass through, got %d", rec.Code)
	}
}

func TestGatewayGuard_RejectsMissingAndWrong(t *testing.T) {
	h := gwHandler("s3cret")

	// No header → 403.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing secret must 403, got %d", rec.Code)
	}

	// Wrong secret → 403.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req.Header.Set(GatewayHeader, "wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong secret must 403, got %d", rec.Code)
	}
}

func TestGatewayGuard_AcceptsCorrect(t *testing.T) {
	h := gwHandler("s3cret")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req.Header.Set(GatewayHeader, "s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct secret must pass, got %d", rec.Code)
	}
}

func TestGatewayGuard_HealthExemptEvenWhenEnabled(t *testing.T) {
	h := gwHandler("s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health must be exempt, got %d", rec.Code)
	}
}

// chain composes the two middlewares in the order internal/platform/router.go
// mounts them (`:51` guard, `:54` auth) and reports what the inner handler saw.
// It exists because the interesting property is not either middleware's own
// behaviour — both are covered above — but what their COMBINATION does to the
// one caller that cannot hold the gateway secret: the browser console
// (ADR-015, ruled 2026-08-20).
func chain(secret string, signer *jwt.Signer, allowDevHeaders bool) (http.Handler, *bool, *Subject) {
	reached := false
	var seen Subject
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen, _ = SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	exempt := func(r *http.Request) bool { return r.URL.Path == "/health" }
	h := GatewayGuard(secret, exempt)(JWTMiddleware(signer, allowDevHeaders, nil)(inner))
	return h, &reached, &seen
}

func gwSigner(t *testing.T) (*jwt.Signer, uuid.UUID, string) {
	t.Helper()
	signer := jwt.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	id := uuid.New()
	token, _, err := signer.IssueAccessToken(id, []string{"member"}, "tenant-a", "admin", "", false)
	require.NoError(t, err)
	return signer, id, token
}

// ADR-015 §驗證 3: a perfect access token is NECESSARY AND NOT SUFFICIENT.
//
// This is the failure the console hits if only half of the production auth path
// lands — the token is added, VITE_CONTENT_API still addresses :8080 directly —
// and the reason it is worth a test of its own is that it looks like an auth
// bug and is not one: the request never reaches authentication. Nothing about
// the token is even read.
func TestGatewayGuard_ValidBearerStillRefusedWithoutTheGatewayHeader(t *testing.T) {
	signer, _, token := gwSigner(t)
	h, reached, _ := chain("s3cret", signer, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/types", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "a browser cannot hold the gateway secret")
	assert.Contains(t, rec.Body.String(), "GATEWAY_REQUIRED",
		"the refusal must name the gateway, not read as an authentication failure")
	assert.False(t, *reached, "the guard runs before anything downstream")
}

// The other half: once the request arrives THROUGH the gateway, the console's
// own bearer is what identifies it. This is the shape production must have —
// gateway injects X-Gateway-Secret, browser supplies Authorization — and it is
// the only shape in which the CMS console works at all outside dev.
func TestGatewayGuard_GatewayHeaderPlusBearerAuthenticates(t *testing.T) {
	signer, id, token := gwSigner(t)
	h, reached, seen := chain("s3cret", signer, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/types", nil)
	req.Header.Set(GatewayHeader, "s3cret")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *reached)
	assert.Equal(t, id, seen.UserID)
	assert.Equal(t, "tenant-a", seen.TenantID)
}

// 🔴 Why neither test above can be replaced by "run it locally and see".
//
// The dev defaults are the two that make the broken shape work: GATEWAY_SECRET
// is empty (`runtime.go:133` reads it, nothing sets it) so the guard is a
// no-op, and AUTH_DEV_HEADERS is true (`runtime.go:113`) so the X-User-*
// headers authenticate on their own. A console that sends no token and
// addresses :8080 directly is therefore INDISTINGUISHABLE from a correct one
// on a developer machine — it is not that the failure is unlikely there, it is
// that the failure cannot occur there. This test pins that, so the emptiness of
// the local signal is a stated property rather than a discovery someone makes
// once in production.
func TestGatewayGuard_DevDefaultsHideBothHalvesOfTheProductionPath(t *testing.T) {
	signer, _, _ := gwSigner(t)
	h, reached, seen := chain("", signer, true) // the local machine

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/types", nil)
	req.Header.Set("X-User-Id", uuid.NewString())
	req.Header.Set("X-User-Roles", "admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "no gateway header, no token, and it works")
	require.True(t, *reached)
	assert.NotEqual(t, uuid.Nil, seen.UserID, "and it is authenticated, by the headers alone")
}
