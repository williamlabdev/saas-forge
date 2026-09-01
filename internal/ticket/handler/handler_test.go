package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/ticket/service"
)

type fakeTicketSvc struct {
	listRes *service.ListResult
	dto     service.TicketDTO
	err     error
}

func (f *fakeTicketSvc) List(context.Context, service.ListInput) (*service.ListResult, error) {
	return f.listRes, f.err
}
func (f *fakeTicketSvc) GetByID(context.Context, uuid.UUID) (service.TicketDTO, error) {
	return f.dto, f.err
}
func (f *fakeTicketSvc) Create(context.Context, service.CreateInput) (service.TicketDTO, error) {
	return f.dto, f.err
}
func (f *fakeTicketSvc) Update(context.Context, service.UpdateInput) (service.TicketDTO, error) {
	return f.dto, f.err
}
func (f *fakeTicketSvc) Delete(context.Context, uuid.UUID) error { return f.err }

func srv(svc service.TicketService) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Routes(r)
	return r
}

func do(t *testing.T, svc service.TicketService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv(svc).ServeHTTP(rec, req)
	return rec
}

func TestList_OK(t *testing.T) {
	svc := &fakeTicketSvc{listRes: &service.ListResult{}}
	rec := do(t, svc, http.MethodGet, "/api/v1/tickets/?limit=5&offset=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestList_Error(t *testing.T) {
	rec := do(t, &fakeTicketSvc{err: apperrors.ErrForbidden}, http.MethodGet, "/api/v1/tickets/", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGet_OK(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeTicketSvc{dto: service.TicketDTO{}}, http.MethodGet, "/api/v1/tickets/"+id.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGet_InvalidID(t *testing.T) {
	rec := do(t, &fakeTicketSvc{}, http.MethodGet, "/api/v1/tickets/bad", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGet_NotFound(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeTicketSvc{err: apperrors.ErrNotFound}, http.MethodGet, "/api/v1/tickets/"+id.String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_OK(t *testing.T) {
	rec := do(t, &fakeTicketSvc{dto: service.TicketDTO{}}, http.MethodPost, "/api/v1/tickets/", `{"name":"Bug"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCreate_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeTicketSvc{}, http.MethodPost, "/api/v1/tickets/", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_ValidationFails(t *testing.T) {
	rec := do(t, &fakeTicketSvc{}, http.MethodPost, "/api/v1/tickets/", `{"name":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdate_OK(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeTicketSvc{dto: service.TicketDTO{}}, http.MethodPut, "/api/v1/tickets/"+id.String(), `{"name":"Bug","status":"open"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestUpdate_InvalidID(t *testing.T) {
	rec := do(t, &fakeTicketSvc{}, http.MethodPut, "/api/v1/tickets/bad", `{"name":"Bug","status":"open"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdate_InvalidJSON(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeTicketSvc{}, http.MethodPut, "/api/v1/tickets/"+id.String(), `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestDelete_OK(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeTicketSvc{}, http.MethodDelete, "/api/v1/tickets/"+id.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestDelete_InvalidID(t *testing.T) {
	rec := do(t, &fakeTicketSvc{}, http.MethodDelete, "/api/v1/tickets/bad", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestDelete_NotFound(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeTicketSvc{err: apperrors.ErrNotFound}, http.MethodDelete, "/api/v1/tickets/"+id.String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}
