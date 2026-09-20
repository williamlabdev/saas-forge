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
	"github.com/williamlabdev/saas-forge/internal/tenant/domain"
	"github.com/williamlabdev/saas-forge/internal/tenant/service"
)

// fakeTenantSvc implements service.TenantService entirely with canned
// return values — these tests exercise ONLY the handler's HTTP-shape
// concerns (routing, param parsing, status-code mapping); the business
// rules themselves are covered by internal/tenant/service's unit tests.
type fakeTenantSvc struct {
	inviteDTO   *service.InviteDTO
	acceptedDTO *service.AcceptedInviteDTO
	membersDTO  *service.MembersListDTO
	memberDTO   *service.MemberDTO
	invitesDTO  *service.PendingInvitesDTO
	err         error

	gotTargetUserID uuid.UUID
	gotInviteID     uuid.UUID
	gotRoleInput    service.UpdateMemberRoleInput
}

func (f *fakeTenantSvc) CreateInvite(context.Context, service.CreateInviteInput) (*service.InviteDTO, error) {
	return f.inviteDTO, f.err
}
func (f *fakeTenantSvc) AcceptInvite(context.Context, string) (*service.AcceptedInviteDTO, error) {
	return f.acceptedDTO, f.err
}
func (f *fakeTenantSvc) ListMembers(context.Context) (*service.MembersListDTO, error) {
	return f.membersDTO, f.err
}
func (f *fakeTenantSvc) UpdateMemberRole(_ context.Context, targetUserID uuid.UUID, in service.UpdateMemberRoleInput) (*service.MemberDTO, error) {
	f.gotTargetUserID = targetUserID
	f.gotRoleInput = in
	return f.memberDTO, f.err
}
func (f *fakeTenantSvc) RemoveMember(_ context.Context, targetUserID uuid.UUID) error {
	f.gotTargetUserID = targetUserID
	return f.err
}
func (f *fakeTenantSvc) ListInvites(context.Context) (*service.PendingInvitesDTO, error) {
	return f.invitesDTO, f.err
}
func (f *fakeTenantSvc) RevokeInvite(_ context.Context, inviteID uuid.UUID) error {
	f.gotInviteID = inviteID
	return f.err
}

func srv(svc service.TenantService) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Routes(r)
	return r
}

func do(t *testing.T, svc service.TenantService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv(svc).ServeHTTP(rec, req)
	return rec
}

func TestListMembers_OK(t *testing.T) {
	svc := &fakeTenantSvc{membersDTO: &service.MembersListDTO{Items: []service.MemberDTO{{UserID: "u1", Email: "a@b.com", Role: "owner"}}}}
	rec := do(t, svc, http.MethodGet, "/api/v1/tenants/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListMembers_ServiceError(t *testing.T) {
	rec := do(t, &fakeTenantSvc{err: apperrors.ErrUnauthorized}, http.MethodGet, "/api/v1/tenants/members", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateMemberRole_OK(t *testing.T) {
	targetID := uuid.New()
	svc := &fakeTenantSvc{memberDTO: &service.MemberDTO{UserID: targetID.String(), Role: "viewer"}}
	rec := do(t, svc, http.MethodPatch, "/api/v1/tenants/members/"+targetID.String(), `{"role":"viewer"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if svc.gotTargetUserID != targetID {
		t.Fatalf("target user id not parsed from path: got %s want %s", svc.gotTargetUserID, targetID)
	}
	if svc.gotRoleInput.Role != "viewer" {
		t.Fatalf("role not decoded: got %q", svc.gotRoleInput.Role)
	}
}

func TestUpdateMemberRole_InvalidUserID(t *testing.T) {
	rec := do(t, &fakeTenantSvc{}, http.MethodPatch, "/api/v1/tenants/members/not-a-uuid", `{"role":"viewer"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateMemberRole_ServiceErrorPropagatesStatus(t *testing.T) {
	targetID := uuid.New()
	// domain.ErrLastOwner is 409 — the handler must render whatever HTTP
	// status the service-layer sentinel carries, not hardcode one.
	rec := do(t, &fakeTenantSvc{err: domain.ErrLastOwner}, http.MethodPatch, "/api/v1/tenants/members/"+targetID.String(), `{"role":"admin"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateMemberRole_MissingBody(t *testing.T) {
	targetID := uuid.New()
	rec := do(t, &fakeTenantSvc{}, http.MethodPatch, "/api/v1/tenants/members/"+targetID.String(), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemoveMember_OK(t *testing.T) {
	targetID := uuid.New()
	svc := &fakeTenantSvc{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/tenants/members/"+targetID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if svc.gotTargetUserID != targetID {
		t.Fatalf("target user id not parsed from path")
	}
}

func TestRemoveMember_InvalidUserID(t *testing.T) {
	rec := do(t, &fakeTenantSvc{}, http.MethodDelete, "/api/v1/tenants/members/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRemoveMember_ServiceErrorPropagatesStatus(t *testing.T) {
	targetID := uuid.New()
	rec := do(t, &fakeTenantSvc{err: domain.ErrSelfRemove}, http.MethodDelete, "/api/v1/tenants/members/"+targetID.String(), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListInvites_OK(t *testing.T) {
	svc := &fakeTenantSvc{invitesDTO: &service.PendingInvitesDTO{Items: []service.PendingInviteDTO{{ID: "i1", Email: "x@y.com"}}}}
	rec := do(t, svc, http.MethodGet, "/api/v1/tenants/invites", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListInvites_ServiceError(t *testing.T) {
	rec := do(t, &fakeTenantSvc{err: apperrors.ErrForbidden}, http.MethodGet, "/api/v1/tenants/invites", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRevokeInvite_OK(t *testing.T) {
	inviteID := uuid.New()
	svc := &fakeTenantSvc{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/tenants/invites/"+inviteID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if svc.gotInviteID != inviteID {
		t.Fatalf("invite id not parsed from path")
	}
}

func TestRevokeInvite_InvalidID(t *testing.T) {
	rec := do(t, &fakeTenantSvc{}, http.MethodDelete, "/api/v1/tenants/invites/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRevokeInvite_NotFoundPropagatesStatus(t *testing.T) {
	inviteID := uuid.New()
	rec := do(t, &fakeTenantSvc{err: domain.ErrInviteNotFound}, http.MethodDelete, "/api/v1/tenants/invites/"+inviteID.String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
