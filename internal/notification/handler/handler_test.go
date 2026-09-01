package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/williamlabdev/saas-forge/internal/notification/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

type fakeNotifSvc struct {
	items []service.NotificationDTO
	dto   service.NotificationDTO
	err   error
}

func (f *fakeNotifSvc) ListMine(context.Context, int) ([]service.NotificationDTO, error) {
	return f.items, f.err
}
func (f *fakeNotifSvc) Create(context.Context, service.CreateInput) (service.NotificationDTO, error) {
	return f.dto, f.err
}

func srv(svc service.NotificationService) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Routes(r)
	return r
}

func do(t *testing.T, svc service.NotificationService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv(svc).ServeHTTP(rec, req)
	return rec
}

func TestList_OK(t *testing.T) {
	rec := do(t, &fakeNotifSvc{items: []service.NotificationDTO{{}}}, http.MethodGet, "/api/v1/notifications/?limit=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestList_Error(t *testing.T) {
	rec := do(t, &fakeNotifSvc{err: apperrors.ErrUnauthorized}, http.MethodGet, "/api/v1/notifications/", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_OK(t *testing.T) {
	rec := do(t, &fakeNotifSvc{}, http.MethodPost, "/api/v1/notifications/", `{"title":"Hi","body":"There"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCreate_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeNotifSvc{}, http.MethodPost, "/api/v1/notifications/", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_ValidationFails(t *testing.T) {
	rec := do(t, &fakeNotifSvc{}, http.MethodPost, "/api/v1/notifications/", `{"title":"","body":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_ServiceError(t *testing.T) {
	rec := do(t, &fakeNotifSvc{err: apperrors.ErrUnauthorized}, http.MethodPost, "/api/v1/notifications/", `{"title":"Hi","body":"There"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}
