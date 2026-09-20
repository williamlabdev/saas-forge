package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

type fakeNotifSvc struct {
	items []service.NotificationDTO
	dto   service.NotificationDTO
	count int
	err   error

	gotLimit  int
	gotUnread bool
	gotMarkID uuid.UUID
}

func (f *fakeNotifSvc) ListMine(_ context.Context, limit int, unread bool) ([]service.NotificationDTO, error) {
	f.gotLimit, f.gotUnread = limit, unread
	return f.items, f.err
}
func (f *fakeNotifSvc) UnreadCount(context.Context) (int, error) {
	return f.count, f.err
}
func (f *fakeNotifSvc) Create(context.Context, service.CreateInput) (service.NotificationDTO, error) {
	return f.dto, f.err
}
func (f *fakeNotifSvc) MarkRead(_ context.Context, id uuid.UUID) (service.NotificationDTO, error) {
	f.gotMarkID = id
	return f.dto, f.err
}
func (f *fakeNotifSvc) MarkAllRead(context.Context) (int, error) {
	return f.count, f.err
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

func TestList_PassesUnreadQueryParam(t *testing.T) {
	svc := &fakeNotifSvc{}
	rec := do(t, svc, http.MethodGet, "/api/v1/notifications/?unread=true&limit=7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if !svc.gotUnread {
		t.Fatalf("expected unread=true to reach the service")
	}
	if svc.gotLimit != 7 {
		t.Fatalf("limit=%d, want 7", svc.gotLimit)
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

func TestUnreadCount_OK(t *testing.T) {
	rec := do(t, &fakeNotifSvc{count: 3}, http.MethodGet, "/api/v1/notifications/unread-count", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"count":3`)) {
		t.Fatalf("body=%s, want count:3", rec.Body)
	}
}

func TestUnreadCount_Error(t *testing.T) {
	rec := do(t, &fakeNotifSvc{err: apperrors.ErrUnauthorized}, http.MethodGet, "/api/v1/notifications/unread-count", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestMarkRead_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeNotifSvc{dto: service.NotificationDTO{ID: id}}
	rec := do(t, svc, http.MethodPatch, "/api/v1/notifications/"+id.String()+"/read", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.gotMarkID != id {
		t.Fatalf("markID=%s, want %s", svc.gotMarkID, id)
	}
}

func TestMarkRead_InvalidID(t *testing.T) {
	rec := do(t, &fakeNotifSvc{}, http.MethodPatch, "/api/v1/notifications/not-a-uuid/read", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404 for an unparseable id", rec.Code)
	}
}

// The mark-read authorization boundary at the HTTP layer (task 2/8): the
// service answers apperrors.ErrNotFound for someone else's notification
// (see notification_service_test.go's boundary test for the service-layer
// half of this), and the handler must surface that as a plain 404 — no
// different status, no body hint that the id was real but not owned by the
// caller.
func TestMarkRead_OtherUsersNotificationIs404(t *testing.T) {
	rec := do(t, &fakeNotifSvc{err: apperrors.ErrNotFound}, http.MethodPatch, "/api/v1/notifications/"+uuid.NewString()+"/read", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestReadAll_OK(t *testing.T) {
	rec := do(t, &fakeNotifSvc{count: 5}, http.MethodPost, "/api/v1/notifications/read-all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"count":5`)) {
		t.Fatalf("body=%s, want count:5", rec.Body)
	}
}

func TestReadAll_Error(t *testing.T) {
	rec := do(t, &fakeNotifSvc{err: apperrors.ErrUnauthorized}, http.MethodPost, "/api/v1/notifications/read-all", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}
