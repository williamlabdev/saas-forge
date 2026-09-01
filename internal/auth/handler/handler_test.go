package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/auth/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
)

type fakeAuthSvc struct {
	tokens *service.TokenResponse
	err    error
}

func (f *fakeAuthSvc) HashPassword(string) (string, error) { return "", nil }
func (f *fakeAuthSvc) Login(context.Context, service.LoginInput) (*service.TokenResponse, error) {
	return f.tokens, f.err
}
func (f *fakeAuthSvc) Refresh(context.Context, string) (*service.TokenResponse, error) {
	return f.tokens, f.err
}
func (f *fakeAuthSvc) SwitchTenant(context.Context, uuid.UUID, string) (*service.TokenResponse, error) {
	return f.tokens, f.err
}
func (f *fakeAuthSvc) Logout(context.Context, string) error { return f.err }

func srv(svc service.AuthService, limiter *ratelimit.IPLimiter) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc, limiter, metrics.NewRegistry()).Routes(r)
	return r
}

func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogin_OK(t *testing.T) {
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{AccessToken: "a"}}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/login", `{"email":"a@b.com","password":"password12"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestLogin_InvalidJSON(t *testing.T) {
	rec := do(t, srv(&fakeAuthSvc{}, nil), http.MethodPost, "/api/v1/auth/login", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLogin_ValidationFails(t *testing.T) {
	// password too short
	rec := do(t, srv(&fakeAuthSvc{}, nil), http.MethodPost, "/api/v1/auth/login", `{"email":"a@b.com","password":"short"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	h := srv(&fakeAuthSvc{err: service.ErrInvalidCredentials}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/login", `{"email":"a@b.com","password":"password12"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLogin_RateLimited(t *testing.T) {
	// max=1: the first request consumes the only slot, the second is denied.
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{}}, ratelimit.NewIPLimiter(1, time.Minute))
	body := `{"email":"a@b.com","password":"password12"}`
	if rec := do(t, h, http.MethodPost, "/api/v1/auth/login", body); rec.Code != http.StatusOK {
		t.Fatalf("first request code=%d", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/auth/login", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRefresh_OK(t *testing.T) {
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{AccessToken: "a"}}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", `{"refresh_token":"tok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRefresh_InvalidJSON(t *testing.T) {
	rec := do(t, srv(&fakeAuthSvc{}, nil), http.MethodPost, "/api/v1/auth/refresh", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRefresh_Revoked(t *testing.T) {
	h := srv(&fakeAuthSvc{err: service.ErrRefreshRevoked}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", `{"refresh_token":"tok"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRefresh_RateLimited(t *testing.T) {
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{}}, ratelimit.NewIPLimiter(1, time.Minute))
	body := `{"refresh_token":"tok"}`
	if rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", body); rec.Code != http.StatusOK {
		t.Fatalf("first request code=%d", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/auth/refresh", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLogout_OK(t *testing.T) {
	h := srv(&fakeAuthSvc{}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/logout", `{"refresh_token":"tok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestLogout_ServiceError(t *testing.T) {
	h := srv(&fakeAuthSvc{err: service.ErrInvalidToken}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/logout", `{"refresh_token":"tok"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSwitchTenant_Unauthenticated(t *testing.T) {
	// No subject in context (no JWT middleware in this harness) → 401 before
	// the service is ever consulted.
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{AccessToken: "a"}}, nil)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/switch-tenant", `{"tenant":"t_x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestSwitchTenant_Authenticated_OK(t *testing.T) {
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{AccessToken: "a", TenantID: "t_x"}}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/switch-tenant", bytes.NewBufferString(`{"tenant":"t_x"}`))
	req = req.WithContext(authn.WithSubject(req.Context(), authn.Subject{UserID: uuid.New()}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestSwitchTenant_RateLimited(t *testing.T) {
	// max=1: the first request consumes the only slot (the limiter gate runs
	// before auth, mirroring login/refresh), the second is denied.
	h := srv(&fakeAuthSvc{tokens: &service.TokenResponse{AccessToken: "a"}}, ratelimit.NewIPLimiter(1, time.Minute))
	body := `{"tenant":"t_x"}`
	if rec := do(t, h, http.MethodPost, "/api/v1/auth/switch-tenant", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first request code=%d (want 401: slot consumed, no subject)", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/v1/auth/switch-tenant", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
