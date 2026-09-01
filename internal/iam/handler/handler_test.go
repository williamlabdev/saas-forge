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
)

type fakeIAMSvc struct {
	roles []string
	err   error
}

func (f *fakeIAMSvc) ListRolesForUser(context.Context, uuid.UUID) ([]string, error) {
	return f.roles, f.err
}
func (f *fakeIAMSvc) AssignRoleByName(context.Context, uuid.UUID, string) error { return f.err }
func (f *fakeIAMSvc) RevokeRoleByName(context.Context, uuid.UUID, string) error { return f.err }

func srv(svc *fakeIAMSvc) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Routes(r)
	return r
}

func do(t *testing.T, svc *fakeIAMSvc, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv(svc).ServeHTTP(rec, req)
	return rec
}

func TestListRoles_OK(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{roles: []string{"admin"}}, http.MethodGet, "/api/v1/users/"+id.String()+"/roles/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListRoles_InvalidUserID(t *testing.T) {
	rec := do(t, &fakeIAMSvc{}, http.MethodGet, "/api/v1/users/bad/roles/", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListRoles_Forbidden(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{err: apperrors.ErrForbidden}, http.MethodGet, "/api/v1/users/"+id.String()+"/roles/", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAssignRole_OK(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{}, http.MethodPut, "/api/v1/users/"+id.String()+"/roles/", `{"role":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestAssignRole_InvalidUserID(t *testing.T) {
	rec := do(t, &fakeIAMSvc{}, http.MethodPut, "/api/v1/users/bad/roles/", `{"role":"admin"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAssignRole_InvalidJSON(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{}, http.MethodPut, "/api/v1/users/"+id.String()+"/roles/", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAssignRole_ValidationFails(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{}, http.MethodPut, "/api/v1/users/"+id.String()+"/roles/", `{"role":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAssignRole_ServiceError(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{err: apperrors.ErrForbidden}, http.MethodPut, "/api/v1/users/"+id.String()+"/roles/", `{"role":"admin"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRevokeRole_OK(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{}, http.MethodDelete, "/api/v1/users/"+id.String()+"/roles/admin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRevokeRole_InvalidUserID(t *testing.T) {
	rec := do(t, &fakeIAMSvc{}, http.MethodDelete, "/api/v1/users/bad/roles/admin", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRevokeRole_ServiceError(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeIAMSvc{err: apperrors.ErrNotFound}, http.MethodDelete, "/api/v1/users/"+id.String()+"/roles/admin", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}
